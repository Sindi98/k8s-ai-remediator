# k8s-ai-remediator

> **[English version](README.en.md)**

Agente AI in Go che osserva eventi Kubernetes di tipo `Warning` e applica remediation controllate usando un LLM locale (Ollama). L'agente costruisce prompt contestuali dagli eventi del cluster, riceve decisioni JSON strutturate e applica solo azioni presenti in una allowlist predefinita, con molteplici livelli di sicurezza.

---

## Indice

- [Architettura](#architettura)
- [Struttura del progetto](#struttura-del-progetto)
- [Prerequisiti](#prerequisiti)
- [Build](#build)
- [Installazione su Kubernetes](#installazione-su-kubernetes)
  - [1. Installazione di Ollama](#1-installazione-di-ollama)
  - [2. Installazione dell'agente](#2-installazione-dellagente)
- [Configurazione](#configurazione)
- [Remediation supportate](#remediation-supportate)
- [Osservabilita](#osservabilita)
- [Sicurezza](#sicurezza)
- [Alta disponibilita (Leader Election)](#alta-disponibilita-leader-election)
- [RBAC namespace-scoped](#rbac-namespace-scoped)
- [Laboratorio di test](#laboratorio-di-test)
- [Scenari di errore](#scenari-di-errore)
- [Sviluppo](#sviluppo)
- [Comandi di verifica](#comandi-di-verifica)
- [Reset ambiente](#reset-ambiente)

---

## Architettura

```
                    +------------------+
                    |   Kubernetes     |
                    |   API Server     |
                    +--------+---------+
                             |
                  Warning Events (poll ogni N sec)
                             |
                    +--------v---------+
                    |  ai-remediator   |
                    |  (Go agent)      |
                    +--------+---------+
                             |
                  Prompt JSON strutturato
                             |
                    +--------v---------+
                    |     Ollama       |
                    |  (LLM locale)    |
                    +--------+---------+
                             |
                  Decision JSON (action, confidence, params)
                             |
                    +--------v---------+
                    |  ai-remediator   |
                    |  Execution Engine|
                    +------------------+
                             |
              Remediation (restart, delete, scale, ...)
```

**Flusso operativo:**

1. Kubernetes genera un evento `Warning` (CrashLoopBackOff, ImagePullBackOff, ecc.)
2. L'agente elenca gli eventi via API e filtra quelli di tipo Warning non ancora processati
3. Per ogni evento costruisce un prompt con namespace, kind, name, reason, message e uno snapshot del Deployment associato
4. Invia il prompt a Ollama con uno schema JSON che vincola la risposta
5. Riceve una decisione strutturata con action, confidence, parameters
6. Valida la decisione: allowlist, policy bounds, OCI image format, confidence threshold
7. Esegue l'azione (o logga in dry-run)

Quando l'evento riguarda un Pod, l'agente risale al Deployment tramite `ownerReferences` (Pod -> ReplicaSet -> Deployment).

---

## Struttura del progetto

```
k8s-ai-remediator/
├── cmd/
│   └── agent/
│       ├── main.go              # Bootstrap, signal handling, leader election, event loop
│       └── main_test.go         # Test di integrazione per executeDecision
├── internal/
│   ├── model/
│   │   └── model.go            # Tipi condivisi: Action, Decision, ChatRequest/Response
│   ├── config/
│   │   ├── config.go           # AgentConfig, parsing variabili d'ambiente
│   │   └── config_test.go
│   ├── ollama/
│   │   ├── client.go           # Client HTTP con rate limiting, retry, TLS
│   │   └── client_test.go
│   ├── kube/
│   │   ├── kube.go             # Operazioni Kubernetes (resolve, remediate, logs, snapshot)
│   │   └── kube_test.go
│   ├── policy/
│   │   ├── policy.go           # Allowlist, validazione OCI, sanitizzazione prompt
│   │   └── policy_test.go
│   └── metrics/
│       ├── metrics.go          # Metriche Prometheus-compatible (zero dipendenze esterne)
│       └── metrics_test.go
├── deploy/
│   └── rbac-namespaced.yaml    # RBAC namespace-scoped di esempio
├── .github/
│   └── workflows/
│       └── ci.yml              # CI/CD: lint, test, build, Docker, security scan
├── Dockerfile                   # Multi-stage build (distroless, non-root)
├── go.mod
└── go.sum
```

### Package interni

| Package | Responsabilita |
|---------|---------------|
| `internal/model` | Tipi condivisi tra tutti i package: costanti Action, struct Decision, tipi Ollama API |
| `internal/config` | `AgentConfig` con tutti i parametri e helper per parsing env var con default |
| `internal/ollama` | Client HTTP per Ollama con rate limiting (`golang.org/x/time/rate`), retry con exponential backoff, supporto TLS |
| `internal/kube` | Tutte le operazioni Kubernetes: risoluzione Pod->Deployment, restart, delete, scale, set image, log inspection, snapshot |
| `internal/policy` | Allowlist delle azioni, validazione OCI image, blocco image update unsafe, sanitizzazione prompt anti-injection |
| `internal/metrics` | Metriche in formato Prometheus text exposition, zero dipendenze esterne |

---

## Prerequisiti

- Cluster Kubernetes funzionante (Docker Desktop, minikube, kind, k3s, EKS, GKE, AKS, ...)
- `kubectl` configurato sul cluster corretto
- Docker per la build dell'immagine
- Go 1.21+ per sviluppo locale (opzionale, la build avviene in Docker)

---

## Build

### Build e push sul registry locale

L'immagine viene sempre pubblicata su un registry locale raggiungibile
tramite `host.docker.internal:5050` (il container `registry` mappa la porta
host 5050 sulla 5000 interna). Usare `host.docker.internal` al posto di
`localhost` fa si che lo stesso tag funzioni sia per il `docker push` dall'host
sia per il pull del kubelet dentro al cluster, evitando `ErrImageNeverPull`
su runtime diversi da Docker (containerd, CRI-O) o su cluster multi-node.

```bash
# Avvia il registry locale (una volta sola)
docker run -d --restart=always -p 5050:5000 --name registry registry:2

# Build e push
REGISTRY=host.docker.internal:5050
IMAGE=$REGISTRY/ai-remediator:0.2.0
docker build -t "$IMAGE" .
docker push "$IMAGE"
```

Il Dockerfile usa un multi-stage build:
- **Stage 1**: Go 1.26.1 compila un binary statico (`CGO_ENABLED=0`)
- **Stage 2**: `gcr.io/distroless/static:nonroot` come base (nessuna shell, utente non-root)

> **Nota su Linux**: `host.docker.internal` e disponibile di default con
> Docker Desktop su macOS/Windows e nelle versioni recenti su Linux. Se non
> risolve, aggiungi `--add-host=host.docker.internal:host-gateway` al
> comando docker oppure usa l'IP del gateway del bridge Docker.
> Su kind puoi in alternativa connettere il registry alla network kind
> (`docker network connect kind registry`) e usare `kind-registry:5000`.
> Su minikube: `minikube addons enable registry` o `host.minikube.internal:5050`.

### Build locale (opzionale)

```bash
go mod tidy
CGO_ENABLED=0 go build -o agent ./cmd/agent
```

---

## Installazione su Kubernetes

### 1. Installazione di Ollama

```bash
# Crea il namespace
kubectl create namespace ollama

# Crea il deployment
kubectl -n ollama create deployment ollama \
  --image=ollama/ollama:latest \
  --port=11434 \
  --replicas=1

# Configura host, risorse e storage
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

# Esponi il service
kubectl -n ollama expose deployment ollama \
  --name=ollama \
  --port=11434 \
  --target-port=11434 \
  --type=ClusterIP

# Attendi il rollout e installa il modello
kubectl -n ollama rollout status deployment/ollama --timeout=180s
kubectl -n ollama exec -it deploy/ollama -- ollama pull qwen2.5:14b
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

> **Nota**: il valore di `OLLAMA_MODEL` nella ConfigMap deve coincidere esattamente con il nome mostrato da `ollama list`.
>
> **Requisiti nodo Ollama**: `qwen2.5:14b` richiede almeno **8GB di RAM libera** per l'inferenza. Se il pod resta in `Pending`, verifica che il nodo abbia risorse sufficienti con `kubectl describe node`. Per cluster locali:
> - **Docker Desktop**: Settings → Resources → assegna almeno **10GB di RAM** e **4 CPU**, poi Apply & Restart
> - **minikube**: `minikube start --memory=10240 --cpus=4`
> - **kind**: configura le risorse del container Docker sottostante

### 2. Installazione dell'agente

```bash
# Crea il namespace
kubectl create namespace ai-remediator

# Crea il service account
kubectl create serviceaccount ai-remediator -n ai-remediator

# Crea il ClusterRole
kubectl create clusterrole ai-remediator \
  --verb=get,list,watch,delete \
  --resource=pods,pods/log,events,namespaces

# Aggiungi le regole per deployments e replicasets
kubectl patch clusterrole ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{
    "apiGroups":["apps"],
    "resources":["deployments","replicasets"],
    "verbs":["get","list","watch","update","patch"]
  }}
]'

# Crea il ClusterRoleBinding
kubectl create clusterrolebinding ai-remediator \
  --clusterrole=ai-remediator \
  --serviceaccount=ai-remediator:ai-remediator

# Crea la ConfigMap
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

# Crea il deployment usando l'immagine del registry locale
kubectl -n ai-remediator create deployment ai-remediator \
  --image=host.docker.internal:5050/ai-remediator:0.2.0

# Collega service account, ConfigMap e porta metrics
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

# Verifica il rollout
kubectl -n ai-remediator rollout status deployment/ai-remediator --timeout=180s
kubectl -n ai-remediator logs deploy/ai-remediator --tail=20
```

> **Nota**: `host.docker.internal:5050` presuppone che il kubelet possa
> risolvere `host.docker.internal` verso il gateway dell'host (default su
> Docker Desktop). Su kind/minikube adatta l'hostname come descritto nella
> sezione [Build](#build).

---

## Configurazione

Tutte le variabili sono lette da environment (tipicamente via ConfigMap).

### Variabili principali

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `OLLAMA_BASE_URL` | `http://ollama.ollama.svc.cluster.local:11434/api` | URL base dell'API Ollama |
| `OLLAMA_MODEL` | `qwen2.5:14b` | Nome del modello LLM (deve corrispondere a `ollama list`) |
| `DRY_RUN` | `false` | Se `true`, logga le decisioni senza applicare remediation |
| `POLL_INTERVAL_SECONDS` | `30` | Intervallo di polling degli eventi (secondi) |
| `DEDUPE_TTL_SECONDS` | `300` | TTL di deduplicazione per `(ns, kind, name, reason)`: eventi identici entro la finestra non generano una nuova chiamata LLM |
| `MAX_EVENTS_PER_POLL` | `10` | Numero massimo di eventi che attivano una chiamata LLM per ciclo di polling; gli eccessi sono rinviati al polling successivo |

### Variabili di policy

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `SCALE_MIN` | `1` | Minimo numero di repliche consentite per `scale_deployment` |
| `SCALE_MAX` | `5` | Massimo numero di repliche consentite |
| `ALLOW_IMAGE_UPDATES` | `false` | Abilita l'azione `set_deployment_image` |
| `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` | `0.92` | Confidenza minima per aggiornare un'immagine |

### Variabili Ollama (resilienza)

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `OLLAMA_RPS` | `2.0` | Max richieste al secondo verso Ollama (rate limiting) |
| `OLLAMA_MAX_RETRIES` | `3` | Tentativi per errori transitori (5xx, rete) con backoff esponenziale |
| `OLLAMA_TLS_SKIP_VERIFY` | `false` | Salta la verifica TLS (per certificati self-signed) |
| `POD_LOG_TAIL_LINES` | `200` | Numero di righe di log lette per container |

### Variabili di osservabilita

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `METRICS_ADDR` | `:9090` | Indirizzo di ascolto per `/metrics` e `/healthz` |

### Variabili di alta disponibilita

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `LEADER_ELECTION` | `false` | Abilita la leader election per deploy multi-replica |
| `LEASE_NAME` | `ai-remediator-leader` | Nome della risorsa Lease |
| `LEASE_NAMESPACE` | `ai-remediator` | Namespace della risorsa Lease |

---

## Remediation supportate

| Azione | Tipo | Descrizione |
|--------|------|-------------|
| `noop` | Passiva | Nessuna azione, la decisione viene solo loggata |
| `ask_human` | Passiva | Segnala che serve intervento manuale |
| `mark_for_manual_fix` | Passiva | Marca la risorsa come non risolvibile automaticamente |
| `inspect_pod_logs` | Read-only | Legge i log correnti e precedenti del container con piu restart |
| `restart_deployment` | Mutazione | Forza un rollout aggiornando l'annotazione del pod template |
| `delete_failed_pod` | Mutazione | Elimina il pod, il controller lo ricrea |
| `delete_and_recreate_pod` | Mutazione | Come sopra, usato quando il pod va ricreato da zero |
| `scale_deployment` | Mutazione | Aggiorna `spec.replicas` entro i limiti `SCALE_MIN`/`SCALE_MAX` |
| `set_deployment_image` | Mutazione | Aggiorna l'immagine del container (richiede `ALLOW_IMAGE_UPDATES=true`, confidenza sopra soglia, immagine OCI valida) |

### Logica di selezione del container per i log

Nei pod multi-container, `inspect_pod_logs` seleziona il container:
1. Usa il container specificato nei parametri, se presente e valido
2. Altrimenti sceglie il container con il maggior numero di restart
3. Come fallback, usa il primo container nella spec

---

## Osservabilita

### Endpoint HTTP

L'agente espone due endpoint HTTP sulla porta configurata in `METRICS_ADDR` (default `:9090`):

| Endpoint | Descrizione |
|----------|-------------|
| `/metrics` | Metriche in formato Prometheus text exposition |
| `/healthz` | Health check (ritorna `200 OK`) |

### Metriche esposte

| Metrica | Tipo | Descrizione |
|---------|------|-------------|
| `remediator_events_processed_total` | Counter | Totale eventi Warning processati |
| `remediator_events_skipped_total` | Gauge | Eventi saltati (dedup o non-Warning) |
| `remediator_decisions_total{action}` | Counter | Decisioni per tipo di azione |
| `remediator_decision_errors_total` | Counter | Errori nella chiamata a Ollama |
| `remediator_execution_errors_total` | Counter | Errori nell'esecuzione della remediation |
| `remediator_ollama_requests_total` | Counter | Totale richieste a Ollama |
| `remediator_ollama_errors_total` | Counter | Errori Ollama |
| `remediator_ollama_avg_latency_seconds` | Gauge | Latenza media delle richieste Ollama |
| `remediator_ollama_rate_limited_total` | Counter | Richieste ritardate dal rate limiter |

### Logging strutturato

L'agente produce log in formato JSON (via `log/slog`) su stdout, compatibili con stack di logging Kubernetes (Loki, Fluentd, CloudWatch, ecc.):

```json
{"time":"2026-03-26T10:00:00Z","level":"INFO","msg":"decision","summary":"Pod crash","action":"restart_deployment","ns":"default","kind":"Deployment","name":"web","confidence":0.85}
```

### Configurare il Service per lo scraping Prometheus

```bash
kubectl -n ai-remediator expose deployment ai-remediator \
  --name=ai-remediator-metrics \
  --port=9090 \
  --target-port=9090 \
  --type=ClusterIP

# Se usi Prometheus Operator, aggiungi un ServiceMonitor:
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

## Sicurezza

### Livelli di protezione

1. **Allowlist delle azioni**: solo le 9 azioni predefinite sono accettate; qualsiasi altra viene rifiutata
2. **Dry-run mode**: con `DRY_RUN=true` nessuna modifica viene applicata al cluster
3. **Policy bounds**: `scale_deployment` vincolato a `SCALE_MIN`/`SCALE_MAX`
4. **Confidence threshold**: `set_deployment_image` bloccato sotto la soglia configurata
5. **Validazione OCI**: le immagini dal LLM vengono validate contro il formato OCI standard
6. **Sanitizzazione prompt**: i messaggi degli eventi Kubernetes vengono sanitizzati prima di essere inviati all'LLM (rimozione caratteri di controllo, pattern di prompt injection, troncamento)
7. **Container distroless**: l'immagine non ha shell, file system minimo, utente non-root
8. **Rate limiting**: previene il sovraccarico di Ollama durante storm di eventi

### Protezione da prompt injection

I campi `reason`, `message` e `extra` degli eventi Kubernetes sono sanitizzati prima di entrare nel prompt LLM:
- Caratteri di controllo rimossi (tranne `\n` e `\t`)
- Pattern di injection comuni redatti: "ignore previous instructions", "disregard above", "system:", "forget everything", "new instructions:"
- Campi troncati a 2000 caratteri (500 per reason)

---

## Alta disponibilita (Leader Election)

Per eseguire l'agente con piu repliche in sicurezza:

```bash
# Aggiungi alla ConfigMap
kubectl -n ai-remediator create configmap ai-remediator-config \
  ... \
  --from-literal=LEADER_ELECTION=true \
  --from-literal=LEASE_NAME=ai-remediator-leader \
  --from-literal=LEASE_NAMESPACE=ai-remediator \
  --dry-run=client -o yaml | kubectl apply -f -

# Scala a piu repliche
kubectl -n ai-remediator scale deployment ai-remediator --replicas=2
```

Con leader election abilitata:
- Solo la replica leader esegue il polling loop
- Le altre repliche restano in attesa
- Se il leader muore, un'altra replica prende il suo posto entro ~15 secondi
- Il meccanismo usa `Lease` (risorsa nativa di Kubernetes `coordination.k8s.io`)

Serve aggiungere i permessi per le Lease:

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

## RBAC namespace-scoped

Per limitare l'agente a namespace specifici (invece di cluster-wide), usa i manifest in `deploy/rbac-namespaced.yaml`:

```bash
kubectl apply -f deploy/rbac-namespaced.yaml
```

Questo crea:
- `Role` + `RoleBinding` nel namespace target (es. `incident-lab`)
- `Role` + `RoleBinding` per la leader election nel namespace dell'agente
- `ServiceAccount` nel namespace dell'agente

Per aggiungere altri namespace, duplica le risorse Role/RoleBinding:

```bash
# Crea Role per un nuovo namespace
kubectl -n <nuovo-namespace> create role ai-remediator \
  --verb=get,list,watch,delete \
  --resource=pods,pods/log,events

kubectl -n <nuovo-namespace> patch role ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{
    "apiGroups":["apps"],
    "resources":["deployments","replicasets"],
    "verbs":["get","list","watch","update","patch"]
  }}
]'

kubectl -n <nuovo-namespace> create rolebinding ai-remediator \
  --role=ai-remediator \
  --serviceaccount=ai-remediator:ai-remediator
```

---

## Laboratorio di test

Il laboratorio usa un Deployment con un pod che parte sano ma va in errore quando compare un file in un volume `emptyDir`. Dato che `emptyDir` si azzera quando il pod viene ricreato, `delete_and_recreate_pod` e una remediation verificabile.

### Setup

```bash
# Crea il namespace
kubectl create namespace incident-lab

# Crea il deployment
kubectl -n incident-lab create deployment healable-app \
  --image=busybox:1.36 \
  -- /bin/sh -c 'echo "started"; while true; do if [ -f /state/poison ]; then echo "poison detected"; exit 1; fi; sleep 2; done'

# Aggiungi emptyDir
kubectl -n incident-lab patch deployment healable-app --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/volumes","value":[
    {"name":"state","emptyDir":{}}
  ]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[
    {"name":"state","mountPath":"/state"}
  ]},
  {"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}
]'

# Attendi che il pod sia sano
kubectl -n incident-lab rollout status deployment/healable-app --timeout=120s
```

### Innesco del guasto

```bash
POD=$(kubectl -n incident-lab get pods -o jsonpath='{.items[0].metadata.name}')
kubectl -n incident-lab exec "$POD" -c busybox -- sh -c 'touch /state/poison'
```

Il container trova il file poison, termina, e il pod entra in `CrashLoopBackOff`.

### Osservazione

```bash
# Terminale 1: osserva i pod
kubectl -n incident-lab get pods -w

# Terminale 2: osserva i log dell'agente
kubectl -n ai-remediator logs deploy/ai-remediator -f

# Terminale 3: osserva gli eventi
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp | tail -20
```

L'agente dovrebbe rilevare l'evento Warning, analizzarlo, e decidere una remediation (tipicamente `delete_and_recreate_pod` o `restart_deployment`). Il nuovo pod nasce con un `emptyDir` vuoto e torna sano.

---

## Scenari di errore

La directory `scenarios/` contiene manifest pronti per riprodurre guasti tipici
a quattro livelli di severita. Sono utili per validare il comportamento dell'agente
in condizioni diverse e per stimolare decisioni dell'LLM coerenti con la policy.

| Severita | Manifest | Reason evento | Comportamento atteso |
|----------|----------|---------------|----------------------|
| **Basso** | `scenarios/low-readiness-flaky.yaml` | `Unhealthy` | `noop` / `inspect_pod_logs` |
| **Medio** | `scenarios/medium-imagepullbackoff.yaml` | `Failed`, `ErrImagePull`, `ImagePullBackOff` | `mark_for_manual_fix` / `ask_human` (o `set_deployment_image` se `ALLOW_IMAGE_UPDATES=true` e confidenza sopra soglia) |
| **Critico** | `scenarios/critical-oomkilled.yaml` | `BackOff`, `OOMKilling` | `inspect_pod_logs` / `ask_human` |
| **Grave** | `scenarios/severe-failedscheduling.yaml` | `FailedScheduling` | `mark_for_manual_fix` / `ask_human` |

Tutti gli scenari presuppongono il namespace `incident-lab` gia creato (vedi
[Laboratorio di test](#laboratorio-di-test)).

### Scenario basso — Readiness probe flaky

Il container gira correttamente ma la readiness probe fallisce in modo
intermittente (cicli 20s ready / 10s non-ready). Ogni fallimento produce un
evento Warning `Unhealthy`.

```bash
kubectl apply -f scenarios/low-readiness-flaky.yaml
kubectl -n incident-lab get pods -l scenario=low -w
kubectl -n incident-lab get events --field-selector reason=Unhealthy --sort-by=.metadata.creationTimestamp | tail
```

**Impatto**: solo rumore nei log/eventi e occasionale rimozione dagli endpoints.
Nessun crash, nessun restart.
**Perche basso**: segnale debole che puo indicare una probe troppo stringente
o flakiness da investigare senza urgenza. Una remediation invasiva (restart,
delete) sarebbe una reazione eccessiva.

### Scenario medio — ImagePullBackOff

Un Deployment referenzia un tag immagine inesistente. Il kubelet non riesce a
scaricare l'immagine e genera eventi Warning `Failed` / `ErrImagePull`.

```bash
kubectl apply -f scenarios/medium-imagepullbackoff.yaml
kubectl -n incident-lab get pods -l scenario=medium -w
```

**Impatto**: il workload non parte ma i servizi gia attivi non sono coinvolti.
**Perche medio**: errore localizzato, non degrada il cluster, tipicamente serve
una correzione di configurazione (tag corretto) da parte di un umano.

### Scenario critico — OOMKilled in CrashLoopBackOff

Il container tenta di allocare piu memoria del `limits.memory`. Il kernel lo
uccide con OOMKilled, kubelet lo riavvia, il ciclo si ripete generando eventi
`BackOff` e `OOMKilling`.

```bash
kubectl apply -f scenarios/critical-oomkilled.yaml
kubectl -n incident-lab get pods -l scenario=critical -w
kubectl -n incident-lab describe pod -l scenario=critical | grep -E 'OOMKilled|Reason|Exit'
```

**Impatto**: il servizio e continuamente down, non esiste self-recovery perche
il restart fallisce con lo stesso errore.
**Perche critico**: crash loop permanente, ogni remediation "standard"
(restart, delete) non risolve il problema; l'agente deve riconoscere che serve
un intervento umano sui limiti di risorse.

### Scenario grave — FailedScheduling

Il pod richiede risorse che nessun nodo puo soddisfare (500 CPU, 500 Gi RAM).
Lo scheduler emette ripetutamente `FailedScheduling` e il pod resta in
`Pending` a tempo indeterminato.

```bash
kubectl apply -f scenarios/severe-failedscheduling.yaml
kubectl -n incident-lab get pods -l scenario=severe
kubectl -n incident-lab describe pod -l scenario=severe | grep -A3 Events
```

**Impatto**: blocco permanente del workload, il pod non partira MAI.
**Perche grave**: nessuna azione dell'agente puo risolverlo (ne restart, ne
delete, ne scale, ne set image). Richiede modifica del manifest, scale del
cluster o cambio di profilo hardware.

### Pulizia

```bash
kubectl delete -f scenarios/low-readiness-flaky.yaml --ignore-not-found
kubectl delete -f scenarios/medium-imagepullbackoff.yaml --ignore-not-found
kubectl delete -f scenarios/critical-oomkilled.yaml --ignore-not-found
kubectl delete -f scenarios/severe-failedscheduling.yaml --ignore-not-found
# oppure in blocco
kubectl delete -f scenarios/ --ignore-not-found
```

### Cosa osservare nei log dell'agente

Per ogni scenario puoi correlare severita percepita dall'LLM e azione scelta:

```bash
kubectl -n ai-remediator logs deploy/ai-remediator -f | grep -E 'decision|severity|action'
```

Le metriche Prometheus `remediator_decisions_total{action=...}` mostrano la
distribuzione delle azioni selezionate sui tre scenari.

---

## Sviluppo

### Eseguire i test

```bash
go test ./... -v -count=1
```

I test usano:
- `k8s.io/client-go/kubernetes/fake` per simulare il cluster Kubernetes
- `net/http/httptest` per simulare le risposte di Ollama
- Coprono: config, azioni, policy, sanitizzazione, retry, metriche, OCI validation

### Linting

```bash
golangci-lint run ./...
```

### CI/CD

La pipeline GitHub Actions (`.github/workflows/ci.yml`) esegue automaticamente:
1. **Lint**: `golangci-lint`
2. **Test**: con race detector e copertura
3. **Build**: binary statico linux/amd64
4. **Docker Build**: build dell'immagine container
5. **Security**: scansione vulnerabilita con `govulncheck`

---

## Comandi di verifica

### Stato Ollama

```bash
kubectl -n ollama get pods,svc
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

### Stato agente

```bash
kubectl -n ai-remediator get pods
kubectl -n ai-remediator logs deploy/ai-remediator --tail=50
```

### Metriche

```bash
kubectl -n ai-remediator port-forward deploy/ai-remediator 9090:9090
curl http://localhost:9090/metrics
curl http://localhost:9090/healthz
```

### Health check dei permessi

```bash
kubectl auth can-i get pods/log \
  --as=system:serviceaccount:ai-remediator:ai-remediator \
  -n incident-lab

kubectl auth can-i update deployments \
  --as=system:serviceaccount:ai-remediator:ai-remediator \
  -n incident-lab
```

### Aggiornamento ConfigMap

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

# Restart per applicare le modifiche
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```

---

## Reset ambiente

### Solo laboratorio

```bash
kubectl delete namespace incident-lab --ignore-not-found
```

### Reset completo

```bash
kubectl delete namespace incident-lab --ignore-not-found
kubectl delete namespace ai-remediator --ignore-not-found
kubectl delete namespace ollama --ignore-not-found
kubectl delete clusterrolebinding ai-remediator --ignore-not-found
kubectl delete clusterrole ai-remediator --ignore-not-found
```
