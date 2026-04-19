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
  - [1. Installing Ollama](#1-installing-ollama)
  - [2. Installing the Agent](#2-installing-the-agent)
- [Configuration](#configuration)
- [Supported Remediations](#supported-remediations)
- [Email notifications](#email-notifications)
- [Observability](#observability)
- [Security](#security)
- [High Availability (Leader Election)](#high-availability-leader-election)
- [Namespace-scoped RBAC](#namespace-scoped-rbac)
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
                    +--------v---------+
                    |  ai-remediator   |
                    |  (Go agent)      |
                    +--------+---------+
                             |
                  Structured JSON prompt
                             |
                    +--------v---------+
                    |     Ollama       |
                    |  (local LLM)    |
                    +--------+---------+
                             |
                  JSON Decision (action, confidence, params)
                             |
                    +--------v---------+
                    |  ai-remediator   |
                    |  Execution Engine|
                    +------------------+
                             |
              Remediation (restart, delete, scale, ...)
```

**Operational Flow:**

1. Kubernetes generates a `Warning` event (CrashLoopBackOff, ImagePullBackOff, etc.)
2. The agent lists events via the API and filters unprocessed Warning events
3. For each event, it builds a prompt with namespace, kind, name, reason, message, and a snapshot of the associated Deployment
4. Sends the prompt to Ollama with a JSON schema constraining the response
5. Receives a structured decision with action, confidence, and parameters
6. Validates the decision: allowlist, policy bounds, OCI image format, confidence threshold
7. Executes the action (or logs it in dry-run mode)

When an event concerns a Pod, the agent traces back to the Deployment via `ownerReferences` (Pod -> ReplicaSet -> Deployment).

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
│   │   └── model.go            # Shared types: Action, Decision, ChatRequest/Response
│   ├── config/
│   │   ├── config.go           # AgentConfig, environment variable parsing
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
│   └── metrics/
│       ├── metrics.go          # Prometheus-compatible metrics (zero external deps)
│       └── metrics_test.go
├── deploy/
│   └── rbac-namespaced.yaml    # Example namespace-scoped RBAC
├── .github/
│   └── workflows/
│       └── ci.yml              # CI/CD: lint, test, build, Docker, security scan
├── Dockerfile                   # Multi-stage build (distroless, non-root)
├── go.mod
└── go.sum
```

### Internal Packages

| Package | Responsibility |
|---------|---------------|
| `internal/model` | Shared types across all packages: Action constants, Decision struct, Ollama API types |
| `internal/config` | `AgentConfig` with all parameters and helpers for env var parsing with defaults |
| `internal/ollama` | HTTP client for Ollama with rate limiting (`golang.org/x/time/rate`), retry with exponential backoff, TLS support |
| `internal/kube` | All Kubernetes operations: Pod->Deployment resolution, restart, delete, scale, set image, log inspection, snapshot |
| `internal/policy` | Action allowlist, OCI image validation, unsafe image update blocking, anti-injection prompt sanitization |
| `internal/metrics` | Metrics in Prometheus text exposition format, zero external dependencies |

---

## Prerequisites

- A running Kubernetes cluster (Docker Desktop, minikube, kind, k3s, EKS, GKE, AKS, ...)
- `kubectl` configured for the correct cluster
- Docker for building the image
- Go 1.21+ for local development (optional, the build happens in Docker)

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

# Build and push
REGISTRY=host.docker.internal:5050
IMAGE=$REGISTRY/ai-remediator:0.2.0
docker build -t "$IMAGE" .
docker push "$IMAGE"
```

The Dockerfile uses a multi-stage build:
- **Stage 1**: Go 1.26.1 compiles a static binary (`CGO_ENABLED=0`)
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

---

## Kubernetes Installation

### 1. Installing Ollama

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

### 2. Installing the Agent

```bash
# Create the namespace
kubectl create namespace ai-remediator

# Create the service account
kubectl create serviceaccount ai-remediator -n ai-remediator

# Create the ClusterRole
kubectl create clusterrole ai-remediator \
  --verb=get,list,watch,delete \
  --resource=pods,pods/log,events,namespaces

# Add rules for deployments and replicasets
kubectl patch clusterrole ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{
    "apiGroups":["apps"],
    "resources":["deployments","replicasets"],
    "verbs":["get","list","watch","update","patch"]
  }}
]'

# Create the ClusterRoleBinding
kubectl create clusterrolebinding ai-remediator \
  --clusterrole=ai-remediator \
  --serviceaccount=ai-remediator:ai-remediator

# Create the ConfigMap (full set: auto-fix for probe/resources/registry,
# extended timeouts for slow local models, dedup TTL)
kubectl create configmap ai-remediator-config \
  -n ai-remediator \
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
  --from-literal=METRICS_ADDR=:9090

# Create the deployment using the image from the local registry
kubectl -n ai-remediator create deployment ai-remediator \
  --image=host.docker.internal:5050/ai-remediator:0.2.0

# Attach service account, ConfigMap, and metrics port
kubectl -n ai-remediator patch deployment ai-remediator --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/serviceAccountName","value":"ai-remediator"},
  {"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"},
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom","value":[
    {"configMapRef":{"name":"ai-remediator-config"}}
  ]},
  {"op":"add","path":"/spec/template/spec/containers/0/ports","value":[
    {"containerPort":9090,"name":"metrics"}
  ]}
]'

# Verify the rollout
kubectl -n ai-remediator rollout status deployment/ai-remediator --timeout=180s
kubectl -n ai-remediator logs deploy/ai-remediator --tail=20
```

> **Note**: `host.docker.internal:5050` assumes the kubelet can resolve
> `host.docker.internal` to the host gateway (default on Docker Desktop).
> On kind/minikube adapt the hostname as described in the [Build](#build)
> section.

---

## Configuration

All variables are read from environment variables (typically via ConfigMap).

### Core Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OLLAMA_BASE_URL` | `http://ollama.ollama.svc.cluster.local:11434/api` | Ollama API base URL |
| `OLLAMA_MODEL` | `qwen2.5:14b` (code default). Recommended on local CPU: `qwen2.5:7b` | LLM model name (must match `ollama list`). On CPU use `7b` (~30-90s per call). For GPU or top quality use `14b` (~3-6 min on CPU) |
| `DRY_RUN` | `false` | If `true`, logs decisions without applying remediation |
| `POLL_INTERVAL_SECONDS` | `30` | Event polling interval (seconds) |
| `DEDUPE_TTL_SECONDS` | `300` | Dedup TTL for `(ns, kind, name, reason)` signals: identical events within the window do not trigger a new LLM call |
| `MAX_EVENTS_PER_POLL` | `10` | Maximum number of events that trigger an LLM call per poll cycle; excess events are deferred to the next poll |
| `INCLUDE_NAMESPACES` | *(empty)* | Comma-separated allowlist of namespaces. When set the agent only reacts to events from those namespaces. Empty = all namespaces minus the excluded ones |
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

Example opt-in on the target Deployment:

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
kubectl -n ai-remediator patch deployment ai-remediator --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom/-","value":
    {"secretRef":{"name":"ai-remediator-notify"}}}
]'

kubectl -n ai-remediator rollout restart deployment/ai-remediator
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
kubectl -n ai-remediator logs deploy/ai-remediator --tail=30 \
  | grep 'notify: SMTP configured'
```

Expected output:
```
notify: SMTP configured host=smtp.mail.me.com port=587 from=alerts-bot@icloud.com to=ops-alerts@example.com minSeverity=medium
```

If you see `notify: SMTP not configured, notifications disabled` instead,
the Secret was not mounted: check the Deployment `envFrom` with
`kubectl -n ai-remediator get deployment ai-remediator -o jsonpath='{.spec.template.spec.containers[0].envFrom}'`.

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
    memory_limit: 256Mi
    memory_request: 128Mi

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
| `remediator_events_skipped_total` | Gauge | Events skipped (dedup or non-Warning) |
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
kubectl -n ai-remediator expose deployment ai-remediator \
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
kubectl -n ai-remediator scale deployment ai-remediator --replicas=2
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
kubectl -n ai-remediator logs deploy/ai-remediator -f

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
kubectl -n ai-remediator logs deploy/ai-remediator -f | grep -E 'decision|severity|action'
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
kubectl -n ai-remediator logs deploy/ai-remediator --tail=50
```

### Metrics

```bash
kubectl -n ai-remediator port-forward deploy/ai-remediator 9090:9090
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
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```

---

## Troubleshooting

Common symptoms and corrective actions, tied to the scenario flow.

### The agent pod is Running but I never see a `decision` for my events

Possible causes:
1. **The running binary does not ship the recent features**. Check the first
   line after the restart:
   ```bash
   kubectl -n ai-remediator logs deploy/ai-remediator --tail=30 \
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
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```

**Fallback if staying on 14b**: raise the timeouts
```bash
kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "OLLAMA_HTTP_TIMEOUT_SECONDS":"600",
    "POLL_CONTEXT_TIMEOUT_SECONDS":"720"
  }}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```
Invariant: `POLL_CONTEXT_TIMEOUT_SECONDS > OLLAMA_HTTP_TIMEOUT_SECONDS`.

### `Post "...": context deadline exceeded` (without `Client.Timeout`)

Here it's the `pollCtx` expiring, not the HTTP client. Raise
`POLL_CONTEXT_TIMEOUT_SECONDS` (see above).

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
| `restart_deployment blocked: event reason=Unhealthy` | LLM suggested a restart for a flaky probe | Expected. Wait for the LLM to converge on `patch_probe` (if the annotation is present) |
| `restart_deployment blocked: pod status shows OOMKilled` | LLM suggested a restart on an OOM | Expected. Wait for `patch_resources` |
| `scale_deployment blocked: event reason=FailedScheduling` | LLM suggested scaling an impossible-to-schedule pod | Expected. Wait for `patch_resources` or `mark_for_manual_fix` |

---

## Environment Reset

### Test Lab Only

```bash
kubectl delete namespace incident-lab --ignore-not-found
```

### Full Reset

```bash
kubectl delete namespace incident-lab --ignore-not-found
kubectl delete namespace ai-remediator --ignore-not-found
kubectl delete namespace ollama --ignore-not-found
kubectl delete clusterrolebinding ai-remediator --ignore-not-found
kubectl delete clusterrole ai-remediator --ignore-not-found
```
