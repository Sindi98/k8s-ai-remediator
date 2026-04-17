# Error scenarios

> **[Versione italiana](README.md)**

Kubernetes manifests that reproduce controlled failures at four severity
levels to validate the `ai-remediator` agent's behavior.

With all flags enabled (`ALLOW_PATCH_PROBE=true`, `ALLOW_PATCH_RESOURCES=true`,
`ALLOW_PATCH_REGISTRY=true`, `ALLOW_IMAGE_UPDATES=true`) and the opt-in
annotations on the Deployments, the agent can close **3 of the 4 scenarios**
autonomously and correctly abstains on the fourth.

| File | Severity | Event reason | Required opt-in | Expected action | Outcome |
|------|----------|--------------|------------------|-----------------|---------|
| `low-readiness-flaky.yaml` | low | `Unhealthy` | `allow-patch: probe` | `patch_probe` | **auto-fix** |
| `medium-imagepullbackoff.yaml` | medium | `ErrImagePull` / `ImagePullBackOff` | `ALLOW_IMAGE_UPDATES=true` | `set_deployment_image` | **auto-fix** (if the registry is reachable) |
| `critical-oomkilled.yaml` | critical | `BackOff` + `OOMKilled` state | `allow-patch: resources` | `patch_resources` | **auto-fix** |
| `severe-failedscheduling.yaml` | severe | `FailedScheduling` | `allow-patch: resources` (optional) | `patch_resources` or `mark_for_manual_fix` | **conditional auto-fix** or abstention |

The `low-*` and `critical-*` manifests already ship the opt-in annotation.
Add it manually on the `severe` one if you want to exercise `patch_resources`:

```bash
kubectl -n incident-lab annotate deployment unschedulable \
  ai-remediator/allow-patch=resources --overwrite
```

## Values produced by the agent (reference)

Example decisions observed with qwen2.5:14b on a local cluster:

### low-readiness-flaky (patch_probe)
```json
{
  "action": "patch_probe",
  "severity": "high",
  "confidence": 1.0,
  "params": {
    "deployment_name": "flaky-probe",
    "container": "app",
    "probe": "readiness",
    "failure_threshold": "5",
    "period_seconds": "15"
  }
}
```
Effect: the readiness probe moves from `failureThreshold=2, periodSeconds=5`
to `failureThreshold=5, periodSeconds=15`. With the scenario's 20s ready / 10s
not-ready cycle the probe no longer stays failing long enough to emit
`Unhealthy` events.

### medium-imagepullbackoff (set_deployment_image)
```json
{
  "action": "set_deployment_image",
  "severity": "high",
  "confidence": 1.0,
  "params": {
    "deployment_name": "broken-image",
    "image": "busybox:latest"
  }
}
```
Effect: the container image changes from the bogus tag to `busybox:latest`.
Requires the kubelet to reach `docker.io` or the image to exist in the local
registry. In case of Docker Hub rate limits, pre-pull and push to the local
registry `host.docker.internal:5050`.

### critical-oomkilled (patch_resources)
```json
{
  "action": "patch_resources",
  "severity": "high",
  "confidence": 1.0,
  "params": {
    "deployment_name": "memory-hog",
    "container": "app",
    "memory_limit": "256Mi",
    "memory_request": "128Mi"
  }
}
```
Effect: the container moves from `memory_limit=32Mi` to `memory_limit=256Mi`.
`polinux/stress` completes the 256MB allocation without OOM → pod `Running`.

### severe-failedscheduling (patch_resources or abstention)
With opt-in enabled you expect:
```json
{
  "action": "patch_resources",
  "severity": "critical",
  "params": {
    "deployment_name": "unschedulable",
    "container": "app",
    "cpu_request": "100m",
    "memory_request": "64Mi",
    "cpu_limit": "500m",
    "memory_limit": "256Mi"
  }
}
```
Note: the validator enforces bounds `[10m, 8]` CPU and `[16Mi, 16Gi]` memory.
Proposed values outside those bounds are rejected with `quantity outside bounds`.

Without the opt-in: `mark_for_manual_fix` — the agent correctly abstains
because it cannot infer sensible values for a generic workload.

## Usage

```bash
kubectl create namespace incident-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f scenarios/low-readiness-flaky.yaml
kubectl apply -f scenarios/medium-imagepullbackoff.yaml
kubectl apply -f scenarios/critical-oomkilled.yaml
kubectl apply -f scenarios/severe-failedscheduling.yaml

# Extra opt-in for the severe scenario (if you want patch_resources)
kubectl -n incident-lab annotate deployment unschedulable \
  ai-remediator/allow-patch=resources --overwrite

# Watch generated events
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp

# Follow the agent decisions
kubectl -n ai-remediator logs deploy/ai-remediator -f \
  | grep -E '"msg":"decision"|patch_|set_deployment_image|blocked'

# Cleanup
kubectl delete -f scenarios/
```

See the **Error scenarios** section in the main README for full context and
applied guardrails.
