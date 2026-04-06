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
- [Observability](#observability)
- [Security](#security)
- [High Availability (Leader Election)](#high-availability-leader-election)
- [Namespace-scoped RBAC](#namespace-scoped-rbac)
- [Test Lab](#test-lab)
- [Development](#development)
- [Verification Commands](#verification-commands)
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

- A running Kubernetes cluster (minikube, kind, k3s, EKS, GKE, AKS, ...)
- `kubectl` configured for the correct cluster
- Docker for building the image
- Go 1.21+ for local development (optional, the build happens in Docker)

---

## Build

### Building the Docker Image

```bash
docker build -t ai-remediator:0.2.0 .
```

The Dockerfile uses a multi-stage build:
- **Stage 1**: Go 1.26.1 compiles a static binary (`CGO_ENABLED=0`)
- **Stage 2**: `gcr.io/distroless/static:nonroot` as the base image (no shell, non-root user)

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
    "requests":{"cpu":"2","memory":"4Gi"},
    "limits":{"cpu":"4","memory":"8Gi"}
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
kubectl -n ollama exec -it deploy/ollama -- ollama pull qwen2.5:14b
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

> **Note**: The `OLLAMA_MODEL` value in the ConfigMap must exactly match the name shown by `ollama list`.

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

# Create the ConfigMap
kubectl create configmap ai-remediator-config \
  -n ai-remediator \
  --from-literal=OLLAMA_BASE_URL=http://ollama.ollama.svc.cluster.local:11434/api \
  --from-literal=OLLAMA_MODEL=qwen2.5:14b \
  --from-literal=DRY_RUN=false \
  --from-literal=SCALE_MIN=1 \
  --from-literal=SCALE_MAX=5 \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=ALLOW_IMAGE_UPDATES=false \
  --from-literal=IMAGE_UPDATE_CONFIDENCE_THRESHOLD=0.92 \
  --from-literal=POD_LOG_TAIL_LINES=200 \
  --from-literal=OLLAMA_RPS=2.0 \
  --from-literal=OLLAMA_MAX_RETRIES=3 \
  --from-literal=METRICS_ADDR=:9090

# Create the deployment
kubectl -n ai-remediator create deployment ai-remediator \
  --image=ai-remediator:0.2.0

# Attach service account, ConfigMap, and local image policy
kubectl -n ai-remediator patch deployment ai-remediator --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/serviceAccountName","value":"ai-remediator"},
  {"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"},
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

> **Note**: Use `imagePullPolicy: Never` only with locally built images (minikube, kind). For a remote registry, remove this setting.

---

## Configuration

All variables are read from environment variables (typically via ConfigMap).

### Core Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `OLLAMA_BASE_URL` | `http://ollama.ollama.svc.cluster.local:11434/api` | Ollama API base URL |
| `OLLAMA_MODEL` | `qwen2.5:14b` | LLM model name (must match `ollama list`) |
| `DRY_RUN` | `false` | If `true`, logs decisions without applying remediation |
| `POLL_INTERVAL_SECONDS` | `30` | Event polling interval (seconds) |

### Policy Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SCALE_MIN` | `1` | Minimum allowed replicas for `scale_deployment` |
| `SCALE_MAX` | `5` | Maximum allowed replicas |
| `ALLOW_IMAGE_UPDATES` | `false` | Enables the `set_deployment_image` action |
| `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` | `0.92` | Minimum confidence to update an image |

### Ollama Variables (Resilience)

| Variable | Default | Description |
|----------|---------|-------------|
| `OLLAMA_RPS` | `2.0` | Max requests per second to Ollama (rate limiting) |
| `OLLAMA_MAX_RETRIES` | `3` | Retry attempts for transient errors (5xx, network) with exponential backoff |
| `OLLAMA_TLS_SKIP_VERIFY` | `false` | Skip TLS verification (for self-signed certificates) |
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

### Container Selection Logic for Logs

In multi-container pods, `inspect_pod_logs` selects the container:
1. Uses the container specified in parameters, if present and valid
2. Otherwise selects the container with the highest restart count
3. Falls back to the first container in the spec

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

1. **Action allowlist**: only the 9 predefined actions are accepted; any other is rejected
2. **Dry-run mode**: with `DRY_RUN=true` no changes are applied to the cluster
3. **Policy bounds**: `scale_deployment` constrained to `SCALE_MIN`/`SCALE_MAX`
4. **Confidence threshold**: `set_deployment_image` blocked below the configured threshold
5. **OCI validation**: images from the LLM are validated against the standard OCI format
6. **Prompt sanitization**: Kubernetes event messages are sanitized before being sent to the LLM (control character removal, prompt injection patterns, truncation)
7. **Distroless container**: the image has no shell, minimal filesystem, non-root user
8. **Rate limiting**: prevents overloading Ollama during event storms

### Prompt Injection Protection

The `reason`, `message`, and `extra` fields from Kubernetes events are sanitized before entering the LLM prompt:
- Control characters removed (except `\n` and `\t`)
- Common injection patterns redacted: "ignore previous instructions", "disregard above", "system:", "forget everything", "new instructions:"
- Fields truncated to 2000 characters (500 for reason)

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
  --from-literal=OLLAMA_MODEL=qwen2.5:14b \
  --from-literal=DRY_RUN=false \
  --from-literal=SCALE_MIN=1 \
  --from-literal=SCALE_MAX=5 \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=ALLOW_IMAGE_UPDATES=false \
  --from-literal=IMAGE_UPDATE_CONFIDENCE_THRESHOLD=0.92 \
  --from-literal=POD_LOG_TAIL_LINES=200 \
  --from-literal=OLLAMA_RPS=2.0 \
  --from-literal=OLLAMA_MAX_RETRIES=3 \
  --from-literal=METRICS_ADDR=:9090 \
  --dry-run=client -o yaml | kubectl apply -f -

# Restart to apply changes
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```

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
