# Error scenarios

> **[Versione italiana](README.md)**

Kubernetes manifests that reproduce controlled failures at four severity
levels to validate the `ai-remediator` agent's behavior.

| File | Severity | Event reason | Expected remediation |
|------|----------|--------------|----------------------|
| `low-readiness-flaky.yaml` | low | `Unhealthy` | `noop` / `inspect_pod_logs` |
| `medium-imagepullbackoff.yaml` | medium | `Failed`, `ErrImagePull`, `ImagePullBackOff` | `mark_for_manual_fix` / `ask_human` (or `set_deployment_image` if `ALLOW_IMAGE_UPDATES=true`) |
| `critical-oomkilled.yaml` | critical | `BackOff`, `OOMKilling` | `inspect_pod_logs` / `ask_human` |
| `severe-failedscheduling.yaml` | severe | `FailedScheduling` | `mark_for_manual_fix` / `ask_human` |

## Usage

```bash
kubectl create namespace incident-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f scenarios/low-readiness-flaky.yaml
kubectl apply -f scenarios/medium-imagepullbackoff.yaml
kubectl apply -f scenarios/critical-oomkilled.yaml
kubectl apply -f scenarios/severe-failedscheduling.yaml

# Watch generated events
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp

# Cleanup
kubectl delete -f scenarios/
```

See the **Error Scenarios** section in the main README for details on each
scenario and the expected agent actions.
