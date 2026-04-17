# Scenari di errore

> **[English version](README.en.md)**

Manifest Kubernetes che riproducono guasti controllati a quattro livelli di
severita per validare il comportamento dell'agente `ai-remediator`.

| File | Severita | Reason degli eventi | Remediation attesa |
|------|----------|---------------------|--------------------|
| `low-readiness-flaky.yaml` | low | `Unhealthy` | `noop` / `inspect_pod_logs` |
| `medium-imagepullbackoff.yaml` | medium | `Failed`, `ErrImagePull`, `ImagePullBackOff` | `mark_for_manual_fix` / `ask_human` (o `set_deployment_image` se `ALLOW_IMAGE_UPDATES=true`) |
| `critical-oomkilled.yaml` | critical | `BackOff`, `OOMKilling` | `inspect_pod_logs` / `ask_human` |
| `severe-failedscheduling.yaml` | severe (grave) | `FailedScheduling` | `mark_for_manual_fix` / `ask_human` |

## Uso

```bash
kubectl create namespace incident-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f scenarios/low-readiness-flaky.yaml
kubectl apply -f scenarios/medium-imagepullbackoff.yaml
kubectl apply -f scenarios/critical-oomkilled.yaml
kubectl apply -f scenarios/severe-failedscheduling.yaml

# Osserva gli eventi generati
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp

# Pulizia
kubectl delete -f scenarios/
```

Consulta la sezione **Scenari di errore** del README principale per i dettagli
su ogni scenario e le azioni attese dall'agente.
