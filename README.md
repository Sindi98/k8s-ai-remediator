# k8s-ai-remediator

Agente AI in Go per osservare eventi Kubernetes di tipo `Warning` e tentare remediation controllate usando Ollama esposto nel cluster. [web:37][page:1]  
L’agente usa l’endpoint Ollama `/api/chat` e richiede output JSON strutturati tramite `format`, in modo da ricevere decisioni vincolate a uno schema noto. [page:1][page:2][web:29][web:30]  
La procedura seguente usa solo comandi `kubectl` per creare namespace, deployment, RBAC, ConfigMap e laboratorio di test, senza applicare YAML al cluster. [web:219][web:120][web:317][web:319]

---

## Indice

- [Architettura](#architettura)
- [Prerequisiti](#prerequisiti)
- [Struttura locale](#struttura-locale)
- [File locali](#file-locali)
- [Installazione di Ollama](#installazione-di-ollama)
- [Installazione dellagente](#installazione-dellagente)
- [Configurazione runtime](#configurazione-runtime)
- [Laboratorio di test](#laboratorio-di-test)
- [Comandi di verifica](#comandi-di-verifica)
- [Reset ambiente](#reset-ambiente)

---

## Architettura

Componenti principali:

1. **Ollama** nel namespace `ollama`. [page:1]
2. **Agente Go** nel namespace `ai-remediator`. [web:312][web:319]
3. **Laboratorio** nel namespace `incident-lab`. [web:219]

Flusso operativo:

1. Kubernetes genera un evento `Warning`. [web:37]
2. L’agente legge l’evento dalle API del cluster. [web:37]
3. L’agente costruisce un prompt con `namespace`, `kind`, `name`, `reason` e `message`. [web:37]
4. L’agente chiama Ollama su `http://ollama.ollama.svc.cluster.local:11434/api/chat`. [page:1]
5. Ollama restituisce JSON strutturato conforme allo schema richiesto. [page:2][web:29][web:30]
6. L’agente applica solo remediation presenti in una allowlist locale. [page:2]

Quando l’evento riguarda un `Pod`, l’agente può risalire al `Deployment` passando da `Pod` a `ReplicaSet` e poi a `Deployment` tramite `ownerReferences`, che è il meccanismo standard di relazione tra risorse Kubernetes. [web:395]  
Questo consente remediation come `restart_deployment` o `delete_and_recreate_pod` su workload gestiti da controller. [web:421][web:394][web:382]

---

## Prerequisiti

Requisiti locali:

- Cluster Kubernetes funzionante. [web:219]
- `kubectl` configurato sul cluster corretto. [web:219]
- Docker disponibile localmente. [web:278]
- Go installato localmente. [web:301]

Questa guida assume un contesto locale o comunque un cluster dove puoi usare immagini buildate localmente per l’agente. [web:278]  
Se l’immagine dell’agente esiste solo in locale, è corretto usare `imagePullPolicy: Never` per evitare pull remoti indesiderati. [web:278]

---

## Struttura locale

```text
k8s-ai-remediator/
├── cmd/
│   └── agent/
│       └── main.go
├── Dockerfile
└── go.mod
```

---

## File locali

### `go.mod`

```go
module github.com/tuo-user/k8s-ai-remediator

go 1.26.1

require (
	k8s.io/api v0.34.1
	k8s.io/apimachinery v0.34.1
	k8s.io/client-go v0.34.1
)
```

La direttiva `go` in `go.mod` definisce la versione minima richiesta dal progetto. [web:301]

### `Dockerfile`

```dockerfile
FROM golang:1.26.1 AS build
WORKDIR /src
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/agent ./cmd/agent

FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/agent /agent
ENTRYPOINT ["/agent"]
```

### `cmd/agent/main.go`

Usa il file `main.go` aggiornato con queste caratteristiche:

- chiamata a Ollama via `/api/chat`; [page:1]
- structured outputs con `format` e JSON schema; [page:2][web:29][web:30]
- remediation `noop`, `restart_deployment`, `delete_failed_pod`, `delete_and_recreate_pod`, `scale_deployment`, `inspect_pod_logs`, `set_deployment_image`, `mark_for_manual_fix`, `ask_human`; [page:2]
- lettura corretta di `pods/log`; [web:322][web:499]
- selezione automatica del container per i log nei pod multi-container; [web:499][web:491]
- risoluzione `Pod -> ReplicaSet -> Deployment` tramite `ownerReferences`. [web:395]

---

## Installazione di Ollama

### 1. Crea il namespace

```bash
kubectl create namespace ollama
```

La creazione esplicita del namespace prima delle altre risorse è la procedura corretta con `kubectl create namespace`. [web:219]

### 2. Crea il deployment

```bash
kubectl -n ollama create deployment ollama \
  --image=ollama/ollama:latest \
  --port=11434 \
  --replicas=1
```

`kubectl create deployment` supporta la creazione del deployment direttamente da CLI. [web:120]

### 3. Configura host, risorse e storage per i modelli

```bash
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
```

L’uso di `emptyDir` rende lo storage dei modelli legato alla vita del pod, perché il volume esiste finché esiste il pod. [web:150][web:452]

### 4. Esponi il service interno

```bash
kubectl -n ollama expose deployment ollama \
  --name=ollama \
  --port=11434 \
  --target-port=11434 \
  --type=ClusterIP
```

### 5. Attendi il rollout

```bash
kubectl -n ollama rollout status deployment/ollama --timeout=180s
kubectl -n ollama get pods,svc
```

`kubectl rollout status` è il comando corretto per verificare il completamento della revisione corrente del deployment. [web:354]

### 6. Installa il modello

```bash
kubectl -n ollama exec -it deploy/ollama -- ollama pull gemma3
kubectl -n ollama exec -it deploy/ollama -- ollama list
```

L’agente deve usare esattamente il nome del modello presente in `ollama list`, perché Ollama risponde con errore `model not found` quando il nome configurato non corrisponde a un modello installato. [web:620][web:622][web:621]

---

## Installazione dell'agente

### 1. Crea il namespace

```bash
kubectl create namespace ai-remediator
```

### 2. Build locale dell’immagine

```bash
go mod tidy
docker build -t ai-remediator:0.1.1 .
```

### 3. Crea il service account

```bash
kubectl create serviceaccount ai-remediator -n ai-remediator
```

`kubectl create serviceaccount` è il comando corretto per creare il service account nel namespace dell’agente. [web:312]

### 4. Crea il ClusterRole di base

```bash
kubectl create clusterrole ai-remediator \
  --verb=get,list,watch,delete \
  --resource=pods,pods/log,events,namespaces
```

La sottorisorsa `pods/log` va autorizzata esplicitamente, perché non è implicita nel permesso su `pods`. [web:322][web:499]

### 5. Aggiungi le regole per `deployments` e `replicasets`

```bash
kubectl patch clusterrole ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{
    "apiGroups":["apps"],
    "resources":["deployments","replicasets"],
    "verbs":["get","list","watch","update","patch"]
  }}
]'
```

`kubectl patch` è il comando corretto per aggiornare una risorsa API in-place senza applicare manifest YAML. [web:475][web:477]

### 6. Crea il ClusterRoleBinding

```bash
kubectl create clusterrolebinding ai-remediator \
  --clusterrole=ai-remediator \
  --serviceaccount=ai-remediator:ai-remediator
```

Il binding tra `ClusterRole` e `ServiceAccount` avviene correttamente con `kubectl create clusterrolebinding`. [web:319][web:605]

### 7. Crea la ConfigMap di runtime

```bash
kubectl create configmap ai-remediator-config \
  -n ai-remediator \
  --from-literal=OLLAMA_BASE_URL=http://ollama.ollama.svc.cluster.local:11434/api \
  --from-literal=OLLAMA_MODEL=gemma3 \
  --from-literal=DRY_RUN=false \
  --from-literal=SCALE_MIN=1 \
  --from-literal=SCALE_MAX=5 \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=ALLOW_IMAGE_UPDATES=false \
  --from-literal=IMAGE_UPDATE_CONFIDENCE_THRESHOLD=0.92 \
  --from-literal=POD_LOG_TAIL_LINES=200
```

`kubectl create configmap --from-literal` è la modalità corretta per creare la configurazione da linea di comando. [web:317]

### 8. Crea il deployment dell’agente

```bash
kubectl -n ai-remediator create deployment ai-remediator \
  --image=ai-remediator:0.1.1
```

### 9. Collega service account, ConfigMap e immagine locale

```bash
kubectl -n ai-remediator patch deployment ai-remediator --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/serviceAccountName","value":"ai-remediator"},
  {"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"},
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom","value":[
    {"configMapRef":{"name":"ai-remediator-config"}}
  ]}
]'
```

Usare `imagePullPolicy: Never` è corretto quando l’immagine dell’agente esiste solo localmente. [web:278]

### 10. Verifica il rollout

```bash
kubectl -n ai-remediator rollout status deployment/ai-remediator --timeout=180s
kubectl -n ai-remediator get pods
kubectl -n ai-remediator logs deploy/ai-remediator --tail=50
```

### 11. Verifica i permessi effettivi del service account

```bash
kubectl auth can-i get pods/log \
  --as=system:serviceaccount:ai-remediator:ai-remediator \
  -n incident-lab

kubectl auth can-i get pods/log \
  --as=system:serviceaccount:ai-remediator:ai-remediator \
  -n default
```

---

## Configurazione runtime

Variabili usate dall’agente:

| Variabile | Valore consigliato |
|---|---|
| `OLLAMA_BASE_URL` | `http://ollama.ollama.svc.cluster.local:11434/api` [page:1] |
| `OLLAMA_MODEL` | `gemma3` oppure il nome esatto mostrato da `ollama list` [web:620][web:621] |
| `DRY_RUN` | `false` |
| `SCALE_MIN` | `1` |
| `SCALE_MAX` | `5` |
| `POLL_INTERVAL_SECONDS` | `30` |
| `ALLOW_IMAGE_UPDATES` | `false` |
| `IMAGE_UPDATE_CONFIDENCE_THRESHOLD` | `0.92` |
| `POD_LOG_TAIL_LINES` | `200` |

Con `DRY_RUN=false`, l’agente prova ad applicare remediation reali invece di limitarsi a loggare la decisione. [page:2]  
Il valore di `OLLAMA_MODEL` deve coincidere con il nome reale del modello installato in Ollama. [web:620][web:622]

---

## Remediation implementate

Le azioni supportate dall’agente sono:

- `noop`
- `restart_deployment`
- `delete_failed_pod`
- `delete_and_recreate_pod`
- `scale_deployment`
- `inspect_pod_logs`
- `set_deployment_image`
- `mark_for_manual_fix`
- `ask_human`

### `inspect_pod_logs`

Legge i log del pod, inclusi i log `previous` del container selezionato, che sono particolarmente utili nei casi di `CrashLoopBackOff`. [web:499][web:496]

### `delete_and_recreate_pod`

Elimina il pod quando la risorsa è un pod gestito da controller, lasciando che Kubernetes lo ricrei automaticamente. [web:421][web:394]

### `restart_deployment`

Forza un nuovo rollout del deployment aggiornando il pod template, che è il principio operativo dietro il restart di un deployment. [web:382][web:386]

### `scale_deployment`

Aggiorna `spec.replicas` del deployment entro i limiti consentiti dalla policy locale. [page:2]

### `set_deployment_image`

Aggiorna l’immagine di un container del deployment solo quando la policy locale lo consente e la decisione include un’immagine concreta. [page:2]

### `mark_for_manual_fix`

Segnala che il caso richiede intervento umano, per esempio quando un `ImagePullBackOff` non è risolvibile in sicurezza partendo dal solo evento. [web:412][web:417]

---

## Laboratorio di test

Il laboratorio usa un `Deployment` con un pod che parte sano, ma va in errore quando compare un file in un volume `emptyDir`. [web:150][web:452]  
Questa simulazione è utile perché `emptyDir` sopravvive ai restart del container nello stesso pod ma si azzera quando il pod viene ricreato, quindi `delete_and_recreate_pod` è una remediation verificabile. [web:150][web:452][web:421]

### 1. Crea il namespace di test

```bash
kubectl delete namespace incident-lab --ignore-not-found
kubectl create namespace incident-lab
```

### 2. Assicurati che l’immagine di test sia disponibile

```bash
docker pull busybox:1.36
docker images | grep busybox
```

### 3. Crea il deployment del laboratorio

```bash
kubectl -n incident-lab create deployment healable-app \
  --image=busybox:1.36 \
  -- /bin/sh -c 'echo "started"; while true; do if [ -f /state/poison ]; then echo "poison detected"; exit 1; fi; sleep 2; done'
```

### 4. Aggiungi `emptyDir` e `imagePullPolicy`

```bash
kubectl -n incident-lab patch deployment healable-app --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/volumes","value":[
    {"name":"state","emptyDir":{}}
  ]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[
    {"name":"state","mountPath":"/state"}
  ]},
  {"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}
]'
```

### 5. Attendi il pod sano

```bash
kubectl -n incident-lab rollout status deployment/healable-app --timeout=120s
kubectl -n incident-lab get pods
```

### 6. Recupera il nome del pod e del container

```bash
POD=$(kubectl -n incident-lab get pods -o jsonpath='{.items.metadata.name}')
echo "$POD"

kubectl -n incident-lab get pod "$POD" -o jsonpath='{.spec.containers[*].name}'; echo
```

Nei pod multi-container, `kubectl logs` e `kubectl exec` richiedono il nome corretto del container. [web:499][web:491][web:506]

### 7. Innesca il guasto

```bash
kubectl -n incident-lab exec "$POD" -c busybox -- sh -c 'touch /state/poison && ls -l /state'
```

Dopo pochi secondi il processo principale trova il file e termina, il container viene riavviato nello stesso pod e il pod tende a entrare in `CrashLoopBackOff`. [web:150][web:452]

### 8. Osserva il laboratorio

Terminale 1:

```bash
kubectl -n incident-lab get pods -w
```

Terminale 2:

```bash
kubectl -n incident-lab get events --sort-by=.metadata.creationTimestamp | tail -n 20
kubectl -n ai-remediator logs deploy/ai-remediator -f
```

### 9. Verifica manuale della remediation corretta

```bash
kubectl -n incident-lab delete pod "$POD"
kubectl -n incident-lab get pods -w
```

Se il pod viene eliminato, il `Deployment` lo ricrea automaticamente per mantenere il numero desiderato di repliche. [web:421][web:394]  
Nel laboratorio questo nuovo pod torna pulito perché nasce con un nuovo `emptyDir` senza il file `poison`. [web:150][web:452]

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
kubectl -n ai-remediator get deployment ai-remediator -o jsonpath='{.spec.template.spec.containers.image}'; echo
kubectl -n ai-remediator logs deploy/ai-remediator --tail=100
```

### Stato laboratorio

```bash
kubectl -n incident-lab get pods
kubectl -n incident-lab describe pod "$POD"
kubectl -n incident-lab logs "$POD" -c busybox --tail=50
```

### Aggiornamento ConfigMap senza YAML applicato al cluster

```bash
kubectl -n ai-remediator create configmap ai-remediator-config \
  --from-literal=OLLAMA_BASE_URL=http://ollama.ollama.svc.cluster.local:11434/api \
  --from-literal=OLLAMA_MODEL=gemma3 \
  --from-literal=DRY_RUN=false \
  --from-literal=SCALE_MIN=1 \
  --from-literal=SCALE_MAX=5 \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=ALLOW_IMAGE_UPDATES=false \
  --from-literal=IMAGE_UPDATE_CONFIDENCE_THRESHOLD=0.92 \
  --from-literal=POD_LOG_TAIL_LINES=200 \
  --dry-run=client -o yaml | kubectl apply -f -
```

### Restart dell’agente dopo aggiornamenti

```bash
kubectl -n ai-remediator rollout restart deployment/ai-remediator
kubectl -n ai-remediator rollout status deployment/ai-remediator --timeout=180s
```

`kubectl rollout restart` è la procedura standard per forzare una nuova revisione del deployment. [web:386][web:383]

---

## Reset ambiente

### Reset solo laboratorio

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

---

## Sequenza minima completa

Per replicare rapidamente l’ambiente in un altro cluster:

```bash
kubectl create namespace ollama
kubectl -n ollama create deployment ollama --image=ollama/ollama:latest --port=11434 --replicas=1
kubectl -n ollama patch deployment ollama --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/env","value":[{"name":"OLLAMA_HOST","value":"0.0.0.0:11434"}]},
  {"op":"add","path":"/spec/template/spec/containers/0/resources","value":{"requests":{"cpu":"2","memory":"4Gi"},"limits":{"cpu":"4","memory":"8Gi"}}},
  {"op":"add","path":"/spec/template/spec/volumes","value":[{"name":"ollama-data","emptyDir":{}}]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"ollama-data","mountPath":"/root/.ollama"}]}
]'
kubectl -n ollama expose deployment ollama --name=ollama --port=11434 --target-port=11434 --type=ClusterIP
kubectl -n ollama rollout status deployment/ollama --timeout=180s
kubectl -n ollama exec -it deploy/ollama -- ollama pull gemma3

kubectl create namespace ai-remediator
kubectl create serviceaccount ai-remediator -n ai-remediator
kubectl create clusterrole ai-remediator --verb=get,list,watch,delete --resource=pods,pods/log,events,namespaces
kubectl patch clusterrole ai-remediator --type='json' -p='[
  {"op":"add","path":"/rules/-","value":{"apiGroups":["apps"],"resources":["deployments","replicasets"],"verbs":["get","list","watch","update","patch"]}}
]'
kubectl create clusterrolebinding ai-remediator --clusterrole=ai-remediator --serviceaccount=ai-remediator:ai-remediator
kubectl create configmap ai-remediator-config -n ai-remediator \
  --from-literal=OLLAMA_BASE_URL=http://ollama.ollama.svc.cluster.local:11434/api \
  --from-literal=OLLAMA_MODEL=gemma3 \
  --from-literal=DRY_RUN=false \
  --from-literal=SCALE_MIN=1 \
  --from-literal=SCALE_MAX=5 \
  --from-literal=POLL_INTERVAL_SECONDS=30 \
  --from-literal=ALLOW_IMAGE_UPDATES=false \
  --from-literal=IMAGE_UPDATE_CONFIDENCE_THRESHOLD=0.92 \
  --from-literal=POD_LOG_TAIL_LINES=200
kubectl -n ai-remediator create deployment ai-remediator --image=ai-remediator:0.1.1
kubectl -n ai-remediator patch deployment ai-remediator --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/serviceAccountName","value":"ai-remediator"},
  {"op":"add","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Never"},
  {"op":"add","path":"/spec/template/spec/containers/0/envFrom","value":[{"configMapRef":{"name":"ai-remediator-config"}}]}
]'
kubectl -n ai-remediator rollout status deployment/ai-remediator --timeout=180s

kubectl delete namespace incident-lab --ignore-not-found
kubectl create namespace incident-lab
kubectl -n incident-lab create deployment healable-app --image=busybox:1.36 -- /bin/sh -c 'echo "started"; while true; do if [ -f /state/poison ]; then echo "poison detected"; exit 1; fi; sleep 2; done'
kubectl -n incident-lab patch deployment healable-app --type='json' -p='[
  {"op":"add","path":"/spec/template/spec/volumes","value":[{"name":"state","emptyDir":{}}]},
  {"op":"add","path":"/spec/template/spec/containers/0/volumeMounts","value":[{"name":"state","mountPath":"/state"}]},
  {"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"IfNotPresent"}
]'
kubectl -n incident-lab rollout status deployment/healable-app --timeout=120s
```
