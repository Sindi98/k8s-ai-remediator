# Scenari di errore

> **[English version](README.en.md)**

Manifest Kubernetes che riproducono guasti controllati a quattro livelli di
severita per validare il comportamento dell'agente `ai-remediator`.

Con tutti i flag abilitati (`ALLOW_PATCH_PROBE=true`, `ALLOW_PATCH_RESOURCES=true`,
`ALLOW_PATCH_REGISTRY=true`, `ALLOW_IMAGE_UPDATES=true`) e le annotation
di opt-in sui Deployment, l'agente puo chiudere in autonomia **3 scenari su 4**
e astenersi correttamente sul quarto.

| File | Severita | Event reason | Opt-in richiesto | Action attesa | Esito |
|------|----------|--------------|-------------------|----------------|-------|
| `low-readiness-flaky.yaml` | low | `Unhealthy` | `allow-patch: probe` | `patch_probe` | **auto-fix** |
| `medium-imagepullbackoff.yaml` | medium | `ErrImagePull` / `ImagePullBackOff` | `ALLOW_IMAGE_UPDATES=true` | `set_deployment_image` | **auto-fix** (se il registry raggiunge il nuovo tag) |
| `critical-oomkilled.yaml` | critical | `BackOff` + stato `OOMKilled` | `allow-patch: resources` | `patch_resources` | **auto-fix** |
| `severe-failedscheduling.yaml` | severe | `FailedScheduling` | `allow-patch: resources` (opzionale) | `patch_resources` o `mark_for_manual_fix` | **auto-fix condizionale** o astensione |

Tutti i manifest `low-*` e `critical-*` contengono gia l'annotation di opt-in.
Per il `severe` va aggiunta a mano se si vuole far provare `patch_resources`:

```bash
kubectl -n incident-lab annotate deployment unschedulable \
  ai-remediator/allow-patch=resources --overwrite
```

## Valori prodotti dall'agente (reference)

Esempi di decisioni osservate con qwen2.5:14b su cluster locale:

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
Effetto: la readinessProbe passa da `failureThreshold=2, periodSeconds=5` a
`failureThreshold=5, periodSeconds=15`. Con il ciclo 20s ready / 10s not-ready
dello scenario la probe non fallisce piu abbastanza a lungo per generare eventi
`Unhealthy`.

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
Effetto: l'immagine del container passa dal tag inventato a `busybox:latest`.
Richiede che il kubelet riesca a raggiungere `docker.io` o che l'immagine sia
disponibile nel registry locale. In caso di rate-limit Docker Hub, pre-pullare
e pushare sul registry `host.docker.internal:5050`.

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
Effetto: il container passa da `memory_limit=32Mi` a `memory_limit=256Mi`.
`polinux/stress` completa l'allocazione di 256MB senza OOM → pod `Running`.

### severe-failedscheduling (patch_resources o astensione)
Con opt-in attivo, ci si aspetta:
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
Nota: il validator applica bounds `[10m, 8]` CPU e `[16Mi, 16Gi]` memoria.
Valori proposti oltre questi limiti vengono rifiutati con `quantity outside bounds`.

Senza opt-in: `mark_for_manual_fix` — correttamente l'agente si astiene
perche non puo dedurre da solo valori sensati per un workload generico.

## Uso

```bash
kubectl create namespace incident-lab --dry-run=client -o yaml | kubectl apply -f -
kubectl apply -f scenarios/low-readiness-flaky.yaml
kubectl apply -f scenarios/medium-imagepullbackoff.yaml
kubectl apply -f scenarios/critical-oomkilled.yaml
kubectl apply -f scenarios/severe-failedscheduling.yaml

# Opt-in aggiuntivo per il severe (se vuoi vedere patch_resources)
kubectl -n incident-lab annotate deployment unschedulable \
  ai-remediator/allow-patch=resources --overwrite

# Osserva gli eventi generati
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp

# Segui le decisioni dell'agente
kubectl -n ai-remediator logs deploy/ai-remediator -f \
  | grep -E '"msg":"decision"|patch_|set_deployment_image|blocked'

# Pulizia
kubectl delete -f scenarios/
```

Consulta la sezione **Scenari di errore** del README principale per il
contesto completo e i guardrail applicati.
