# k8s-ai-remediator

> **[Versione italiana](README.md)**

AI agent written in Go that watches Kubernetes `Warning` events and applies controlled remediation using a local LLM (Ollama). The agent builds contextual prompts from cluster events, receives structured JSON decisions, and only executes actions from a predefined allowlist, with multiple security layers.

---

## Table of Contents

- [Architecture](#architecture)
- [Project Structure](#project-structure)
- [Prerequisites](#prerequisites)
- [Build](#build)
- [Kubernetes Installation](#kubernetes-installation)
  - [Install with the script (recommended)](#install-with-the-script-recommended)
  - [Manual install](#manual-install)
- [Configuration](#configuration)
- [Supported Remediations](#supported-remediations)
- [Email notifications](#email-notifications)
- [Observability](#observability)
- [Security](#security)
- [High Availability (Leader Election)](#high-availability-leader-election)
- [Namespace-scoped RBAC](#namespace-scoped-rbac)
- [Admin GUI](#admin-gui)
- [Test Lab](#test-lab)
- [Error Scenarios](#error-scenarios)
- [Development](#development)
- [Verification Commands](#verification-commands)
- [Troubleshooting](#troubleshooting)
- [Environment Reset](#environment-reset)

---

## Architecture

```
                    +------------------+
                    |   Kubernetes     |
                    |   API Server     |
                    +--------+---------+
                             |
                  Warning Events (poll every N sec)
                             |
                    +--------v---------+       +------------------+
                    |  ai-remediator   |<----->|  Dedup Store     |
                    |  (Go agent)      |       |  memory | Redis  |
                    +--------+---------+       +------------------+
                             |
                  Structured JSON prompt (fresh events only)
                             |
                    +--------v---------+
                    |     Ollama       |
                    |  (local LLM)     |
                    +--------+---------+
                             |
                  JSON Decision (action, confidence, params)
                             |
                    +--------v---------+       +------------------+
                    |  ai-remediator   |------>|  SMTP Notifier   |
                    |  Execution Engine|       |  (optional)      |
                    +--------+---------+       +------------------+
                             |
              Remediation (restart, delete, scale, patch, ...)
```

**Operational Flow:**

1. Kubernetes generates a `Warning` event (CrashLoopBackOff, ImagePullBackOff, etc.)
2. The agent lists events via the API and filters unprocessed Warning events
3. The dedup store (`internal/dedup`, memory or Redis) discards:
   - events already seen by `resourceVersion` (`seen:`)
   - `(ns, kind, name, reason)` signals already processed within `DEDUPE_TTL_SECONDS` (`signal:`)
4. For each fresh event it builds a prompt with namespace, kind, name, reason, message, and a snapshot of the associated Deployment
5. Sends the prompt to Ollama with a JSON schema constraining the response
6. Receives a structured decision with action, confidence, and parameters
7. Validates the decision: allowlist, policy bounds, OCI image format, confidence threshold
8. Executes the action (or logs it in dry-run mode) and, if configured, sends an email over SMTP (`internal/notify`) with the decision summary + outcome

When an event concerns a Pod, the agent traces back to the Deployment via `ownerReferences` (Pod -> ReplicaSet -> Deployment).

The dedup store is pluggable: `redis` by default (the deployment default, installed by `scripts/install.sh`, survives pod restarts), with `memory` (in-process, wiped on restart) as a fallback (see [Dedup Backend](#dedup-backend)). On a Redis outage the store fails open: the agent behaves as if dedup were in-memory and never blocks remediation.

---

## Project Structure

```
k8s-ai-remediator/
├── cmd/
│   └── agent/
│       ├── main.go              # Bootstrap, signal handling, leader election, event loop
│       └── main_test.go         # Integration tests for executeDecision
├── internal/
│   ├── model/
│   │   └── model.go            # Shared types: Action, Decision, Severity, Ollama API types
│   ├── config/
│   │   ├── config.go           # AgentConfig, environment variable parsing (DEDUP_* and NOTIFY_* included)
│   │   └── config_test.go
│   ├── ollama/
│   │   ├── client.go           # HTTP client with rate limiting, retry, TLS
│   │   └── client_test.go
│   ├── kube/
│   │   ├── kube.go             # Kubernetes operations (resolve, remediate, logs, snapshot)
│   │   └── kube_test.go
│   ├── policy/
│   │   ├── policy.go           # Allowlist, OCI validation, prompt sanitization
│   │   └── policy_test.go
│   ├── dedup/
│   │   ├── dedup.go            # Store interface + MemoryStore (in-process) implementation
│   │   ├── redis.go            # RedisStore (native TTL, fail-open on errors)
│   │   ├── factory.go          # NewStore(BackendConfig) -> memory|redis
│   │   └── *_test.go           # miniredis tests for atomicity, TTL, key-prefix isolation
│   ├── notify/
│   │   ├── notify.go           # SMTP notifier (STARTTLS, fire-and-forget with concurrency cap)
│   │   └── notify_test.go
│   ├── metrics/
│   │   ├── metrics.go          # Prometheus-compatible metrics (zero external deps)
│   │   └── metrics_test.go
│   └── webui/                   # Optional admin GUI (handlers, auth, embedded templates+assets)
├── deploy/
│   ├── agent.yaml              # End-to-end manifest: Namespace+ConfigMap+Secret+Deployment+Service (+Ingress)
│   ├── rbac-cluster.yaml       # Cluster-wide RBAC (default): SA+ClusterRole+lease — events across the cluster
│   ├── rbac-namespaced.yaml    # Namespace-scoped RBAC (advanced, see the caveat in the file)
│   ├── rbac-webui.yaml         # Extra RBAC for the GUI (configmap/secret/deploy + namespaces/roles)
│   ├── rbac-scenarios.yaml     # Optional RBAC for the "Scenarios" feature in the sandbox namespace
│   └── redis.yaml              # Single-instance Redis for dedup (Deployment+Service+PVC+NetworkPolicy)
├── scenarios/                   # Test manifests: low/medium/critical/severe
├── scripts/
│   ├── install.sh              # Idempotent installer: Ollama + RBAC + agent + GUI, in the right order
│   └── mirror-images.sh        # Batch mirror of redis/busybox/polinux to the local registry
├── .github/
│   └── workflows/
│       └── ci.yml              # CI/CD: lint, test, build, Docker, security scan
├── .golangci.yml                # golangci-lint v2 config (standard set + idiomatic exclusions)
├── .dockerignore                # Shrinks the build context (no .git/.idea/docs/deploy)
├── .gitignore                   # Build/test artifacts excluded from version control
├── Dockerfile                   # Multi-stage build (distroless, non-root)
├── go.mod
└── go.sum
```

### Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/model` | Shared types across all packages: Action constants, Decision struct, Severity, Ollama API types |
| `internal/config` | `AgentConfig` with all parameters and helpers for env var parsing with defaults (includes `DEDUP_BACKEND`, `REDIS_*`, `NOTIFY_*`) |
| `internal/ollama` | HTTP client for Ollama with rate limiting (`golang.org/x/time/rate`), retry with exponential backoff, TLS support |
| `internal/kube` | All Kubernetes operations: Pod->Deployment resolution, restart, delete, scale, set image, log inspection, snapshot |
| `internal/policy` | Action allowlist, OCI image validation, unsafe image update blocking, anti-injection prompt sanitization |
| `internal/dedup` | Pluggable dedup store: `MemoryStore` (map+mutex, on-demand eviction) and `RedisStore` (native `SetNX`+TTL, fail-open on errors). Factory `NewStore(BackendConfig)` |
| `internal/notify` | Fire-and-forget SMTP notifier (PLAIN over STARTTLS). Returns a no-op if `HOST`/`USER`/`TO` are empty; concurrency cap prevents goroutine leaks during event storms |
| `internal/metrics` | Metrics in Prometheus text exposition format, zero external dependencies |
| `internal/webui` | Optional admin GUI (HMAC login, dashboard, SSE logs, cluster, configuration, scenarios, RBAC). HTML templates, CSS/JS and scenarios are embedded into the binary via `go:embed` |

---

## Prerequisites

- A running Kubernetes cluster (Docker Desktop, minikube, kind, k3s, EKS, GKE, AKS, ...)
- `kubectl` configured for the correct cluster
- Docker for building the image
- Go 1.25.11+ for local development (optional, the build happens in Docker)

---

## Build

### Build and push to the local registry

The image is always published to a local registry reachable via
`host.docker.internal:5050` (the `registry` container maps host port 5050
to the internal port 5000). Using `host.docker.internal` instead of
`localhost` lets the same tag work both for `docker push` from the host and
for kubelet pulls from inside the cluster, avoiding `ErrImageNeverPull`
on non-Docker runtimes (containerd, CRI-O) or multi-node setups.

```bash
# Start the local registry (once)
docker run -d --restart=always -p 5050:5000 --name registry registry:2

# Build and push (same tag used by deploy/agent.yaml and install.sh --build)
REGISTRY=host.docker.internal:5050
IMAGE=$REGISTRY/k8s-ai-remediator:latest
docker build -t "$IMAGE" .
docker push "$IMAGE"
```

> Alternatively, `scripts/install.sh --build` runs build, push and deploy in
> one shot (see [Kubernetes Installation](#kubernetes-installation)).

The Dockerfile uses a multi-stage build:
- **Stage 1**: Go 1.25.11 compiles a static binary (`CGO_ENABLED=0`, `-trimpath -ldflags="-s -w"`). Dependencies are downloaded in a separate layer (`go mod download`) so the cache holds until `go.mod`/`go.sum` change
- **Stage 2**: `gcr.io/distroless/static:nonroot` as the base image (no shell, non-root user)

> **Linux note**: `host.docker.internal` is available by default with
> Docker Desktop on macOS/Windows and recent Linux versions. If it does not
> resolve, add `--add-host=host.docker.internal:host-gateway` to the docker
> command, or use the Docker bridge gateway IP.
> On kind you can alternatively connect the registry to the kind network
> (`docker network connect kind registry`) and use `kind-registry:5000`.
> On minikube: `minikube addons enable registry` or `host.minikube.internal:5050`.

### Local Build (optional)

```bash
go mod tidy
CGO_ENABLED=0 go build -o agent ./cmd/agent
```

### Mirroring upstream images to the local registry

On local clusters without egress (Docker Desktop, kind, minikube) the
kubelet cannot pull images from Docker Hub and pods end up in
`ImagePullBackOff`. Besides `ai-remediator`, the repo manifests also use
`redis:7.2-alpine` (dedup backend) and `busybox:1.36` /
`polinux/stress:latest` (lab scenarios). Use the helper script to
pre-load them into the local registry:

```bash
# Pull+tag+push to host.docker.internal:5050 (default)
scripts/mirror-images.sh

# Different registry
REGISTRY=kind-registry:5000 scripts/mirror-images.sh

# Only a subset
scripts/mirror-images.sh redis:7.2-alpine busybox:1.36
```

After mirroring, rewrite the workload image to point at the local
registry, e.g.:

```bash
kubectl -n ai-remediator set image deploy/ai-remediator-redis \
  redis=host.docker.internal:5050/redis:7.2-alpine
```

---

## Kubernetes Installation

The recommended path is the **`scripts/install.sh`** script: it sets
everything up in the right order (Ollama → Redis → RBAC → agent → GUI), is
idempotent (safe to re-run) and verifies the rollout. A manifest-based manual
equivalent follows.

### Install with the script (recommended)

```bash
# Full local setup: build image + Ollama + model + agent + GUI
scripts/install.sh --build

# Cluster that already has Ollama and an image pushed elsewhere
scripts/install.sh --skip-ollama --image <registry>/k8s-ai-remediator:<tag>

# Headless agent, no admin GUI
scripts/install.sh --no-webui

# Preview the actions without touching the cluster / tear everything down
scripts/install.sh --dry-run
scripts/install.sh --uninstall
```

**What it does, in order:**

1. **Preflight** — checks `kubectl` and cluster reachability
2. **`--build`** (optional) — `docker build` + `docker push` of the image
3. **Ollama** (default, skip with `--skip-ollama`) — Deployment + Service in the `ollama` namespace and `ollama pull` of the model
4. **Redis** (mandatory dedup backend) — creates the `ai-remediator-redis` Secret (random password if none given), applies `deploy/redis.yaml` and waits for its rollout. With `--redis-addr host:port` it skips the bundled Redis and points at an external one
5. **Cluster-wide RBAC** — `deploy/rbac-cluster.yaml` (ServiceAccount + ClusterRole + lease Role). Required because the agent lists events from **all** namespaces in one call: a namespaced Role is not enough
6. **Agent** — `deploy/agent.yaml` (Namespace, ConfigMap, Secret, Deployment, Service); with the GUI on it also applies `deploy/rbac-webui.yaml` and `deploy/rbac-scenarios.yaml`
7. **Overrides** image, model, `DEDUP_BACKEND`/`REDIS_ADDR` and GUI credentials, then `rollout` and verify (pods + logs)

**Main flags** (sensible defaults; `scripts/install.sh --help` for the full list):

| Flag | Default | Effect |
|------|---------|--------|
| `--build` | off | build & push the image before deploying (needs Docker) |
| `--image` / `--registry` | `host.docker.internal:5050/k8s-ai-remediator:latest` | image to deploy |
| `--model` | `qwen2.5:7b` | Ollama model to pull and use |
| `--skip-ollama` | off | do not install Ollama (assume it is already present) |
| `--no-webui` | off | install the agent without the GUI (no GUI RBAC) |
| `--webui-user` / `--webui-password` | `admin` / generated | GUI login credentials |
| `--sandbox-ns` | `incident-lab` | namespace allowed to receive fault scenarios |
| `--redis-addr` | *(empty)* | use an external Redis (`host:port`) and skip the bundled one |
| `--redis-password` | generated | Redis password (bundled or external); random if unset |
| `--dry-run` / `--uninstall` | — | preview without changes / remove created namespaces and RBAC |

> If you do not pass `--webui-password`, the script generates a random one and
> prints it at the end: **save it** (shown only once). In production front the
> GUI with a TLS Ingress.

### Manual install

Same result as the script, applying the manifests by hand.

#### 1. Ollama

> The script installs Ollama automatically; this block is only for a manual
> or standalone Ollama setup.

```bash
# Create the namespace
kubectl create namespace ollama

# Create the deployment
kubectl -n ollama create deployment ollama \
  --image=ollama/ollama:latest \
  --port=11434 \
  --replicas=1

# Configure host, resources, and storage
kubectl -n ollama patch deployment ollama --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/env","value":[
    {"name":"OLLAMA_HOST","value":"0.0.0.0:11434"}
  ]},
  {"op":"add","path":"/spec/template/spec/containers/0/resources","value":{
    "requests":{"cpu":"1","memory":"4Gi"},
    "limits":{"cpu":"8","memory":"16Gi"}
  }},
  {"op":"add","path":"/spec/template/spec/volumes","value":[
    {"name":"ollama-data","emptyDir":{}}
  ]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[
    {"name":"ollama-data","mountPath":"/root/.ollama"}
  ]}
]'

# Expose the service
kubectl -n ollama expose deployment ollama \
  --name=ollama \
  --port=11434 \
  --target-port=11434 \
  --type=ClusterIP

# Wait for rollout and install the model
kubectl -n ollama rollout status deployment/ollama --timeout=180s
# qwen2.5:7b is the recommended default on CPU (30-90s per call).
# Use qwen2.5:14b if you have a GPU or want top quality (3-6 min on CPU).
kubectl -n ollama exec -it deploy/ollama -- ollama pull qwen2.5:7b
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

> **Note**: The `OLLAMA_MODEL` value in the ConfigMap must exactly match the name shown by `ollama list`.
>
> **Model choice and RAM requirements**:
> - `qwen2.5:7b` (recommended on CPU): ~4 GB free RAM, calls ~30-90s. Quality is sufficient because the prompt is structured as a decision tree with few-shot examples.
> - `qwen2.5:14b`: ~8 GB free RAM, calls ~3-6 min on CPU. Higher quality; suitable if you have GPU or extended timeouts.
>
> If the pod stays in `Pending`, verify the node has enough resources with `kubectl describe node`. For local clusters:
> - **Docker Desktop**: Settings → Resources → allocate at least **6 GB RAM / 4 CPUs** (14b: 10 GB), then Apply & Restart
> - **minikube**: `minikube start --memory=6144 --cpus=4` (14b: `--memory=10240`)
> - **kind**: configure the resources of the underlying Docker container

#### Changing the model after installation

The default (code and `deploy/agent.yaml`) is `qwen2.5:7b`. If the agent uses
the wrong model (e.g. `qwen2.5:14b`), it is because the ConfigMap sets it
explicitly: update it and roll out the Deployment, or use the GUI
**Configuration → LLM (Ollama)** page (it writes the ConfigMap and rolls out).

```bash
# Set the model and reload the agent
kubectl -n ai-remediator patch configmap ai-remediator-config --type merge \
  -p '{"data":{"OLLAMA_MODEL":"qwen2.5:7b"}}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent

# Make sure the model is pulled in Ollama (the name must match)
kubectl -n ollama exec deploy/ollama -- ollama pull qwen2.5:7b
kubectl -n ollama exec deploy/ollama -- ollama list
```

#### 2. Agent (manifests)

The manifests in `deploy/` are the source of truth: apply them in the order
namespace → RBAC → Redis → agent.

```bash
# Namespace
kubectl create namespace ai-remediator

# Cluster-wide RBAC: cluster-scoped event/pod reads + remediation + leases
kubectl apply -f deploy/rbac-cluster.yaml

# (optional) GUI and fault-scenario RBAC
kubectl apply -f deploy/rbac-webui.yaml
kubectl apply -f deploy/rbac-scenarios.yaml      # sandbox: incident-lab namespace

# Redis (mandatory dedup backend): password Secret + Deployment + Service + PVC
kubectl -n ai-remediator create secret generic ai-remediator-redis \
  --from-literal=password="$(openssl rand -base64 32)"
kubectl apply -f deploy/redis.yaml
kubectl -n ai-remediator rollout status deployment/ai-remediator-redis --timeout=60s

# Agent: Namespace + ConfigMap + Secret + Deployment + Service
# (agent.yaml already ships DEDUP_BACKEND=redis, REDIS_ADDR and REDIS_PASSWORD wired)
kubectl apply -f deploy/agent.yaml

# Customise the image and the GUI password (the manifest ships a placeholder)
kubectl -n ai-remediator set image deployment/ai-remediator-agent \
  agent=host.docker.internal:5050/k8s-ai-remediator:latest
kubectl -n ai-remediator patch secret ai-remediator-secrets --type merge \
  -p '{"stringData":{"WEBUI_PASSWORD":"<pick-a-password>"}}'

# Roll out and verify
kubectl -n ai-remediator rollout status deployment/ai-remediator-agent --timeout=180s
kubectl -n ai-remediator logs deploy/ai-remediator-agent --tail=20
```

> **Note**: `host.docker.internal:5050` assumes the kubelet can resolve
> `host.docker.internal` to the host gateway (default on Docker Desktop).
> On kind/minikube adapt the hostname as described in the [Build](#build)
> section.
>
> To restrict the agent to specific namespaces use `INCLUDE_NAMESPACES`
> (application-level filter). `deploy/rbac-namespaced.yaml` alone is **not
> enough**: event listing is cluster-wide and still needs the read granted by
> `rbac-cluster.yaml`. See [Namespace-scoped RBAC](#namespace-scoped-rbac).

---

## Configuration

All variables are read from environment variables (typically via ConfigMap).

### Core Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OLLAMA_BASE_URL` | `http://ollama.ollama.svc.cluster.local:11434/api` | Ollama API base URL |
| `OLLAMA_MODEL` | `qwen2.5:7b` | LLM model name (must match `ollama list`). `7b` (default, recommended on CPU): ~30-90s per call. For GPU or top quality set `qwen2.5:14b`: ~3-6 min on CPU |
| `DRY_RUN` | `false` | If `true`, logs decisions without applying remediation |
| `POLL_INTERVAL_SECONDS` | `30` | Event polling interval (seconds) |
| `DEDUPE_TTL_SECONDS` | `300` | Dedup TTL for `(ns, kind, name, reason)` signals: identical events within the window do not trigger a new LLM call |
| `MAX_EVENTS_PER_POLL` | `10` | Maximum number of events that trigger an LLM call per poll cycle; excess events are deferred to the next poll |
| `INCLUDE_NAMESPACES` | *(empty)* | Comma-separated allowlist of namespaces. When set the agent only reacts to events from those namespaces; it also populates the GUI **Cluster** page selector. Empty = all namespaces minus the excluded ones. Settable from the GUI: **Configuration → Namespace filters → Include namespaces** |
| `EXCLUDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease,local-path-storage` | Comma-separated denylist of system namespaces. Events here are never sent to the LLM. Always wins over the allowlist (a namespace in both is excluded) |

### Policy Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SCALE_MIN` | `1` | Minimum allowed replicas for `scale_deployment` |
| `SCALE_MAX` | `5` | Maximum allowed replicas |
| `ALLOW_IMAGE_UPDATES` | `false` | Enables the `set_deployment_image` action |
| `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` | `0.92` | Minimum confidence to update an image |
| `ALLOW_PATCH_PROBE` | `false` | Enables the `patch_probe` action (also requires the `ai-remediator/allow-patch` annotation with scope `probe`) |
| `ALLOW_PATCH_RESOURCES` | `false` | Enables the `patch_resources` action (scope `resources`) |
| `ALLOW_PATCH_REGISTRY` | `false` | Enables the `patch_registry` action (scope `registry`) |
| `PATCH_CONFIDENCE_THRESHOLD` | `0.85` | Minimum confidence for any `patch_*` action |

### Ollama Variables (Resilience)

| Variable | Default | Description |
|----------|---------|-------------|
| `OLLAMA_RPS` | `2.0` | Max requests per second to Ollama (rate limiting) |
| `OLLAMA_MAX_RETRIES` | `3` | Retry attempts for transient errors (5xx, network) with exponential backoff |
| `OLLAMA_TLS_SKIP_VERIFY` | `false` | Skip TLS verification (for self-signed certificates) |
| `OLLAMA_HTTP_TIMEOUT_SECONDS` | `180` | HTTP timeout per request to Ollama (awaiting headers + body). Increase if you see `Client.Timeout exceeded while awaiting headers` with slow models (CPU, unloaded GPU, cold start) |
| `POLL_CONTEXT_TIMEOUT_SECONDS` | `300` | Context timeout wrapping the entire poll cycle (event list + Ollama calls). Must stay larger than `OLLAMA_HTTP_TIMEOUT_SECONDS`, otherwise the context expires before the HTTP client and produces `context deadline exceeded` |
| `POD_LOG_TAIL_LINES` | `200` | Number of log lines read per container |

### Observability Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `METRICS_ADDR` | `:9090` | Listen address for `/metrics` and `/healthz` |

### High Availability Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `LEADER_ELECTION` | `false` | Enables leader election for multi-replica deployments |
| `LEASE_NAME` | `ai-remediator-leader` | Lease resource name |
| `LEASE_NAMESPACE` | `ai-remediator` | Lease resource namespace |

### Dedup Backend

**Redis is the default dedup backend**: `deploy/agent.yaml` sets `DEDUP_BACKEND=redis` and `scripts/install.sh` installs and wires up Redis automatically. Redis persists dedup state (events already seen + recent signals) across pod restarts and leader failovers, so the agent does not remediate the same incident twice right after a restart. The `memory` backend (in-process) remains as a fallback: simple and zero-ops, but **lost on pod restart**.

| Variable | Default | Description |
|----------|---------|-------------|
| `DEDUP_BACKEND` | `redis` ¹ | Dedup store backend. Values: `redis` (deployment default, shared, survives restarts), `memory` (in-process, fallback) |
| `REDIS_ADDR` | `ai-remediator-redis:6379` ¹ | Redis `host:port`. Required when `DEDUP_BACKEND=redis` |
| `REDIS_PASSWORD` | *(empty)* | Redis password (injected from the `ai-remediator-redis` Secret). Leave empty for unauthenticated Redis |
| `REDIS_DB` | `0` | Redis logical DB number |
| `REDIS_KEY_PREFIX` | `k8s-remediator:` | Prefix applied to every key the agent writes. Useful when sharing Redis with other services |

> ¹ This is the default at every level: code (`internal/config`), `deploy/agent.yaml` and `scripts/install.sh`. For local out-of-cluster runs without Redis, the agent automatically falls back to `memory` at startup (warning in the logs, never blocks remediation).

**Behaviour during a Redis outage**: the store **fails open** (`MarkSeen` returns `fresh=true`, `IsSignalFresh` returns `false`). In practice: if Redis is down the agent behaves as if dedup were in-memory (possible duplicates), but **never blocks remediation**. If the initial connection fails at startup the agent automatically falls back to `memory` and logs a warning.

#### Quick-start: Redis backend

`scripts/install.sh` runs all of the steps below automatically (Redis is
mandatory): it creates the password Secret, applies `deploy/redis.yaml`, waits
for the rollout and wires `DEDUP_BACKEND`/`REDIS_ADDR`/`REDIS_PASSWORD` onto the
agent. Use `--redis-addr host:port` to point at an external Redis and skip the
bundled one. The commands below are for a manual install or to understand what
the script does.

The `deploy/redis.yaml` manifest installs a single-instance Redis (Deployment + Service + PVC + NetworkPolicy, `redis:7.2-alpine`, AOF enabled, non-root, read-only rootfs):

```bash
# 1. (optional, recommended) Secret with the password
kubectl -n ai-remediator create secret generic ai-remediator-redis \
  --from-literal=password="$(openssl rand -base64 32)"

# 2. Apply the manifest
kubectl apply -f deploy/redis.yaml

# 3. Wait until Redis is ready
kubectl -n ai-remediator rollout status deployment/ai-remediator-redis --timeout=60s

# 4. Add the env vars to the agent ConfigMap and reload the deployment
kubectl -n ai-remediator patch configmap ai-remediator-config --type=merge -p '{
  "data": {
    "DEDUP_BACKEND": "redis",
    "REDIS_ADDR": "ai-remediator-redis:6379",
    "REDIS_KEY_PREFIX": "k8s-remediator:"
  }
}'

# 5. Inject REDIS_PASSWORD from the Secret (if created at step 1)
kubectl -n ai-remediator patch deployment ai-remediator-agent --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/env","value":[
    {"name":"REDIS_PASSWORD","valueFrom":{"secretKeyRef":{"name":"ai-remediator-redis","key":"password"}}}
  ]}
]'

# 6. Restart and verify
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
kubectl -n ai-remediator logs deploy/ai-remediator-agent --tail=5 | grep 'dedup store'
# expected: "dedup store initialised" backend=redis
```

> **NetworkPolicy note**: the manifest accepts ingress from pods labeled
> `app=ai-remediator-agent` (the real label set by `agent.yaml`), plus
> `app=ai-remediator` and `app.kubernetes.io/name=ai-remediator` for manual/
> `kubectl create` deployments. If your agent uses different labels or the
> cluster CNI does not enforce NetworkPolicies (Docker Desktop does not by
> default), delete `networkpolicy/ai-remediator-redis` to avoid surprises.

**Verify on the Redis side**:

```bash
kubectl -n ai-remediator exec deploy/ai-remediator-redis -- \
  redis-cli -a "$REDIS_PASSWORD" --no-auth-warning KEYS 'k8s-remediator:*'
# seen:<ns>/<name>/<resourceVersion>   (TTL = EVENT_SEEN_TTL_SECONDS)
# signal:<ns>|<kind>|<name>|<reason>   (TTL = DEDUPE_TTL_SECONDS)
```

**Falling back to `memory` (not recommended)**: set `DEDUP_BACKEND=memory` in
the ConfigMap. Acceptable only if the agent rarely restarts and you are fine
with a handful of signals being re-evaluated afterwards, or if you run with
`DRY_RUN=true` purely for observation.

**Why Redis is the default**:
- Production with auto-remediation policies enabled (`ALLOW_PATCH_*`), where a duplicate `patch_resources` or `restart_deployment` right after a restart may amplify an incident instead of resolving it.
- You plan to scale the agent beyond a single replica (also requires loop-level changes, outside the scope of this step).

---

## Supported Remediations

| Action | Type | Description |
|--------|------|-------------|
| `noop` | Passive | No action, the decision is only logged |
| `ask_human` | Passive | Signals that manual intervention is needed |
| `mark_for_manual_fix` | Passive | Marks the resource as not automatically resolvable |
| `inspect_pod_logs` | Read-only | Reads current and previous logs of the container with the most restarts |
| `restart_deployment` | Mutation | Forces a rollout by updating the pod template annotation |
| `delete_failed_pod` | Mutation | Deletes the pod; the controller recreates it |
| `delete_and_recreate_pod` | Mutation | Same as above, used when the pod needs to be recreated from scratch |
| `scale_deployment` | Mutation | Updates `spec.replicas` within `SCALE_MIN`/`SCALE_MAX` bounds |
| `set_deployment_image` | Mutation | Updates the container image (requires `ALLOW_IMAGE_UPDATES=true`, confidence above threshold, valid OCI image) |
| `patch_probe` | Mutation (patch) | Tunes readiness/liveness probe timing fields (`initialDelaySeconds`, `periodSeconds`, `failureThreshold`, `successThreshold`, `timeoutSeconds`). Never the probe handler |
| `patch_resources` | Mutation (patch) | Updates CPU/memory `requests`/`limits` of a container within fixed bounds |
| `patch_registry` | Mutation (patch) | Rewrites only the registry prefix of the image, preserving path and tag |

### Controlled Deployment patches (new)

The three `patch_*` actions let the agent fix common misconfigurations
(overly strict probe, undersized requests/limits, wrong registry). They
are disabled by default and require a double opt-in:

1. **Global feature flag** via env var (`ALLOW_PATCH_PROBE`, `ALLOW_PATCH_RESOURCES`, `ALLOW_PATCH_REGISTRY`).
2. **Per-Deployment opt-in** via the annotation `ai-remediator/allow-patch: "probe,resources,registry"` (or `"*"` for all scopes).

In addition, every `patch_*` is blocked if the LLM confidence is below
`PATCH_CONFIDENCE_THRESHOLD` (default `0.85`).

**Step 1 — enable the global feature flags** (flips them from `false` to
`true` in the ConfigMap, then rolls out the Deployment):

```bash
kubectl -n ai-remediator patch configmap ai-remediator-config --type merge -p '{
  "data": {
    "ALLOW_PATCH_PROBE": "true",
    "ALLOW_PATCH_RESOURCES": "true",
    "ALLOW_PATCH_REGISTRY": "true"
  }
}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
```

> You can also toggle them from the GUI **Configuration** page ("action
> policies" section), which writes the ConfigMap and rolls out.
> The flags alone are not enough: every target Deployment must **also** carry
> the `ai-remediator/allow-patch` annotation (step 2).

**Step 2 — opt-in on the target Deployment:**

```bash
kubectl -n myns annotate deployment myapp \
  ai-remediator/allow-patch="probe,resources"
```

Parameters expected from the LLM for each action:

- `patch_probe`: `deployment_name`, `container`, `probe` (`readiness`|`liveness`), at least one of `initial_delay_seconds`, `period_seconds`, `failure_threshold`, `success_threshold`, `timeout_seconds`.
- `patch_resources`: `deployment_name`, `container`, at least one of `cpu_request`, `memory_request`, `cpu_limit`, `memory_limit` (Kubernetes quantities).
- `patch_registry`: `deployment_name`, `container`, `new_registry` (e.g. `host.docker.internal:5050`).

Probe numeric fields are validated against fixed bounds (e.g.
`period_seconds` in `[1, 300]`), resource quantities against `[10m, 8]`
CPU and `[16Mi, 16Gi]` memory, and image references are rebuilt
preserving path+tag.

### Container Selection Logic for Logs

In multi-container pods, `inspect_pod_logs` selects the container:
1. Uses the container specified in parameters, if present and valid
2. Otherwise selects the container with the highest restart count
3. Falls back to the first container in the spec

---

## Email notifications

The agent can send a short email over SMTP after each `executeDecision`,
summarising "anomaly detected -> decision taken -> post-intervention
status". Good for small test clusters; not meant for production-scale
volume (one email per decision).

Three supported patterns, all via the same code:

1. **iCloud self-notification**: your Apple ID is both sender and recipient.
   Nothing else to create.
2. **Dedicated Gmail bot**: a separate address for the agent, alerts routed
   to an ops mailbox.
3. **Transactional provider** (Resend / Mailgun / Postmark): only host and
   credentials change.

### Setup with iCloud Mail (patterns 1 or 2)

Generate an **app-specific password** for your Apple ID
(<https://account.apple.com> → Sign-In & Security → App-specific
passwords). **Important**: never paste this password into chat or into a
repository; apply it only as a cluster Secret.

Placeholders below: replace `alerts-bot@icloud.com` with your actual Apple
ID and `ops-alerts@example.com` with the mailbox where you want the
alerts (it can be the same address for self-notification).

```bash
# Create the Secret with credentials (freshly generated app-specific password)
kubectl -n ai-remediator create secret generic ai-remediator-notify \
  --from-literal=NOTIFY_SMTP_USER='alerts-bot@icloud.com' \
  --from-literal=NOTIFY_SMTP_PASSWORD='xxxx-xxxx-xxxx-xxxx'

# Only host/port/recipients in the ConfigMap (non-secret)
kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "NOTIFY_SMTP_HOST":"smtp.mail.me.com",
    "NOTIFY_SMTP_PORT":"587",
    "NOTIFY_FROM":"alerts-bot@icloud.com",
    "NOTIFY_TO":"ops-alerts@example.com",
    "NOTIFY_MIN_SEVERITY":"medium"
  }}'

# Mount the Secret as envFrom (in addition to the existing ConfigMap)
kubectl -n ai-remediator patch deployment ai-remediator-agent --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom/-","value":
    {"secretRef":{"name":"ai-remediator-notify"}}}
]'

kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
```

### Setup with Gmail (alternative)

Create a dedicated account (e.g. `ai-remediator-bot@gmail.com`), enable 2FA,
generate an App Password (<https://myaccount.google.com/apppasswords>), and
only change host and username:

```bash
kubectl -n ai-remediator create secret generic ai-remediator-notify \
  --from-literal=NOTIFY_SMTP_USER='ai-remediator-bot@gmail.com' \
  --from-literal=NOTIFY_SMTP_PASSWORD='xxxxxxxxxxxxxxxx'

kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "NOTIFY_SMTP_HOST":"smtp.gmail.com",
    "NOTIFY_SMTP_PORT":"587",
    "NOTIFY_FROM":"ai-remediator-bot@gmail.com",
    "NOTIFY_TO":"ops-alerts@example.com",
    "NOTIFY_MIN_SEVERITY":"medium"
  }}'
```

The Deployment patch and rollout restart are identical to the iCloud case.

### Configuration variables

| Variable | Default | Description |
|----------|---------|-------------|
| `NOTIFY_SMTP_HOST` | *(empty)* | SMTP host (e.g. `smtp.mail.me.com`, `smtp.gmail.com`). Empty → notifier disabled (no-op) |
| `NOTIFY_SMTP_PORT` | `587` | SMTP port with STARTTLS |
| `NOTIFY_SMTP_USER` | *(empty)* | SMTP username (usually the full email). Empty → notifier disabled |
| `NOTIFY_SMTP_PASSWORD` | *(empty)* | App-specific password |
| `NOTIFY_FROM` | = `NOTIFY_SMTP_USER` | Sender visible in the email |
| `NOTIFY_TO` | *(empty)* | Recipient. Empty → notifier disabled. May equal `NOTIFY_FROM` (self-notification) |
| `NOTIFY_MIN_SEVERITY` | `medium` | Only send email for decisions at or above this severity. Values: `critical`, `high`, `medium`, `low`, `info` |

If any of `HOST`, `USER`, `TO` is empty the notifier does not even attempt
to connect: the startup log prints `notify: SMTP not configured,
notifications disabled`.

### Tuning verbosity

| Use case | `NOTIFY_MIN_SEVERITY` | Typical frequency |
|----------|----------------------|-------------------|
| Emergencies only (prod) | `critical` | Rare |
| Normal production | `high` | A few a day on a healthy cluster |
| Test/demo (default) | `medium` | One per active scenario |
| Verbose debug | `low` | Up to one every 5 min per `(deployment, reason)` — dedup TTL throttles |
| Everything | `info` | Includes LLM `noop` decisions |

At any `NOTIFY_MIN_SEVERITY`, signal dedup (TTL `DEDUPE_TTL_SECONDS`) keeps
traffic under control: you never receive more than one email per
`(ns, Deployment, reason)` within the window.

### Setup verification

```bash
# After the rollout restart, the line must report every field populated
kubectl -n ai-remediator logs deploy/ai-remediator-agent --tail=30 \
  | grep 'notify: SMTP configured'
```

Expected output:
```
notify: SMTP configured host=smtp.mail.me.com port=587 from=alerts-bot@icloud.com to=ops-alerts@example.com minSeverity=medium
```

If you see `notify: SMTP not configured, notifications disabled` instead,
the Secret was not mounted: check the Deployment `envFrom` with
`kubectl -n ai-remediator get deployment ai-remediator-agent -o jsonpath='{.spec.template.spec.containers[0].envFrom}'`.

### Email body

Plain text with three blocks:

```
Situazione anomala
  Namespace: incident-lab
  Kind: Pod
  Name: memory-hog-xxxx
  Reason: BackOff
  Message: ...
  Severity: high

Decisione presa
  Action: patch_resources
  Probable cause: ...
  Summary: ...
  Confidence: 1.00
  Parameters:
    container: app
    memory_limit: 512Mi
    memory_request: 256Mi

Situazione post intervento
  Azione applicata con successo.
```

If `executeDecision` fails (guard block or Kubernetes error), the third
block reads `Azione non applicata. Errore: <text>`. Sending is
fire-and-forget with a 30s timeout and never blocks the poll loop.

---

## Observability

### HTTP Endpoints

The agent exposes two HTTP endpoints on the port configured in `METRICS_ADDR` (default `:9090`):

| Endpoint | Description |
|----------|-------------|
| `/metrics` | Metrics in Prometheus text exposition format |
| `/healthz` | Health check (returns `200 OK`) |

### Exposed Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `remediator_events_processed_total` | Counter | Total Warning events processed |
| `remediator_events_skipped_total` | Counter | Events skipped (dedup or non-Warning) |
| `remediator_decisions_total{action}` | Counter | Decisions by action type |
| `remediator_decision_errors_total` | Counter | Errors calling Ollama |
| `remediator_execution_errors_total` | Counter | Errors executing remediation |
| `remediator_ollama_requests_total` | Counter | Total requests to Ollama |
| `remediator_ollama_errors_total` | Counter | Ollama errors |
| `remediator_ollama_avg_latency_seconds` | Gauge | Average Ollama request latency |
| `remediator_ollama_rate_limited_total` | Counter | Requests delayed by rate limiter |

### Structured Logging

The agent produces JSON-formatted logs (via `log/slog`) on stdout, compatible with Kubernetes logging stacks (Loki, Fluentd, CloudWatch, etc.):

```json
{"time":"2026-03-26T10:00:00Z","level":"INFO","msg":"decision","summary":"Pod crash","action":"restart_deployment","ns":"default","kind":"Deployment","name":"web","confidence":0.85}
```

### Configuring the Service for Prometheus Scraping

```bash
kubectl -n ai-remediator expose deployment ai-remediator-agent \
  --name=ai-remediator-metrics \
  --port=9090 \
  --target-port=9090 \
  --type=ClusterIP

# If using Prometheus Operator, add a ServiceMonitor:
# apiVersion: monitoring.coreos.com/v1
# kind: ServiceMonitor
# metadata:
#   name: ai-remediator
#   namespace: ai-remediator
# spec:
#   selector:
#     matchLabels:
#       app: ai-remediator
#   endpoints:
#   - port: metrics
#     interval: 30s
```

---

## Security

### Protection Layers

1. **Action allowlist**: only the 12 predefined actions are accepted; any other is rejected
2. **Dry-run mode**: with `DRY_RUN=true` no changes are applied to the cluster
3. **Policy bounds**: `scale_deployment` constrained to `SCALE_MIN`/`SCALE_MAX`; `patch_resources` to bounds `[10m, 8]` CPU and `[16Mi, 16Gi]` memory; `patch_probe` to per-field bounds (`failureThreshold` 1-20, `periodSeconds` 1-300, ...)
4. **Confidence threshold**: `set_deployment_image` blocked below `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` (default 0.92); the three `patch_*` actions below `PATCH_CONFIDENCE_THRESHOLD` (default 0.85)
5. **Per-action feature flag**: `ALLOW_IMAGE_UPDATES`, `ALLOW_PATCH_PROBE`, `ALLOW_PATCH_RESOURCES`, `ALLOW_PATCH_REGISTRY` — all default `false`
6. **Per-Deployment opt-in** for `patch_*` actions: requires the annotation `ai-remediator/allow-patch` with scopes (`probe`, `resources`, `registry`, `*`)
7. **Counter-productive remediation guards** (block LLM decisions that would not fix the root cause and would waste cycles):
   - `restart_deployment` blocked on `Unhealthy` events (probe misconfigurations aren't fixed by a restart)
   - `restart_deployment` blocked when `PodStatusSummary` shows `OOMKilled` or `exit=137` (memory pressure isn't fixed by a restart)
   - `scale_deployment` and `restart_deployment` blocked on `FailedScheduling` events (impossible resource requests aren't fixed by scaling)
8. **OCI validation**: images from the LLM are validated against the standard OCI format
9. **Prompt sanitization**: Kubernetes event messages are sanitized before being sent to the LLM (control character removal, prompt injection patterns, truncation)
10. **Distroless container**: the image has no shell, minimal filesystem, non-root user
11. **Rate limiting**: prevents overloading Ollama during event storms
12. **Signal dedup**: collapses `(ns, Deployment, reason)` events into a single LLM call per `DEDUPE_TTL_SECONDS` (default 300s); cap `MAX_EVENTS_PER_POLL` (default 10)

### Prompt Injection Protection

The `reason`, `message`, and `extra` fields from Kubernetes events are sanitized before entering the LLM prompt:
- Control characters removed (except `\n` and `\t`)
- Common injection patterns redacted: "ignore previous instructions", "disregard above", "system:", "forget everything", "new instructions:"
- Fields truncated to 2000 characters (500 for reason)

### Decision flow

For each Warning event the agent follows this deterministic flow:

```
Kubernetes event (Warning)
        |
        v
Dedup per (ns, Deployment|Pod, reason) with TTL
        |  (skip if already processed within TTL)
        v
Per-poll cap (MAX_EVENTS_PER_POLL)
        |
        v
Prompt construction:
  - sanitized event
  - Deployment snapshot: replicas, containers, Allow-patch scopes, probe timings
  - PodStatusSummary: pod phase, container state, lastTerminated reason/exit
  - HARD RULES specific to each reason
        |
        v
Ollama call with JSON response schema
        |  (rate-limited at OLLAMA_RPS, retry on 5xx, timeout OLLAMA_HTTP_TIMEOUT_SECONDS)
        v
Decision validated: allowlist, severity, confidence
        |
        v
Policy guards:
  - MaybeBlockUnsafeImageUpdate       (confidence >= threshold, valid OCI, flag on)
  - MaybeBlockUnsafePatch             (patch_*: flag on, confidence >= threshold)
  - MaybeBlockRestartOnProbeFailure   (event=Unhealthy)
  - MaybeBlockRestartOnOOMKilled      (extra contains OOMKilled/exit=137)
  - MaybeBlockWrongActionOnFailedScheduling (event=FailedScheduling)
        |
        v
Execution (if dry-run off) or log-only (if dry-run on):
  - patch_* also read the Deployment annotation for extra opt-in
  - Numeric parameters validated against per-field/per-resource bounds
```

If the guard rejects the decision, the agent logs a visible `execute decision failed` and the dedup TTL rate-limits further attempts on the same signal for `DEDUPE_TTL_SECONDS`, avoiding loops.

---

## High Availability (Leader Election)

To run the agent with multiple replicas safely:

```bash
# Add to the ConfigMap
kubectl -n ai-remediator create configmap ai-remediator-config \
  ... \
  --from-literal=LEADER_ELECTION=true \
  --from-literal=LEASE_NAME=ai-remediator-leader \
  --from-literal=LEASE_NAMESPACE=ai-remediator \
  --dry-run=client -o yaml | kubectl apply -f -

# Scale to multiple replicas
kubectl -n ai-remediator scale deployment ai-remediator-agent --replicas=2
```

With leader election enabled:
- Only the leader replica executes the polling loop
- Other replicas remain on standby
- If the leader dies, another replica takes over within ~15 seconds
- The mechanism uses `Lease` (a native Kubernetes resource from `coordination.k8s.io`)

You need to add permissions for Leases:

```bash
kubectl patch clusterrole ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{
    "apiGroups":["coordination.k8s.io"],
    "resources":["leases"],
    "verbs":["get","create","update"]
  }}
]'
```

---

## Namespace-scoped RBAC

To restrict the agent to specific namespaces (instead of cluster-wide), use the manifests in `deploy/rbac-namespaced.yaml`:

```bash
kubectl apply -f deploy/rbac-namespaced.yaml
```

This creates:
- `Role` + `RoleBinding` in the target namespace (e.g., `incident-lab`)
- `Role` + `RoleBinding` for leader election in the agent's namespace
- `ServiceAccount` in the agent's namespace

To add more namespaces, duplicate the Role/RoleBinding resources:

```bash
# Create Role for a new namespace
kubectl -n <new-namespace> create role ai-remediator \
  --verb=get,list,watch,delete \
  --resource=pods,pods/log,events

kubectl -n <new-namespace> patch role ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{
    "apiGroups":["apps"],
    "resources":["deployments","replicasets"],
    "verbs":["get","list","watch","update","patch"]
  }}
]'

kubectl -n <new-namespace> create rolebinding ai-remediator \
  --role=ai-remediator \
  --serviceaccount=ai-remediator:ai-remediator
```

---

## Admin GUI

An optional web GUI with a dedicated login form lets operators run the most common tasks from the browser:

- **Login**: classic username/password form, HMAC-signed session cookie valid for 12h. The `/api/*` endpoints also accept HTTP Basic auth so curl-based scripts keep working.
- **Dashboard**: live status of the agent Deployment (desired/ready replicas), pods, ConfigMap, Secret, leader lease, **live probes for Ollama (model list + latency) and Redis (TCP ping)**, **recent decisions feed** from the remediation loop (action / severity / outcome), and a read-only view of the running configuration.
- **Logs**: live tail of the agent pod via Server-Sent Events, with pause/clear controls.
- **Cluster**: pod table for the namespaces listed in `INCLUDE_NAMESPACES`, with phase filter and name search, restart count, last-termination reason and a "logs" button that opens a tail panel (with `previous` checkbox). The namespace selector is populated from `INCLUDE_NAMESPACES`, read **live from the ConfigMap**: to add the desired namespace set it from **Configuration → Namespace filters → Include namespaces** and the Cluster page shows it immediately, without waiting for the pod restart (the agent, instead, applies the new scope on its next rollout). If the list is empty the selector shows "(no INCLUDE_NAMESPACES configured)".
- **Configuration** (accordion with multiple sections): LLM model, Ollama tuning (RPS, retries, timeouts), behavior (`DRY_RUN`, severity, polling), scaling bounds, namespace filters, action policies (`ALLOW_PATCH_*`, confidence thresholds), dedup backend + Redis, SMTP (with "Send test email"), agent replica count. Each form writes to ConfigMap or Secret and triggers a Deployment rollout.
- **Scenarios**: apply and clean up the fault scenarios described in [Error Scenarios](#error-scenarios), restricted to a sandbox namespace allowlist.
- **RBAC**: apply namespace-scoped `Role` + `RoleBinding` to onboard a new namespace without editing YAML by hand.

The source of truth for every operation is a Kubernetes object (Deployment, ConfigMap, Secret, Role, RoleBinding); the GUI never holds its own state, so it is safe to scale to multiple replicas or restart at will.

> **Note**: the Configuration page deliberately does **not** expose self-trapping variables (`WEBUI_*`, `AGENT_NAMESPACE`, `AGENT_DEPLOYMENT_NAME`, `AGENT_CONFIGMAP_NAME`, `AGENT_SECRET_NAME`, `METRICS_ADDR`, `LEADER_ELECTION`, `LEASE_*`): editing them from the GUI risks making the GUI itself unreachable or breaking leader election. For those settings use `kubectl edit cm` + a manual rollout.

### Architecture

The GUI runs inside the same pod as the agent (`internal/webui`), exposes a separate HTTP port (default `:8080`) and shares the agent's `ServiceAccount`. The login page issues an HMAC-SHA256-signed session cookie (12h TTL, key derived from `WEBUI_PASSWORD`); rotating the password invalidates every existing session at once. Endpoints also accept HTTP Basic auth headers (compared with `crypto/subtle` in constant time) so curl-based scripts keep working. **TLS must be terminated at the Ingress**: without HTTPS the form credentials travel in clear text on the wire.

### Quick install

Two flows depending on what you already have on the cluster.

#### A. From scratch (no existing agent)

The GUI is enabled by default by `scripts/install.sh` (see [Kubernetes
Installation](#kubernetes-installation)): that is the simplest path and it also
sets a random password.

If you prefer the manifests by hand (the GUI is already `WEBUI_ENABLED=true` in `deploy/agent.yaml`):

```bash
kubectl create namespace ai-remediator
kubectl apply -f deploy/rbac-cluster.yaml         # base agent RBAC (cluster-wide)
kubectl apply -f deploy/rbac-webui.yaml           # GUI Role + ClusterRole
kubectl apply -f deploy/rbac-scenarios.yaml       # optional, only for "Scenarios"
kubectl apply -f deploy/agent.yaml                # CM + Secret + Deployment + Service
# then set a real GUI password (the manifest ships a placeholder):
kubectl -n ai-remediator patch secret ai-remediator-secrets --type merge \
  -p '{"stringData":{"WEBUI_PASSWORD":"<pick-a-password>"}}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
```

#### B. Add the GUI to an existing agent install

If you installed the agent following [Kubernetes Installation](#kubernetes-installation) with your own Deployment / ConfigMap names, **do not** apply `agent.yaml`: just install the GUI RBAC and add the variables below.

```bash
kubectl apply -f deploy/rbac-webui.yaml
kubectl apply -f deploy/rbac-scenarios.yaml       # optional
```

Then add the variables to your existing ConfigMap, populate the Secret with login credentials and trigger a rollout.

### Configuration variables

Add to the agent ConfigMap (the `AGENT_*` keys point the GUI at the right Deployment / ConfigMap / Secret — only change them if your names differ from the defaults):

| Variable | Default | Notes |
| -------- | ------- | ----- |
| `WEBUI_ENABLED` | `false` | Set to `true` to enable the GUI |
| `WEBUI_ADDR` | `:8080` | Listen address (separate from `METRICS_ADDR`) |
| `AGENT_NAMESPACE` | `ai-remediator` | Namespace where the agent runs |
| `AGENT_DEPLOYMENT_NAME` | `ai-remediator-agent` | Deployment name |
| `AGENT_CONFIGMAP_NAME` | `ai-remediator-config` | ConfigMap holding non-secret config |
| `AGENT_SECRET_NAME` | `ai-remediator-secrets` | Secret for SMTP password, login, Redis |
| `SCENARIO_SANDBOX_NAMESPACES` | `incident-lab` | CSV. Only these namespaces may receive fault scenarios. Empty = scenarios disabled |

Add to the agent Secret (mandatory when `WEBUI_ENABLED=true`):

| Key | Notes |
| --- | ----- |
| `WEBUI_USERNAME` | Login username |
| `WEBUI_PASSWORD` | Login password (change from the default) |

One-liner to set credentials and enable the GUI without editing YAML:

```bash
kubectl -n ai-remediator create secret generic ai-remediator-secrets \
  --from-literal=WEBUI_USERNAME=admin \
  --from-literal=WEBUI_PASSWORD="$(openssl rand -base64 24)" \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl -n ai-remediator patch cm ai-remediator-config --type merge -p '{
  "data": { "WEBUI_ENABLED": "true" }
}'
```

### Build, push and rollout

The binary embeds HTML templates, CSS, JS and the scenario manifests as files: every GUI change requires a fresh build. Full sequence:

```bash
# Sync the working tree
git checkout master
git pull origin master

# Build + push multi-arch (amd64 + arm64; replace the registry to suit your setup)
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  -t host.docker.internal:5050/k8s-ai-remediator:latest \
  --push .

# Rollout
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
kubectl -n ai-remediator rollout status deployment/ai-remediator-agent --timeout=180s

# Verify the pod actually runs the new image: the Image ID (sha256) must change
kubectl -n ai-remediator describe pod -l app=ai-remediator-agent | grep -E 'Image:|Image ID:'
```

If the `Image ID` does not change (the `:latest` cache on the node can fool you), force a unique tag per build:

```bash
TAG="dev-$(git rev-parse --short HEAD)"
docker buildx build --platform linux/amd64,linux/arm64 \
  -t host.docker.internal:5050/k8s-ai-remediator:$TAG --push .
kubectl -n ai-remediator set image deployment/ai-remediator-agent \
  agent=host.docker.internal:5050/k8s-ai-remediator:$TAG
kubectl -n ai-remediator rollout status deployment/ai-remediator-agent
```

### Access

#### Port-forward (local tests)

```bash
kubectl -n ai-remediator port-forward svc/ai-remediator-webui 8080:80
# open http://127.0.0.1:8080 and sign in via the login form
```

> **Config saves and port-forward**: every GUI "Save and rollout" restarts the
> Deployment so the agent re-reads the config. With `replicas: 1` the single
> pod is replaced and an open `kubectl port-forward` prints `lost connection to
> pod`: this is expected (kubectl stays bound to the original pod) — just
> **re-run the port-forward**. To never lose the GUI during rollouts run
> `replicas: 2`: leader election still guarantees a single active remediator
> and the `Service` stays continuously served (see
> [High Availability](#high-availability-leader-election)). Note: for the
> **Cluster** page you don't need to wait for the rollout — `INCLUDE_NAMESPACES`
> is read live from the ConfigMap.

#### Ingress (production)

The `ai-remediator-webui` Service in `deploy/agent.yaml` serves the GUI on port 80 -> container 8080. The `Ingress` block in the manifest is commented out; enable it after preparing a hostname and a TLS certificate (e.g. via cert-manager). The no-buffer annotations below are **required** for the SSE log stream on the Logs page:

```yaml
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: ai-remediator-webui
  namespace: ai-remediator
  annotations:
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-buffering: "off"
spec:
  ingressClassName: nginx
  tls:
    - hosts: [ai-remediator.example.com]
      secretName: ai-remediator-tls
  rules:
    - host: ai-remediator.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend:
              service:
                name: ai-remediator-webui
                port:
                  number: 80
```

### RBAC permissions granted by the GUI

`deploy/rbac-webui.yaml` adds the following to the `ai-remediator` ServiceAccount:

- **In the agent namespace**: `get/list/watch/create/update/patch` on `configmaps` and `secrets`; `get/list/watch` on `pods`, `pods/log`; `get/update/patch` on `deployments` and `deployments/scale`; `get/list/watch` on `leases`.
- **Cluster-wide**: `get/list/create/patch` on `namespaces`; `get/list/create/update/delete` on `roles` and `rolebindings`; `get/list/watch` on `pods` and `get` on `pods/log` (used by the Cluster page — accesses are further filtered at the application layer by the `INCLUDE_NAMESPACES` allowlist).

These are the minimum permissions required to read ConfigMap/Secret values, persist updates, scale the Deployment, follow agent and monitored-pod logs, and onboard new namespaces via "Apply RBAC". The cluster-wide RBAC verbs are gated by the login form and should sit behind Ingress TLS.

`deploy/rbac-scenarios.yaml` (optional) adds inside a single sandbox namespace:

- `create/delete` on `apps/deployments`,
- `create/delete/get/list/patch/update` on `services`, `configmaps`, `secrets`.

Required only for the GUI "Scenarios" feature (apply + cleanup of fault manifests). Kept out of the base Role in `rbac-namespaced.yaml` on least-privilege grounds: the remediation loop itself never creates or deletes Deployments.

### Security

- **TLS required in production**: without HTTPS the login form credentials are sent in clear text.
- **Password rotation**: the cookie HMAC key is derived from `WEBUI_PASSWORD`. Changing the password (`kubectl patch secret`) invalidates every existing session in one shot.
- **Scenario sandbox**: the "Scenarios" feature rejects both cluster-scoped objects and namespaces missing from the `SCENARIO_SANDBOX_NAMESPACES` allowlist. Keep it limited to test-only namespaces.
- **Replicas**: the GUI is stateless and safe to scale. Internal leader election ensures that, even with multiple replicas, only one drives the remediation loop.
- **Audit**: every mutating action logs the action, target and timestamp to stdout (visible from the Logs page).

### Disabling the GUI

```bash
kubectl -n ai-remediator set env deployment/ai-remediator-agent WEBUI_ENABLED=false
kubectl -n ai-remediator rollout status deployment/ai-remediator-agent
```

or remove the `WEBUI_ENABLED` key from the ConfigMap and reload the Deployment. The pod will no longer open port 8080.

---

## Test Lab

The test lab uses a Deployment with a pod that starts healthy but fails when a file appears in an `emptyDir` volume. Since `emptyDir` is wiped when the pod is recreated, `delete_and_recreate_pod` is a verifiable remediation.

### Setup

```bash
# Create the namespace
kubectl create namespace incident-lab

# Create the deployment
kubectl -n incident-lab create deployment healable-app \
  --image=busybox:1.36 \
  -- /bin/sh -c 'echo "started"; while true; do if [ -f /state/poison ]; then echo "poison detected"; exit 1; fi; sleep 2; done'

# Add emptyDir volume
kubectl -n incident-lab patch deployment healable-app --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/volumes","value":[
    {"name":"state","emptyDir":{}}
  ]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[
    {"name":"state","mountPath":"/state"}
  ]},
  {"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}
]'

# Opt-in to all patch_* actions (lets the agent modify probe/resources/registry
# autonomously when appropriate)
kubectl -n incident-lab annotate deployment healable-app \
  ai-remediator/allow-patch='*' --overwrite

# Wait for the pod to be healthy
kubectl -n incident-lab rollout status deployment/healable-app --timeout=120s
```

### Triggering the Failure

```bash
POD=$(kubectl -n incident-lab get pods -o jsonpath='{.items[0].metadata.name}')
kubectl -n incident-lab exec "$POD" -c busybox -- sh -c 'touch /state/poison'
```

The container finds the poison file, terminates, and the pod enters `CrashLoopBackOff`.

### Observation

```bash
# Terminal 1: watch the pods
kubectl -n incident-lab get pods -w

# Terminal 2: watch the agent logs
kubectl -n ai-remediator logs deploy/ai-remediator-agent -f

# Terminal 3: watch the events
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp | tail -20
```

The agent should detect the Warning event, analyze it, and decide on a remediation (typically `delete_and_recreate_pod` or `restart_deployment`). The new pod starts with an empty `emptyDir` and returns to a healthy state.

---

## Error Scenarios

The `scenarios/` directory contains ready-to-apply manifests that reproduce
typical failures at four severity levels. They validate the agent's behavior
under different conditions and exercise the LLM's policy-aware decisions.

With all flags enabled (`ALLOW_PATCH_PROBE=true`, `ALLOW_PATCH_RESOURCES=true`,
`ALLOW_PATCH_REGISTRY=true`, `ALLOW_IMAGE_UPDATES=true`) **3 of the 4
scenarios** are closed autonomously and the fourth ends with a correct
abstention.

| Severity | Manifest | Event reason | Deployment opt-in | Expected action | Outcome |
|----------|----------|--------------|--------------------|-----------------|---------|
| **Low** | `scenarios/low-readiness-flaky.yaml` | `Unhealthy` | `allow-patch: probe` (present) | `patch_probe` | auto-fix |
| **Medium** | `scenarios/medium-imagepullbackoff.yaml` | `ErrImagePull` / `ImagePullBackOff` | — (uses `ALLOW_IMAGE_UPDATES`) | `set_deployment_image` | auto-fix (if registry reachable) |
| **Critical** | `scenarios/critical-oomkilled.yaml` | `BackOff` + `OOMKilled` state | `allow-patch: resources` (present) | `patch_resources` | auto-fix |
| **Severe** | `scenarios/severe-failedscheduling.yaml` | `FailedScheduling` | `allow-patch: resources` (optional) | `patch_resources` or `mark_for_manual_fix` | conditional auto-fix or abstention |

Detail of values produced by the agent (reference with qwen2.5:14b):
see [`scenarios/README.en.md`](scenarios/README.en.md).

All scenarios assume the `incident-lab` namespace already exists (see
[Test Lab](#test-lab)).

### Low scenario — flaky readiness probe

The container runs fine, but the readiness probe fails intermittently (cycles
of 20s ready / 10s not ready). Each failure emits an `Unhealthy` Warning event.

```bash
kubectl apply -f scenarios/low-readiness-flaky.yaml
kubectl -n incident-lab get pods -l scenario=low -w
kubectl -n incident-lab get events --field-selector reason=Unhealthy --sort-by=.metadata.creationTimestamp | tail
```

**Impact**: only noise in logs/events and occasional removal from endpoints.
No crashes, no restarts.
**Why low**: weak signal that may indicate an over-strict probe or flakiness
worth investigating without urgency. An invasive remediation (restart, delete)
would be overkill.

### Medium scenario — ImagePullBackOff

A Deployment references a non-existent image tag. The kubelet fails to pull
the image and emits `Failed` / `ErrImagePull` Warning events.

```bash
kubectl apply -f scenarios/medium-imagepullbackoff.yaml
kubectl -n incident-lab get pods -l scenario=medium -w
```

**Impact**: the workload never starts, but running services are unaffected.
**Why medium**: localized error, no cluster-wide degradation, typically needs
a configuration fix (correct tag) from a human.

### Critical scenario — OOMKilled in CrashLoopBackOff

The container tries to allocate more memory than `limits.memory`. The kernel
kills it with OOMKilled, kubelet restarts it, the loop repeats generating
`BackOff` and `OOMKilling` events.

```bash
kubectl apply -f scenarios/critical-oomkilled.yaml
kubectl -n incident-lab get pods -l scenario=critical -w
kubectl -n incident-lab describe pod -l scenario=critical | grep -E 'OOMKilled|Reason|Exit'
```

**Impact**: the service is continuously down, no self-recovery is possible
because every restart fails with the same error.
**Why critical**: permanent crash loop; standard remediations (restart, delete)
do not fix it. The agent must recognize that human intervention on resource
limits is required.

### Severe scenario — FailedScheduling

The pod requests resources no node can satisfy (500 CPU, 500 Gi RAM). The
scheduler repeatedly emits `FailedScheduling` and the pod stays `Pending`
indefinitely.

```bash
kubectl apply -f scenarios/severe-failedscheduling.yaml
kubectl -n incident-lab get pods -l scenario=severe
kubectl -n incident-lab describe pod -l scenario=severe | grep -A3 Events
```

**Impact**: permanent workload block, the pod will NEVER start.
**Why severe**: no agent action can fix it (neither restart, delete, scale,
nor set image). Requires manifest changes, cluster scaling, or a hardware
profile change.

### Cleanup

```bash
kubectl delete -f scenarios/low-readiness-flaky.yaml --ignore-not-found
kubectl delete -f scenarios/medium-imagepullbackoff.yaml --ignore-not-found
kubectl delete -f scenarios/critical-oomkilled.yaml --ignore-not-found
kubectl delete -f scenarios/severe-failedscheduling.yaml --ignore-not-found
# or all at once
kubectl delete -f scenarios/ --ignore-not-found
```

### What to observe in the agent logs

For each scenario you can correlate the LLM's perceived severity with the
chosen action:

```bash
kubectl -n ai-remediator logs deploy/ai-remediator-agent -f | grep -E 'decision|severity|action'
```

The Prometheus metric `remediator_decisions_total{action=...}` shows the
distribution of actions selected across the three scenarios.

---

## Development

### Running Tests

```bash
go test ./... -v -count=1
```

Tests use:
- `k8s.io/client-go/kubernetes/fake` to simulate the Kubernetes cluster
- `net/http/httptest` to simulate Ollama responses
- Coverage includes: config, actions, policy, sanitization, retry, metrics, OCI validation

### Linting

```bash
golangci-lint run ./...
```

### CI/CD

The GitHub Actions pipeline (`.github/workflows/ci.yml`) automatically runs:
1. **Lint**: `golangci-lint`
2. **Test**: with race detector and coverage
3. **Build**: static binary for linux/amd64
4. **Docker Build**: container image build
5. **Security**: vulnerability scanning with `govulncheck`

---

## Verification Commands

### Ollama Status

```bash
kubectl -n ollama get pods,svc
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

### Agent Status

```bash
kubectl -n ai-remediator get pods
kubectl -n ai-remediator logs deploy/ai-remediator-agent --tail=50
```

### Metrics

```bash
kubectl -n ai-remediator port-forward deploy/ai-remediator-agent 9090:9090
curl http://localhost:9090/metrics
curl http://localhost:9090/healthz
```

### Permission Health Check

```bash
kubectl auth can-i get pods/log \
  --as=system:serviceaccount:ai-remediator:ai-remediator \
  -n incident-lab

kubectl auth can-i update deployments \
  --as=system:serviceaccount:ai-remediator:ai-remediator \
  -n incident-lab
```

### Updating the ConfigMap

```bash
kubectl -n ai-remediator create configmap ai-remediator-config \
  --from-literal=OLLAMA_BASE_URL=http://ollama.ollama.svc.cluster.local:11434/api \
  --from-literal=OLLAMA_MODEL=qwen2.5:7b \
  --from-literal=DRY_RUN=false \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=MIN_SEVERITY=medium \
  --from-literal=SCALE_MIN=1 \
  --from-literal=SCALE_MAX=5 \
  --from-literal=ALLOW_IMAGE_UPDATES=true \
  --from-literal=IMAGE_UPDATE_CONFIDENCE_THRESHOLD=0.92 \
  --from-literal=ALLOW_PATCH_PROBE=true \
  --from-literal=ALLOW_PATCH_RESOURCES=true \
  --from-literal=ALLOW_PATCH_REGISTRY=true \
  --from-literal=PATCH_CONFIDENCE_THRESHOLD=0.85 \
  --from-literal=POD_LOG_TAIL_LINES=200 \
  --from-literal=OLLAMA_RPS=2.0 \
  --from-literal=OLLAMA_MAX_RETRIES=3 \
  --from-literal=OLLAMA_HTTP_TIMEOUT_SECONDS=360 \
  --from-literal=POLL_CONTEXT_TIMEOUT_SECONDS=480 \
  --from-literal=DEDUPE_TTL_SECONDS=300 \
  --from-literal=MAX_EVENTS_PER_POLL=10 \
  --from-literal=METRICS_ADDR=:9090 \
  --dry-run=client -o yaml | kubectl apply -f -

# Restart to apply changes
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
```

---

## Troubleshooting

Common symptoms and corrective actions, tied to the scenario flow.

### The agent pod is Running but I never see a `decision` for my events

Possible causes:
1. **The running binary does not ship the recent features**. Check the first
   line after the restart:
   ```bash
   kubectl -n ai-remediator logs deploy/ai-remediator-agent --tail=30 \
     | grep '"msg":"agent started"' | tail -1 | jq .
   ```
   It must contain `"buildFeatures":"dedup,infer-dep-from-podname,..."` and
   consistent values for `allowPatchProbe`, `dedupeTTLSec`, etc. If the
   field is missing, rebuild/push the image and `rollout restart`.
2. **The ConfigMap is missing an env var** (e.g. `MIN_SEVERITY=low` while
   events are at `medium`): review the Configuration section.
3. **Dedup TTL is active**: each signal is reconsidered only after
   `DEDUPE_TTL_SECONDS` (default 5 min). Wait or lower it.

### `ollama rate limiter: context deadline exceeded`

The LLM request sat in the queue longer than the `pollCtx`. Typical during
event storms that are not deduplicated (rolled-over pods with different
names) or for system-namespace events that should never be processed.

Verify:
- `"excludeNamespaces"` at startup contains `kube-system`, `kube-public`,
  `kube-node-lease`, `local-path-storage`. Without the denylist every
  CoreDNS or kube-scheduler warning reaches the LLM.
- `"dedupeTTLSec"` and `"maxEventsPerPoll"` are non-zero.
- Inference works: `ResolveDeploymentFromPod` + `InferDeploymentFromPodName`
  should converge all phantom pods onto the same Deployment.
- On a test cluster with many stale events, wait a poll or two: the dedup
  TTL and cap drain the queue. To cut further noise, switch to a tight
  allowlist with `INCLUDE_NAMESPACES=incident-lab`.

### `Post "...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`

Ollama takes longer than `OLLAMA_HTTP_TIMEOUT_SECONDS` to respond.
Typical times on CPU:
- `qwen2.5:7b`: 30-90s (180s timeout is enough)
- `qwen2.5:14b`: 100-360s (needs 360-600s timeout)

**Preferred fix**: switch to 7b (4x faster):
```bash
kubectl -n ollama exec -it deploy/ollama -- ollama pull qwen2.5:7b
kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "OLLAMA_MODEL":"qwen2.5:7b",
    "OLLAMA_HTTP_TIMEOUT_SECONDS":"180",
    "POLL_CONTEXT_TIMEOUT_SECONDS":"300"
  }}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
```

**Fallback if staying on 14b**: raise the timeouts
```bash
kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "OLLAMA_HTTP_TIMEOUT_SECONDS":"600",
    "POLL_CONTEXT_TIMEOUT_SECONDS":"720"
  }}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
```
Invariant: `POLL_CONTEXT_TIMEOUT_SECONDS > OLLAMA_HTTP_TIMEOUT_SECONDS`.

### `Post "...": context deadline exceeded` (without `Client.Timeout`)

Here it's the `pollCtx` expiring, not the HTTP client. Raise
`POLL_CONTEXT_TIMEOUT_SECONDS` (see above).

### Pods in `ImagePullBackOff` (Redis, `healable-app`, scenarios)

On local clusters without Docker Hub egress (Docker Desktop, kind,
minikube) the kubelet cannot pull `redis:7.2-alpine`, `busybox:1.36` or
`polinux/stress:latest`. Symptom: `kubectl -n <ns> describe pod ...`
shows `Failed to pull image` against `registry-1.docker.io`.

Fix: run the mirror script once and rewrite the workload image to point
at the local registry:

```bash
scripts/mirror-images.sh

# Redis
kubectl -n ai-remediator set image deploy/ai-remediator-redis \
  redis=host.docker.internal:5050/redis:7.2-alpine

# Scenarios (once per affected Deployment)
kubectl -n incident-lab set image deploy/<name> <container>=host.docker.internal:5050/busybox:1.36
```

Details in the [Mirroring upstream images to the local registry](#mirroring-upstream-images-to-the-local-registry) section.

### `notify: SMTP not configured, notifications disabled` after creating the Secret

The agent logs `SMTP not configured` when at least one of
`NOTIFY_SMTP_HOST`, `NOTIFY_SMTP_USER`, `NOTIFY_TO` is empty in the
container env. Common causes:

1. **The Secret does not exist**: `kubectl set env --from=secret/ai-remediator-notify`
   returns `secrets "ai-remediator-notify" not found`. Create it first
   (see [Email notifications](#email-notifications)), then rerun the
   command.
2. **The Secret was never mounted on the Deployment**: the `envFrom`
   patch was skipped or applied to a different Deployment. Verify:
   ```bash
   kubectl -n ai-remediator get deployment ai-remediator-agent \
     -o jsonpath='{.spec.template.spec.containers[0].envFrom}' | jq .
   ```
   It must contain both `configMapRef: ai-remediator-config` and
   `secretRef: ai-remediator-notify`.
3. **Missing `rollout restart`** after mounting the Secret: existing
   pods still have empty env vars. Force the rollout:
   ```bash
   kubectl -n ai-remediator rollout restart deployment/ai-remediator-agent
   ```

### `execute decision failed` with one of the following messages

| Message | Cause | Fix |
|---------|-------|-----|
| `set_deployment_image disabled by policy` | `ALLOW_IMAGE_UPDATES=false` | Enable it in the ConfigMap if you want tag auto-fix |
| `set_deployment_image blocked: confidence X below threshold Y` | LLM not confident enough | Lower `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` or wait for a clearer signal |
| `set_deployment_image blocked: invalid OCI image reference` | LLM invented a bad image | None: correct rejection |
| `patch_probe disabled by policy` | `ALLOW_PATCH_PROBE=false` | Enable the flag |
| `patch_probe blocked: confidence X below threshold Y` | LLM not confident | Lower `PATCH_CONFIDENCE_THRESHOLD` or wait for new evidence |
| `deployment ... does not opt in to patch_probe (set annotation ai-remediator/allow-patch)` | Missing annotation on Deployment | `kubectl annotate deployment X ai-remediator/allow-patch=probe` |
| `probe field "failure_threshold" not an integer: parsing "x5"` | LLM emitted a multiplier or expression | Correct rejection. Wait for the next cycle; the prompt now explicitly requires plain integers |
| `cpu_request: quantity outside bounds` | Value outside `[10m, 8]` CPU or `[16Mi, 16Gi]` memory | Correct rejection. Bump bounds in code or manual fix |
| `restart_deployment blocked: event reason=Unhealthy` | LLM suggested a restart for a flaky probe | Expected. With the `probe` opt-in enabled (`ALLOW_PATCH_PROBE=true` + annotation) the agent auto-converts it to `patch_probe`; otherwise wait for the LLM to converge on `patch_probe` |
| `restart_deployment blocked: pod status shows OOMKilled` | LLM suggested a restart on an OOM | Expected. With the `resources` opt-in enabled the agent auto-converts it to `patch_resources` (512Mi floor); otherwise wait for `patch_resources` |
| `scale_deployment blocked: event reason=FailedScheduling` | LLM suggested scaling an impossible-to-schedule pod | Expected. Wait for `patch_resources` or `mark_for_manual_fix` |

### `roles ... forbidden: attempting to grant RBAC permissions not currently held` from "Apply RBAC"

The GUI applies the onboarding `Role` as the `ai-remediator` ServiceAccount.
Kubernetes forbids creating a Role that grants permissions the SA does not hold
(privilege-escalation prevention). It happened because the Role included
`events: [delete]` while the agent only holds `events` get/list/watch (it never
deletes events). **Fixed**: the Role now grants `delete` on `pods` only. To pick
up the fix on a running agent you need a new image build and a rollout.
Immediate workaround without a rebuild: onboard the namespace by hand with your
own (admin) kubeconfig, which bypasses the check against the agent SA:

```bash
kubectl apply -f deploy/rbac-namespaced.yaml   # already fixed (delete on pods only)
# for a namespace other than incident-lab, rewrite the namespace field before applying
```

---

## Environment Reset

> If you installed with `scripts/install.sh`, the simplest path is
> `scripts/install.sh --uninstall` (removes the namespaces and RBAC it created;
> it leaves Ollama, which you can drop with `kubectl delete namespace ollama`).

### Test Lab Only

```bash
kubectl delete namespace incident-lab --ignore-not-found
```

### Full Reset

```bash
kubectl delete namespace incident-lab --ignore-not-found
kubectl delete namespace ai-remediator --ignore-not-found
kubectl delete namespace ollama --ignore-not-found
# Cluster-wide RBAC (agent + GUI)
kubectl delete clusterrolebinding ai-remediator ai-remediator-webui-rbac-admin --ignore-not-found
kubectl delete clusterrole ai-remediator ai-remediator-webui-rbac-admin --ignore-not-found
```
