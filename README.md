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
- [Notifiche email](#notifiche-email)
- [Osservabilita](#osservabilita)
- [Sicurezza](#sicurezza)
- [Alta disponibilita (Leader Election)](#alta-disponibilita-leader-election)
- [RBAC namespace-scoped](#rbac-namespace-scoped)
- [Laboratorio di test](#laboratorio-di-test)
- [Scenari di errore](#scenari-di-errore)
- [Sviluppo](#sviluppo)
- [Comandi di verifica](#comandi-di-verifica)
- [Troubleshooting](#troubleshooting)
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
# qwen2.5:7b e il default consigliato per CPU (chiamate da 30-90s).
# Se hai GPU o vuoi massima qualita, usa qwen2.5:14b (chiamate 3-6min su CPU).
kubectl -n ollama exec -it deploy/ollama -- ollama pull qwen2.5:7b
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

> **Nota**: il valore di `OLLAMA_MODEL` nella ConfigMap deve coincidere esattamente con il nome mostrato da `ollama list`.
>
> **Scelta del modello e requisiti di RAM**:
> - `qwen2.5:7b` (consigliato per CPU): ~4 GB RAM libera, chiamate ~30-90s. Qualita piu che sufficiente perche il prompt e strutturato come decision tree con few-shot examples.
> - `qwen2.5:14b`: ~8 GB RAM libera, chiamate ~3-6 min su CPU. Qualita superiore; adatto se hai GPU o timeout estesi.
>
> Se il pod resta in `Pending`, verifica che il nodo abbia risorse sufficienti con `kubectl describe node`. Per cluster locali:
> - **Docker Desktop**: Settings → Resources → assegna almeno **6 GB RAM / 4 CPU** (14b: 10 GB), poi Apply & Restart
> - **minikube**: `minikube start --memory=6144 --cpus=4` (14b: `--memory=10240`)
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

# Crea la ConfigMap (set completo: auto-fix per probe, resources, registry,
# timeouts estesi per modelli locali lenti, dedup TTL)
kubectl create configmap ai-remediator-config \
  -n ai-remediator \
  --from-literal=OLLAMA_BASE_URL=http://ollama.ollama.svc.cluster.local:11434/api \
  --from-literal=OLLAMA_MODEL=qwen2.5:7b \
  --from-literal=DRY_RUN=false \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=MIN_SEVERITY=low \
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
| `OLLAMA_MODEL` | `qwen2.5:14b` (default di codice). Consigliato per CPU locale: `qwen2.5:7b` | Nome del modello LLM (deve corrispondere a `ollama list`). Con CPU usa `7b`: ~30-90s per chiamata. Con GPU o per alta qualita, `14b`: ~3-6min su CPU |
| `DRY_RUN` | `false` | Se `true`, logga le decisioni senza applicare remediation |
| `POLL_INTERVAL_SECONDS` | `30` | Intervallo di polling degli eventi (secondi) |
| `DEDUPE_TTL_SECONDS` | `300` | TTL di deduplicazione per `(ns, kind, name, reason)`: eventi identici entro la finestra non generano una nuova chiamata LLM |
| `MAX_EVENTS_PER_POLL` | `10` | Numero massimo di eventi che attivano una chiamata LLM per ciclo di polling; gli eccessi sono rinviati al polling successivo |
| `INCLUDE_NAMESPACES` | *(vuoto)* | Allowlist di namespace, comma-separated. Se valorizzato, l'agente reagisce **solo** a eventi di quei namespace. Vuoto = tutti i namespace tranne quelli esclusi |
| `EXCLUDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease,local-path-storage` | Denylist di namespace di sistema. Eventi qui non vengono mai inviati al LLM. Vince sempre sull'allowlist (un namespace listato in entrambi viene escluso) |

### Variabili di policy

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `SCALE_MIN` | `1` | Minimo numero di repliche consentite per `scale_deployment` |
| `SCALE_MAX` | `5` | Massimo numero di repliche consentite |
| `ALLOW_IMAGE_UPDATES` | `false` | Abilita l'azione `set_deployment_image` |
| `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` | `0.92` | Confidenza minima per aggiornare un'immagine |
| `ALLOW_PATCH_PROBE` | `false` | Abilita l'azione `patch_probe` (richiede anche annotation `ai-remediator/allow-patch` con scope `probe`) |
| `ALLOW_PATCH_RESOURCES` | `false` | Abilita l'azione `patch_resources` (scope `resources`) |
| `ALLOW_PATCH_REGISTRY` | `false` | Abilita l'azione `patch_registry` (scope `registry`) |
| `PATCH_CONFIDENCE_THRESHOLD` | `0.85` | Confidenza minima per qualsiasi azione `patch_*` |

### Variabili Ollama (resilienza)

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `OLLAMA_RPS` | `2.0` | Max richieste al secondo verso Ollama (rate limiting) |
| `OLLAMA_MAX_RETRIES` | `3` | Tentativi per errori transitori (5xx, rete) con backoff esponenziale |
| `OLLAMA_TLS_SKIP_VERIFY` | `false` | Salta la verifica TLS (per certificati self-signed) |
| `OLLAMA_HTTP_TIMEOUT_SECONDS` | `180` | Timeout HTTP per ogni richiesta a Ollama (attesa headers + body). Aumenta se vedi `Client.Timeout exceeded while awaiting headers` con modelli lenti (CPU, GPU scarica, cold start) |
| `POLL_CONTEXT_TIMEOUT_SECONDS` | `300` | Timeout del context che avvolge l'intero ciclo di polling (list eventi + chiamate Ollama). Deve restare maggiore di `OLLAMA_HTTP_TIMEOUT_SECONDS` altrimenti il context scade prima del client HTTP e produce `context deadline exceeded` |
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
| `patch_probe` | Mutazione (patch) | Modifica i campi temporali di readiness/liveness probe (`initialDelaySeconds`, `periodSeconds`, `failureThreshold`, `successThreshold`, `timeoutSeconds`). Mai il probe handler |
| `patch_resources` | Mutazione (patch) | Aggiorna `requests`/`limits` di CPU/memoria di un container entro bounds prefissati |
| `patch_registry` | Mutazione (patch) | Riscrive solo il prefisso registry dell'immagine, mantenendo path e tag |

### Patch controllati di Deployment (nuovo)

Le tre azioni `patch_*` permettono all'agente di correggere configurazioni
tipiche (probe troppo stringente, requests/limits inadeguati, registry
sbagliato). Sono tutte disabilitate di default e richiedono un duplice
consenso:

1. **Feature flag globale** via env var (`ALLOW_PATCH_PROBE`, `ALLOW_PATCH_RESOURCES`, `ALLOW_PATCH_REGISTRY`).
2. **Opt-in per Deployment** tramite annotation `ai-remediator/allow-patch: "probe,resources,registry"` (o `"*"` per tutti gli scope).

In piu ogni `patch_*` e bloccato se la confidence dell'LLM e sotto
`PATCH_CONFIDENCE_THRESHOLD` (default `0.85`).

Esempio di opt-in sul Deployment target:

```bash
kubectl -n myns annotate deployment myapp \
  ai-remediator/allow-patch="probe,resources"
```

Parametri attesi dall'LLM per ogni azione:

- `patch_probe`: `deployment_name`, `container`, `probe` (`readiness`|`liveness`), almeno uno tra `initial_delay_seconds`, `period_seconds`, `failure_threshold`, `success_threshold`, `timeout_seconds`.
- `patch_resources`: `deployment_name`, `container`, almeno uno tra `cpu_request`, `memory_request`, `cpu_limit`, `memory_limit` (quantita Kubernetes).
- `patch_registry`: `deployment_name`, `container`, `new_registry` (es. `host.docker.internal:5050`).

I campi numerici delle probe sono validati contro bounds prefissati
(es. `period_seconds` in `[1, 300]`), le quantita di risorse contro
`[10m, 8]` CPU e `[16Mi, 16Gi]` memoria, e i riferimenti immagine
vengono ricostruiti preservando path+tag.

### Logica di selezione del container per i log

Nei pod multi-container, `inspect_pod_logs` seleziona il container:
1. Usa il container specificato nei parametri, se presente e valido
2. Altrimenti sceglie il container con il maggior numero di restart
3. Come fallback, usa il primo container nella spec

---

## Notifiche email

L'agente puo inviare via SMTP una breve email dopo ogni `executeDecision`,
con il quadro "situazione anomala -> decisione presa -> situazione post
intervento". Va bene per cluster di test con carico moderato; non e pensato
per volumi di produzione (un'email per decisione).

Tre pattern supportati senza modifiche al codice:

1. **Self-notification iCloud**: usi il tuo Apple ID sia come mittente che
   come destinatario (ti mandi gli alert a te stesso). Non serve creare
   altri account.
2. **Account bot Gmail** dedicato: un indirizzo separato per l'agente,
   gli alert vanno a un indirizzo operativo.
3. **Servizio transazionale** (Resend / Mailgun / Postmark): cambia solo
   host e credenziali, il codice resta lo stesso.

### Setup con iCloud Mail (pattern 1 o 2)

Genera una **password app-specific** per il tuo Apple ID
(<https://account.apple.com> → Accesso e Sicurezza → Password specifiche
per app). **Importante**: non incollarla in chat o in repository; applicala
solo come Secret sul cluster.

Placeholder usati sotto: sostituisci `alerts-bot@icloud.com` con il tuo
Apple ID reale e `ops-alerts@example.com` con la casella dove vuoi
ricevere gli alert (puo essere lo stesso indirizzo per self-notification).

```bash
# Crea il Secret con le credenziali (password app-specific appena generata)
kubectl -n ai-remediator create secret generic ai-remediator-notify \
  --from-literal=NOTIFY_SMTP_USER='alerts-bot@icloud.com' \
  --from-literal=NOTIFY_SMTP_PASSWORD='xxxx-xxxx-xxxx-xxxx'

# Solo host/porta/destinatari nella ConfigMap (non-segreti)
kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "NOTIFY_SMTP_HOST":"smtp.mail.me.com",
    "NOTIFY_SMTP_PORT":"587",
    "NOTIFY_FROM":"alerts-bot@icloud.com",
    "NOTIFY_TO":"ops-alerts@example.com",
    "NOTIFY_MIN_SEVERITY":"medium"
  }}'

# Monta il Secret come envFrom (si somma alla ConfigMap gia presente)
kubectl -n ai-remediator patch deployment ai-remediator --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom/-","value":
    {"secretRef":{"name":"ai-remediator-notify"}}}
]'

kubectl -n ai-remediator rollout restart deployment/ai-remediator
```

### Setup con Gmail (alternativa)

Crea un account dedicato (es. `ai-remediator-bot@gmail.com`), attiva 2FA,
genera una App Password (<https://myaccount.google.com/apppasswords>) e
cambia solo host e utente:

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

Il Deployment patch e il rollout restart sono identici al caso iCloud.

### Variabili di configurazione

| Variabile | Default | Descrizione |
|-----------|---------|-------------|
| `NOTIFY_SMTP_HOST` | *(vuoto)* | Host SMTP (es. `smtp.mail.me.com`, `smtp.gmail.com`). Vuoto → notifier disattivato (no-op) |
| `NOTIFY_SMTP_PORT` | `587` | Porta SMTP con STARTTLS |
| `NOTIFY_SMTP_USER` | *(vuoto)* | Username SMTP (di solito email completa). Vuoto → notifier disattivato |
| `NOTIFY_SMTP_PASSWORD` | *(vuoto)* | Password app-specific |
| `NOTIFY_FROM` | = `NOTIFY_SMTP_USER` | Mittente visibile nell'email |
| `NOTIFY_TO` | *(vuoto)* | Destinatario. Vuoto → notifier disattivato. Puo coincidere con `NOTIFY_FROM` (self-notification) |
| `NOTIFY_MIN_SEVERITY` | `medium` | Invia email solo per decisioni a severita >= di questa. Valori: `critical`, `high`, `medium`, `low`, `info` |

Se uno qualsiasi tra `HOST`, `USER`, `TO` e vuoto, il notifier non prova
nemmeno a connettersi: il log allo startup mostra `notify: SMTP not
configured, notifications disabled`.

### Tuning della verbosita

| Use case | `NOTIFY_MIN_SEVERITY` | Frequenza tipica |
|----------|-------------------|------------------|
| Solo emergenze (prod) | `critical` | Rara |
| Produzione normale | `high` | Alcune al giorno per cluster sano |
| Test/demo (default) | `medium` | Una ogni scenario attivo |
| Debug verboso | `low` | Fino a una ogni 5 min per ogni `(deployment, reason)` — la dedup TTL rate-limita |
| Tutto | `info` | Include azioni `noop` dell'LLM |

A parita di `NOTIFY_MIN_SEVERITY`, la dedup di segnale (TTL
`DEDUPE_TTL_SECONDS`) limita comunque il traffico: non ricevi piu di una
mail per `(ns, Deployment, reason)` dentro la finestra.

### Verifica setup

```bash
# Dopo il rollout restart, la riga deve riportare tutti i campi valorizzati
kubectl -n ai-remediator logs deploy/ai-remediator --tail=30 \
  | grep 'notify: SMTP configured'
```

Output atteso:
```
notify: SMTP configured host=smtp.mail.me.com port=587 from=alerts-bot@icloud.com to=ops-alerts@example.com minSeverity=medium
```

Se vedi invece `notify: SMTP not configured, notifications disabled`, il
Secret non e stato montato: controlla `envFrom` del Deployment con
`kubectl -n ai-remediator get deployment ai-remediator -o jsonpath='{.spec.template.spec.containers[0].envFrom}'`.

### Corpo dell'email

Plain text con tre blocchi:

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

Se `executeDecision` fallisce (guard o errore Kubernetes), il terzo blocco
mostra `Azione non applicata. Errore: <testo>`. L'invio e fire-and-forget
con timeout di 30s e non blocca il poll loop.

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

1. **Allowlist delle azioni**: solo le 12 azioni predefinite sono accettate; qualsiasi altra viene rifiutata
2. **Dry-run mode**: con `DRY_RUN=true` nessuna modifica viene applicata al cluster
3. **Policy bounds**: `scale_deployment` vincolato a `SCALE_MIN`/`SCALE_MAX`; `patch_resources` ai bounds `[10m, 8]` CPU e `[16Mi, 16Gi]` memoria; `patch_probe` ai bounds per campo (`failureThreshold` 1-20, `periodSeconds` 1-300, ...)
4. **Confidence threshold**: `set_deployment_image` bloccato sotto `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` (default 0.92); le tre azioni `patch_*` sotto `PATCH_CONFIDENCE_THRESHOLD` (default 0.85)
5. **Feature flag per azione rischiosa**: `ALLOW_IMAGE_UPDATES`, `ALLOW_PATCH_PROBE`, `ALLOW_PATCH_RESOURCES`, `ALLOW_PATCH_REGISTRY` — tutte `false` di default
6. **Opt-in per Deployment** per le azioni `patch_*`: richiesta la annotation `ai-remediator/allow-patch` con gli scope (`probe`, `resources`, `registry`, `*`)
7. **Guardie contro remediation controproducenti** (bloccano decisioni dell'LLM che non risolverebbero la causa e sprechero cicli):
   - `restart_deployment` bloccato su eventi `Unhealthy` (probe misconfiguration non si risolve con un restart)
   - `restart_deployment` bloccato se `PodStatusSummary` mostra `OOMKilled` o `exit=137` (memoria insufficiente non si risolve con un restart)
   - `scale_deployment` e `restart_deployment` bloccati su eventi `FailedScheduling` (risorse impossibili non si risolvono scalando)
8. **Validazione OCI**: le immagini dal LLM vengono validate contro il formato OCI standard
9. **Sanitizzazione prompt**: i messaggi degli eventi Kubernetes vengono sanitizzati prima di essere inviati all'LLM (rimozione caratteri di controllo, pattern di prompt injection, troncamento)
10. **Container distroless**: l'immagine non ha shell, file system minimo, utente non-root
11. **Rate limiting**: previene il sovraccarico di Ollama durante storm di eventi
12. **Deduplicazione per segnale**: collassa gli eventi `(ns, Deployment, reason)` in un'unica chiamata LLM per `DEDUPE_TTL_SECONDS` (default 300s); cap `MAX_EVENTS_PER_POLL` (default 10)

### Protezione da prompt injection

I campi `reason`, `message` e `extra` degli eventi Kubernetes sono sanitizzati prima di entrare nel prompt LLM:
- Caratteri di controllo rimossi (tranne `\n` e `\t`)
- Pattern di injection comuni redatti: "ignore previous instructions", "disregard above", "system:", "forget everything", "new instructions:"
- Campi troncati a 2000 caratteri (500 per reason)

### Flusso di decisione

Per ogni evento Warning l'agente segue questo flusso deterministico:

```
Evento Kubernetes (Warning)
        |
        v
Dedup per (ns, Deployment|Pod, reason) con TTL
        |  (skip se gia processato nello stesso TTL)
        v
Cap per poll (MAX_EVENTS_PER_POLL)
        |
        v
Costruzione prompt:
  - evento sanitizzato
  - Deployment snapshot: replicas, containers, Allow-patch scopes, probe timings
  - PodStatusSummary: fase pod, container state, lastTerminated reason/exit
  - HARD RULES specifiche per ogni reason
        |
        v
Chiamata Ollama con response schema JSON
        |  (rate-limited a OLLAMA_RPS, retry su 5xx, timeout OLLAMA_HTTP_TIMEOUT_SECONDS)
        v
Decision validata: allowlist azioni, severity, confidence
        |
        v
Policy guards:
  - MaybeBlockUnsafeImageUpdate       (confidence >= soglia, OCI valida, flag on)
  - MaybeBlockUnsafePatch             (patch_*: flag on, confidence >= soglia)
  - MaybeBlockRestartOnProbeFailure   (event=Unhealthy)
  - MaybeBlockRestartOnOOMKilled      (extra contiene OOMKilled/exit=137)
  - MaybeBlockWrongActionOnFailedScheduling (event=FailedScheduling)
        |
        v
Esecuzione (in dry-run-off) o log-only (in dry-run-on):
  - patch_* leggono la annotation del Deployment per ulteriore opt-in
  - parametri numerici validati contro bounds per campo/risorsa
```

Se il guard rifiuta la decisione, l'agente emette un `execute decision failed` visibile e la dedup TTL rate-limita nuovi tentativi sullo stesso segnale per `DEDUPE_TTL_SECONDS`, evitando loop.

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

Con tutti i flag attivi (`ALLOW_PATCH_PROBE=true`, `ALLOW_PATCH_RESOURCES=true`,
`ALLOW_PATCH_REGISTRY=true`, `ALLOW_IMAGE_UPDATES=true`) **3 scenari su 4** si
risolvono in autonomia; sul quarto l'agente si astiene correttamente.

| Severita | Manifest | Reason evento | Opt-in sul Deployment | Action attesa | Esito |
|----------|----------|---------------|------------------------|----------------|-------|
| **Basso** | `scenarios/low-readiness-flaky.yaml` | `Unhealthy` | `allow-patch: probe` (gia presente) | `patch_probe` | auto-fix |
| **Medio** | `scenarios/medium-imagepullbackoff.yaml` | `ErrImagePull` / `ImagePullBackOff` | — (usa `ALLOW_IMAGE_UPDATES`) | `set_deployment_image` | auto-fix (se il registry raggiunge il tag) |
| **Critico** | `scenarios/critical-oomkilled.yaml` | `BackOff` + stato `OOMKilled` | `allow-patch: resources` (gia presente) | `patch_resources` | auto-fix |
| **Grave** | `scenarios/severe-failedscheduling.yaml` | `FailedScheduling` | `allow-patch: resources` (opzionale) | `patch_resources` o `mark_for_manual_fix` | auto-fix condizionale o astensione |

Dettaglio dei valori prodotti dall'agente (reference con qwen2.5:14b):
vedi [`scenarios/README.md`](scenarios/README.md).

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

# Restart per applicare le modifiche
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```

---

## Troubleshooting

Sintomi comuni e azioni correttive, riferiti al flusso degli scenari.

### Il pod dell'agente gira, ma non vedo mai `decision` sui miei eventi

Possibili cause:
1. **Il binario in esecuzione non ha le feature recenti**. Verifica dalla
   prima riga dopo il restart:
   ```bash
   kubectl -n ai-remediator logs deploy/ai-remediator --tail=30 \
     | grep '"msg":"agent started"' | tail -1 | jq .
   ```
   Deve contenere `"buildFeatures":"dedup,infer-dep-from-podname,..."` e
   valori coerenti per `allowPatchProbe`, `dedupeTTLSec`, eccetera. Se il
   campo manca, rebuild e push dell'immagine + `rollout restart`.
2. **La `ConfigMap` manca di una env var** (es. `MIN_SEVERITY=low` mentre
   gli eventi sono a `medium`): rivedi la sezione "Configurazione".
3. **Dedup TTL attivo**: ogni segnale viene rivalutato solo dopo
   `DEDUPE_TTL_SECONDS` (default 5 min). Aspetta, oppure abbassalo.

### `ollama rate limiter: context deadline exceeded`

La richiesta LLM e stata accodata piu a lungo del `pollCtx`. Tipico di
storm di eventi non deduplicati (pod gia ruotati con nomi differenti)
oppure eventi di sistema che non dovremmo neanche processare.

Verifica:
- `"excludeNamespaces"` allo startup contenga `kube-system`, `kube-public`,
  `kube-node-lease`, `local-path-storage`. Senza il denylist, ogni warning
  di CoreDNS o kube-scheduler arriva al LLM.
- `"dedupeTTLSec"` e `"maxEventsPerPoll"` siano non-zero.
- La inferenza funzioni: `ResolveDeploymentFromPod` + `InferDeploymentFromPodName`
  devono convergere tutti i pod fantasma sullo stesso Deployment.
- In un cluster di test con molti eventi vecchi, aspetta un giro o due: la
  dedup TTL e il cap drenano la coda. Per ridurre ulteriormente il rumore
  passa a `INCLUDE_NAMESPACES=incident-lab` (allowlist stretta).

### `Post "...": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`

Ollama ci mette piu di `OLLAMA_HTTP_TIMEOUT_SECONDS` a rispondere.
Tempi tipici su CPU:
- `qwen2.5:7b`: 30-90s (timeout 180s basta)
- `qwen2.5:14b`: 100-360s (timeout 360-600s)

**Soluzione preferita**: passa al 7b (4x piu veloce):
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

**Alternativa se resti sul 14b**: alza i timeout
```bash
kubectl -n ai-remediator patch configmap ai-remediator-config \
  --type=merge -p '{"data":{
    "OLLAMA_HTTP_TIMEOUT_SECONDS":"600",
    "POLL_CONTEXT_TIMEOUT_SECONDS":"720"
  }}'
kubectl -n ai-remediator rollout restart deployment/ai-remediator
```
Invariante: `POLL_CONTEXT_TIMEOUT_SECONDS > OLLAMA_HTTP_TIMEOUT_SECONDS`.

### `Post "...": context deadline exceeded` (senza `Client.Timeout`)

Qui sta scadendo il `pollCtx`, non il client HTTP. Alza
`POLL_CONTEXT_TIMEOUT_SECONDS` (vedi sopra).

### `execute decision failed` con uno dei seguenti messaggi

| Messaggio | Causa | Rimedio |
|-----------|-------|---------|
| `set_deployment_image disabled by policy` | `ALLOW_IMAGE_UPDATES=false` | Abilitalo nella ConfigMap se vuoi auto-fix del tag. |
| `set_deployment_image blocked: confidence X below threshold Y` | LLM non sufficientemente sicuro | Scendi `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` o aspetta un segnale piu chiaro |
| `set_deployment_image blocked: invalid OCI image reference` | LLM ha inventato un'immagine non valida | Nessuno: rifiuto corretto |
| `patch_probe disabled by policy` | `ALLOW_PATCH_PROBE=false` | Abilita il flag |
| `patch_probe blocked: confidence X below threshold Y` | LLM non sicuro | Scendi `PATCH_CONFIDENCE_THRESHOLD` o attendi nuova evidenza |
| `deployment ... does not opt in to patch_probe (set annotation ai-remediator/allow-patch)` | Manca annotation sul Deployment | `kubectl annotate deployment X ai-remediator/allow-patch=probe` |
| `probe field "failure_threshold" not an integer: parsing "x5"` | LLM ha emesso un moltiplicatore o espressione | Rifiuto corretto. Attendi il prossimo ciclo; il prompt ora richiede esplicitamente integer puri |
| `cpu_request: quantity outside bounds` | Valore fuori `[10m, 8]` CPU o `[16Mi, 16Gi]` memoria | Rifiuto corretto. Alza i bounds in codice o fai manual fix |
| `restart_deployment blocked: event reason=Unhealthy` | L'LLM ha proposto di riavviare un pod con probe flaky | Atteso. Attendi che l'LLM converga su `patch_probe` (se la annotation c'e) |
| `restart_deployment blocked: pod status shows OOMKilled` | L'LLM ha proposto restart su un OOM | Atteso. Attendi `patch_resources` |
| `scale_deployment blocked: event reason=FailedScheduling` | L'LLM ha proposto di scalare un pod che chiede troppe risorse | Atteso. Attendi `patch_resources` o `mark_for_manual_fix` |

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
