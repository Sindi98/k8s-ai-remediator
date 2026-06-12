#!/usr/bin/env bash
# install.sh — set up the whole ai-remediator stack on a Kubernetes cluster,
# cleanly and idempotently, in the correct order:
#
#   1. (optional) build & push the agent image to your registry
#   2. (optional) install Ollama and pull the LLM model
#   3. install Redis (mandatory dedup backend) + its password Secret,
#      or point at an external Redis with --redis-addr
#   4. apply RBAC (cluster-wide ServiceAccount/ClusterRole + leader-election)
#   5. apply the agent (Namespace, ConfigMap, Secret, Deployment, Service)
#   6. (optional) apply the admin GUI + scenarios RBAC
#   7. override image / model / GUI credentials, then roll out and verify
#
# Every step uses `kubectl apply` (or merge patches), so re-running the script
# converges the cluster to the desired state without erroring on existing
# objects. Nothing is destroyed unless you pass --uninstall.
#
# Usage:
#   scripts/install.sh [flags]
#
# Common flags (all also settable via env vars, shown in brackets):
#   --image REF          Agent image to deploy           [IMAGE]
#   --registry HOST      Registry prefix for the image   [REGISTRY]
#   --model NAME         Ollama model to pull/use         [MODEL]
#   --think MODE         Model thinking mode: false|true|auto (default: false;
#                        required "false" for reasoning models like qwen3.x) [OLLAMA_THINK]
#   --ollama-storage SZ  PVC size for Ollama models (/root/.ollama), e.g. 20Gi.
#                        "none" = emptyDir: models re-download on every
#                        Ollama pod restart                            [OLLAMA_STORAGE]
#   --build              Build & push the image first (needs Docker)  [BUILD=true]
#   --skip-ollama        Do not install Ollama / pull the model       [INSTALL_OLLAMA=false]
#   --no-webui           Install the agent without the admin GUI      [ENABLE_WEBUI=false]
#   --webui-user NAME    GUI login username (default: admin)          [WEBUI_USERNAME]
#   --webui-password PW  GUI login password (default: random)         [WEBUI_PASSWORD]
#   --sandbox-ns NS      Namespace allowed to receive fault scenarios [SANDBOX_NS]
#   --redis-addr ADDR    Use an external Redis (host:port); skip the bundled one [REDIS_ADDR]
#   --redis-password PW  Redis password (bundled or external; random if unset)   [REDIS_PASSWORD]
#   --no-scenarios       Skip the scenarios sandbox RBAC              [ENABLE_SCENARIOS=false]
#   --dry-run            Print the actions without changing the cluster
#   --uninstall          Remove everything this script creates
#   -h, --help           Show this help and exit
#
# Examples:
#   # Full local setup (build image, install Ollama, enable GUI):
#   scripts/install.sh --build
#
#   # Cluster that already has Ollama and a pushed image:
#   scripts/install.sh --skip-ollama --image my-registry/k8s-ai-remediator:v1
#
#   # Headless agent, no GUI, watch only one namespace afterwards:
#   scripts/install.sh --no-webui
#
#   # Tear everything down:
#   scripts/install.sh --uninstall

set -euo pipefail

# --- resolve repo paths so the script works from any directory -------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DEPLOY_DIR="${REPO_ROOT}/deploy"

# --- defaults (override via env or flags) ----------------------------------
NAMESPACE="${NAMESPACE:-ai-remediator}"
OLLAMA_NAMESPACE="${OLLAMA_NAMESPACE:-ollama}"
REGISTRY="${REGISTRY:-host.docker.internal:5050}"
IMAGE="${IMAGE:-}"                       # defaults to $REGISTRY/k8s-ai-remediator:latest below
MODEL="${MODEL:-qwen3.5:9b}"
# Thinking mode wired into the agent ConfigMap (OLLAMA_THINK). qwen3.x and
# gemma4 are reasoning models: leaving thinking on makes every LLM call spend
# minutes on CPU and exceed the agent HTTP timeout, so "false" is the default.
# "auto" keeps the Ollama server default; models without the capability are
# detected and handled automatically by the agent.
OLLAMA_THINK="${OLLAMA_THINK:-false}"
# Persistent storage for the Ollama model weights. Without it (value "none",
# the previous emptyDir behaviour) every Ollama pod restart re-downloads
# several GB and decisions time out until the pull completes. Requires a
# default StorageClass, like the Redis PVC already does.
OLLAMA_STORAGE="${OLLAMA_STORAGE:-20Gi}"
SANDBOX_NS="${SANDBOX_NS:-incident-lab}"

# Redis is the (mandatory) dedup backend. Leave REDIS_ADDR empty to install the
# bundled single-instance Redis (deploy/redis.yaml); set it to an existing
# host:port to use an external Redis instead. REDIS_PASSWORD is stored in the
# ai-remediator-redis Secret; a random one is generated when installing the
# bundled Redis without an explicit value.
REDIS_ADDR="${REDIS_ADDR:-}"
REDIS_PASSWORD="${REDIS_PASSWORD:-}"

INSTALL_OLLAMA="${INSTALL_OLLAMA:-true}"
ENABLE_WEBUI="${ENABLE_WEBUI:-true}"
ENABLE_SCENARIOS="${ENABLE_SCENARIOS:-true}"
BUILD="${BUILD:-false}"
DRY_RUN="${DRY_RUN:-false}"
UNINSTALL=false

WEBUI_USERNAME="${WEBUI_USERNAME:-admin}"
WEBUI_PASSWORD="${WEBUI_PASSWORD:-}"     # generated if empty

ROLLOUT_TIMEOUT="${ROLLOUT_TIMEOUT:-180s}"

# --- pretty output ----------------------------------------------------------
if [ -t 1 ]; then
    BOLD=$'\033[1m'; GREEN=$'\033[32m'; YELLOW=$'\033[33m'; RED=$'\033[31m'; DIM=$'\033[2m'; RESET=$'\033[0m'
else
    BOLD=''; GREEN=''; YELLOW=''; RED=''; DIM=''; RESET=''
fi
step() { printf '\n%s==>%s %s%s%s\n' "$GREEN" "$RESET" "$BOLD" "$*" "$RESET"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '%sWARN:%s %s\n' "$YELLOW" "$RESET" "$*" >&2; }
die()  { printf '%sERROR:%s %s\n' "$RED" "$RESET" "$*" >&2; exit 1; }

# run executes a command, or prints it in --dry-run mode.
run() {
    if [ "$DRY_RUN" = true ]; then
        printf '%s    [dry-run] %s%s\n' "$DIM" "$*" "$RESET"
    else
        "$@"
    fi
}

# apply_stdin pipes YAML from stdin to `kubectl apply` (or prints in dry-run).
apply_stdin() {
    if [ "$DRY_RUN" = true ]; then
        printf '%s    [dry-run] kubectl apply -f -  (%s)%s\n' "$DIM" "${1:-stdin}" "$RESET"
        cat >/dev/null
    else
        kubectl apply -f -
    fi
}

have() { command -v "$1" >/dev/null 2>&1; }

# Print the leading comment block (the help header) up to, but not including,
# the `set -euo pipefail` line. Robust to the header growing or shrinking.
usage() { sed -n '2,/^set -euo pipefail/{/^set -euo pipefail/!p;}' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

# --- argument parsing -------------------------------------------------------
while [ "$#" -gt 0 ]; do
    case "$1" in
        --image)            IMAGE="$2"; shift 2 ;;
        --registry)         REGISTRY="$2"; shift 2 ;;
        --model)            MODEL="$2"; shift 2 ;;
        --think)            OLLAMA_THINK="$2"; shift 2 ;;
        --ollama-storage)   OLLAMA_STORAGE="$2"; shift 2 ;;
        --build)            BUILD=true; shift ;;
        --skip-ollama)      INSTALL_OLLAMA=false; shift ;;
        --no-webui)         ENABLE_WEBUI=false; shift ;;
        --webui-user)       WEBUI_USERNAME="$2"; shift 2 ;;
        --webui-password)   WEBUI_PASSWORD="$2"; shift 2 ;;
        --sandbox-ns)       SANDBOX_NS="$2"; shift 2 ;;
        --redis-addr)       REDIS_ADDR="$2"; shift 2 ;;
        --redis-password)   REDIS_PASSWORD="$2"; shift 2 ;;
        --no-scenarios)     ENABLE_SCENARIOS=false; shift ;;
        --dry-run)          DRY_RUN=true; shift ;;
        --uninstall)        UNINSTALL=true; shift ;;
        -h|--help)          usage; exit 0 ;;
        *)                  die "unknown flag: $1 (use --help)" ;;
    esac
done

# Remember whether the user pinned an image explicitly (flag or env): only
# auto-generated references get the unique per-build tag below.
IMAGE_EXPLICIT=false
[ -n "$IMAGE" ] && IMAGE_EXPLICIT=true
IMAGE="${IMAGE:-${REGISTRY}/k8s-ai-remediator:latest}"
BUILD_VERSION=""

case "$OLLAMA_THINK" in
    false|true|auto) ;;
    *) die "--think must be one of: false, true, auto (got: ${OLLAMA_THINK})" ;;
esac

# --- preflight --------------------------------------------------------------
preflight() {
    step "Preflight checks"
    have kubectl || die "kubectl not found in PATH"
    if [ "$DRY_RUN" != true ]; then
        kubectl cluster-info >/dev/null 2>&1 || die "kubectl cannot reach a cluster (check your kubeconfig/context)"
    fi
    [ -f "${DEPLOY_DIR}/agent.yaml" ] || die "deploy/agent.yaml not found — run from a repo checkout"
    [ -f "${DEPLOY_DIR}/rbac-cluster.yaml" ] || die "deploy/rbac-cluster.yaml not found"
    [ -n "$REDIS_ADDR" ] || [ -f "${DEPLOY_DIR}/redis.yaml" ] || die "deploy/redis.yaml not found (needed for the bundled Redis; or pass --redis-addr for an external one)"
    info "kubectl context: $(kubectl config current-context 2>/dev/null || echo '?')"
    info "agent image:     ${IMAGE}"
    info "ollama model:    ${MODEL} ($([ "$INSTALL_OLLAMA" = true ] && echo 'will install Ollama' || echo 'Ollama install skipped'), thinking: ${OLLAMA_THINK}, storage: ${OLLAMA_STORAGE})"
    info "dedup backend:   Redis ($([ -n "$REDIS_ADDR" ] && echo "external: ${REDIS_ADDR}" || echo 'bundled (deploy/redis.yaml)'))"
    info "admin GUI:       $([ "$ENABLE_WEBUI" = true ] && echo enabled || echo disabled)"
}

# ensure_namespace creates a namespace if missing (idempotent).
ensure_namespace() {
    kubectl create namespace "$1" --dry-run=client -o yaml | apply_stdin "namespace/$1"
}

# --- optional: build & push the image --------------------------------------
build_image() {
    [ "$BUILD" = true ] || return 0
    step "Building and pushing the agent image"
    have docker || die "--build requires Docker in PATH"

    # A fixed tag (:latest) + imagePullPolicy IfNotPresent lets the kubelet
    # silently reuse a STALE cached image after a push: containerd-backed
    # runtimes (kind, minikube, recent Docker Desktop) keep their own image
    # store, so `rollout restart` brings up new pods with the old binary.
    # Tag every build uniquely with the git commit (plus a timestamp when the
    # tree is dirty), so `set image` really changes the pod spec and forces a
    # genuine pull. The same build is also pushed as :latest for manual flows.
    if [ "$IMAGE_EXPLICIT" != true ]; then
        if have git && git -C "$REPO_ROOT" rev-parse --short HEAD >/dev/null 2>&1; then
            BUILD_VERSION="$(git -C "$REPO_ROOT" rev-parse --short HEAD)"
            if ! git -C "$REPO_ROOT" diff --quiet HEAD -- 2>/dev/null; then
                BUILD_VERSION="${BUILD_VERSION}-dirty-$(date +%s)"
            fi
            info "building from commit ${BUILD_VERSION}: $(git -C "$REPO_ROOT" log -1 --format=%s 2>/dev/null || true)"
            info "(make sure this checkout is up to date: git pull origin master)"
        else
            BUILD_VERSION="build-$(date +%s)"
        fi
        IMAGE="${REGISTRY}/k8s-ai-remediator:${BUILD_VERSION}"
    fi

    run docker build --build-arg BUILD_VERSION="${BUILD_VERSION:-dev}" -t "$IMAGE" "$REPO_ROOT"
    run docker push "$IMAGE"
    if [ "$IMAGE_EXPLICIT" != true ]; then
        run docker tag "$IMAGE" "${REGISTRY}/k8s-ai-remediator:latest"
        run docker push "${REGISTRY}/k8s-ai-remediator:latest"
    fi
}

# --- optional: install Ollama and pull the model ---------------------------
install_ollama() {
    [ "$INSTALL_OLLAMA" = true ] || { step "Skipping Ollama install (--skip-ollama)"; return 0; }
    step "Installing Ollama in namespace '${OLLAMA_NAMESPACE}'"
    ensure_namespace "$OLLAMA_NAMESPACE"

    # Model storage: a PVC by default so the weights survive pod restarts.
    # The PVC request is immutable once created (only expansion is allowed),
    # so re-runs with the same size are a no-op.
    local ollama_volume_source='emptyDir: {}'
    if [ "$OLLAMA_STORAGE" != "none" ]; then
        apply_stdin "ollama models PVC (${OLLAMA_STORAGE})" <<YAML
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ollama-models
  namespace: ${OLLAMA_NAMESPACE}
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: ${OLLAMA_STORAGE}
YAML
        ollama_volume_source=$'persistentVolumeClaim:\n            claimName: ollama-models'
    fi

    apply_stdin "ollama Deployment+Service" <<YAML
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama
  namespace: ${OLLAMA_NAMESPACE}
  labels: { app: ollama }
spec:
  replicas: 1
  selector:
    matchLabels: { app: ollama }
  template:
    metadata:
      labels: { app: ollama }
    spec:
      containers:
        - name: ollama
          image: ollama/ollama:latest
          ports:
            - containerPort: 11434
          env:
            - { name: OLLAMA_HOST, value: "0.0.0.0:11434" }
          resources:
            requests: { cpu: "500m", memory: "2Gi" }
            # qwen3.5:9b (q4) loads ~7GB of weights; 12Gi leaves headroom for
            # the KV cache and the runtime without OOMKilling the pod. Only
            # the request drives scheduling, so small nodes still fit.
            limits:   { cpu: "4",    memory: "12Gi" }
          volumeMounts:
            - { name: ollama-data, mountPath: /root/.ollama }
      volumes:
        - name: ollama-data
          ${ollama_volume_source}
---
apiVersion: v1
kind: Service
metadata:
  name: ollama
  namespace: ${OLLAMA_NAMESPACE}
spec:
  selector: { app: ollama }
  ports:
    - { port: 11434, targetPort: 11434 }
YAML

    step "Waiting for Ollama to be ready, then pulling '${MODEL}'"
    if ! run kubectl -n "$OLLAMA_NAMESPACE" rollout status deployment/ollama --timeout="$ROLLOUT_TIMEOUT"; then
        warn "Ollama did not become ready in ${ROLLOUT_TIMEOUT}."
        kubectl -n "$OLLAMA_NAMESPACE" get pods,pvc 2>/dev/null || true
        warn "If pvc/ollama-models is Pending, the cluster has no default StorageClass: provide one or re-run with --ollama-storage none (models will re-download on every restart)."
        die "Ollama is required (or pass --skip-ollama for an external one); aborting."
    fi
    info "Pulling the model (first run downloads several GB — this can take a while)..."
    run kubectl -n "$OLLAMA_NAMESPACE" exec deploy/ollama -- ollama pull "$MODEL"
    run kubectl -n "$OLLAMA_NAMESPACE" exec deploy/ollama -- ollama list
}

# --- Redis dedup backend (mandatory) ---------------------------------------
# ensure_redis_secret creates the ai-remediator-redis Secret that holds the
# Redis password (read by both the Redis Deployment and the agent). Idempotent:
# an existing Secret is left untouched so re-running the installer never rotates
# the password out from under a running Redis — which would lock the agent out
# of the persisted dedup state. The first argument controls whether a random
# password is generated when none is provided: true for the bundled Redis (we
# own both ends), false for an external Redis (a random password the external
# server does not know would just fail auth; connect unauthenticated instead).
ensure_redis_secret() {
    local allow_generate="${1:-false}"
    if [ "$DRY_RUN" = true ]; then
        printf '%s    [dry-run] ensure secret/ai-remediator-redis in %s%s\n' "$DIM" "$NAMESPACE" "$RESET"
        return 0
    fi
    if kubectl -n "$NAMESPACE" get secret ai-remediator-redis >/dev/null 2>&1; then
        info "Redis Secret 'ai-remediator-redis' already exists — keeping the current password"
        return 0
    fi
    if [ -z "$REDIS_PASSWORD" ]; then
        if [ "$allow_generate" = true ]; then
            REDIS_PASSWORD="$(gen_password)"
            REDIS_PASSWORD_GENERATED=true
        else
            info "No Redis password provided — agent will connect to ${REDIS_ADDR} without auth"
            return 0
        fi
    fi
    kubectl -n "$NAMESPACE" create secret generic ai-remediator-redis \
        --from-literal=password="$REDIS_PASSWORD"
    info "Created Redis password Secret 'ai-remediator-redis'"
}

# install_redis deploys the bundled Redis (deploy/redis.yaml) and waits for it,
# or — when REDIS_ADDR points at an external instance — skips the deployment and
# just ensures the password Secret. Sets REDIS_ADDR to the bundled Service when
# installing the bundled one so install_agent can wire it into the ConfigMap.
install_redis() {
    ensure_namespace "$NAMESPACE"

    if [ -n "$REDIS_ADDR" ]; then
        step "Using external Redis at ${REDIS_ADDR} (skipping bundled install)"
        ensure_redis_secret false  # external server owns the password; never invent one
        return 0
    fi

    step "Installing Redis dedup backend in namespace '${NAMESPACE}'"
    ensure_redis_secret true  # must exist before the Deployment starts (requirepass)

    # redis.yaml is namespaced to ai-remediator; retarget it if NAMESPACE differs.
    if [ "$NAMESPACE" = "ai-remediator" ]; then
        run kubectl apply -f "${DEPLOY_DIR}/redis.yaml"
    elif [ "$DRY_RUN" = true ]; then
        printf '%s    [dry-run] retarget redis.yaml to namespace %s%s\n' "$DIM" "$NAMESPACE" "$RESET"
    else
        sed "s/namespace: ai-remediator/namespace: ${NAMESPACE}/g" "${DEPLOY_DIR}/redis.yaml" | kubectl apply -f -
    fi

    REDIS_ADDR="ai-remediator-redis:6379"

    if [ "$DRY_RUN" = true ]; then
        printf '%s    [dry-run] kubectl -n %s rollout status deployment/ai-remediator-redis%s\n' "$DIM" "$NAMESPACE" "$RESET"
        return 0
    fi
    step "Waiting for Redis to be ready"
    if ! kubectl -n "$NAMESPACE" rollout status deployment/ai-remediator-redis --timeout="$ROLLOUT_TIMEOUT"; then
        warn "Redis did not become ready in ${ROLLOUT_TIMEOUT}."
        kubectl -n "$NAMESPACE" get pods -l app.kubernetes.io/name=ai-remediator-redis || true
        warn "If its PVC is Pending, the cluster has no default StorageClass — provide one, or pass --redis-addr to use an external Redis."
        die "Redis is the required dedup backend; aborting."
    fi
}

# --- RBAC + agent -----------------------------------------------------------
install_agent() {
    step "Applying RBAC (cluster-wide ServiceAccount + ClusterRole + leases)"
    ensure_namespace "$NAMESPACE"
    run kubectl apply -f "${DEPLOY_DIR}/rbac-cluster.yaml"

    if [ "$ENABLE_WEBUI" = true ]; then
        info "Applying admin GUI RBAC (configmap/secret/deploy + onboarding)"
        run kubectl apply -f "${DEPLOY_DIR}/rbac-webui.yaml"
    fi

    step "Applying the agent (ConfigMap, Secret, Deployment, Service)"
    run kubectl apply -f "${DEPLOY_DIR}/agent.yaml"

    # Override the bits the static manifest can't know: image, model, GUI
    # toggle and credentials. Merge patches so other keys are preserved.
    info "Setting image to ${IMAGE}"
    run kubectl -n "$NAMESPACE" set image deployment/ai-remediator-agent "agent=${IMAGE}"

    info "Setting OLLAMA_MODEL=${MODEL}, OLLAMA_THINK=${OLLAMA_THINK}, WEBUI_ENABLED=${ENABLE_WEBUI}, SCENARIO_SANDBOX_NAMESPACES=${SANDBOX_NS}, DEDUP_BACKEND=redis, REDIS_ADDR=${REDIS_ADDR}"
    run kubectl -n "$NAMESPACE" patch configmap ai-remediator-config --type merge \
        -p "{\"data\":{\"OLLAMA_MODEL\":\"${MODEL}\",\"OLLAMA_THINK\":\"${OLLAMA_THINK}\",\"OLLAMA_BASE_URL\":\"http://ollama.${OLLAMA_NAMESPACE}.svc.cluster.local:11434/api\",\"WEBUI_ENABLED\":\"${ENABLE_WEBUI}\",\"SCENARIO_SANDBOX_NAMESPACES\":\"${SANDBOX_NS}\",\"DEDUP_BACKEND\":\"redis\",\"REDIS_ADDR\":\"${REDIS_ADDR}\"}}"

    if [ "$ENABLE_WEBUI" = true ]; then
        if [ -z "$WEBUI_PASSWORD" ]; then
            WEBUI_PASSWORD="$(gen_password)"
            WEBUI_PASSWORD_GENERATED=true
        fi
        info "Setting admin GUI credentials (user: ${WEBUI_USERNAME})"
        run kubectl -n "$NAMESPACE" patch secret ai-remediator-secrets --type merge \
            -p "{\"stringData\":{\"WEBUI_USERNAME\":\"${WEBUI_USERNAME}\",\"WEBUI_PASSWORD\":\"${WEBUI_PASSWORD}\"}}"
    fi
}

# --- optional: scenarios sandbox RBAC --------------------------------------
install_scenarios() {
    { [ "$ENABLE_WEBUI" = true ] && [ "$ENABLE_SCENARIOS" = true ] && [ -n "$SANDBOX_NS" ]; } || return 0
    step "Enabling the GUI 'Scenarios' sandbox in namespace '${SANDBOX_NS}'"
    ensure_namespace "$SANDBOX_NS"
    # rbac-scenarios.yaml is namespaced to incident-lab; retarget it if the
    # sandbox namespace differs, otherwise apply as-is.
    if [ "$SANDBOX_NS" = "incident-lab" ]; then
        run kubectl apply -f "${DEPLOY_DIR}/rbac-scenarios.yaml"
    else
        if [ "$DRY_RUN" = true ]; then
            printf '%s    [dry-run] retarget rbac-scenarios.yaml to namespace %s%s\n' "$DIM" "$SANDBOX_NS" "$RESET"
        else
            sed "s/namespace: incident-lab/namespace: ${SANDBOX_NS}/g" "${DEPLOY_DIR}/rbac-scenarios.yaml" | kubectl apply -f -
        fi
    fi
}

# --- rollout + verify -------------------------------------------------------
rollout_and_verify() {
    step "Rolling out the agent"
    run kubectl -n "$NAMESPACE" rollout restart deployment/ai-remediator-agent
    if [ "$DRY_RUN" = true ]; then return 0; fi
    if ! kubectl -n "$NAMESPACE" rollout status deployment/ai-remediator-agent --timeout="$ROLLOUT_TIMEOUT"; then
        warn "Deployment did not become ready in ${ROLLOUT_TIMEOUT}."
        kubectl -n "$NAMESPACE" get pods -l app=ai-remediator-agent || true
        warn "If pods are ImagePullBackOff: build/push the image (re-run with --build) or mirror it (scripts/mirror-images.sh)."
        return 1
    fi
    step "Verifying"
    kubectl -n "$NAMESPACE" get pods -l app=ai-remediator-agent
    info "Recent agent logs:"
    kubectl -n "$NAMESPACE" logs deploy/ai-remediator-agent --tail=12 2>/dev/null | sed 's/^/      /' || true

    # When we just built an image, prove the running pod executes THAT build:
    # the binary logs its stamped version ("agent binary" line) at startup.
    if [ "$BUILD" = true ] && [ -n "$BUILD_VERSION" ]; then
        sleep 3
        if kubectl -n "$NAMESPACE" logs deploy/ai-remediator-agent --tail=50 2>/dev/null \
            | grep -q "\"version\":\"${BUILD_VERSION}\""; then
            info "${GREEN}image verified: the pod runs build ${BUILD_VERSION}${RESET}"
        else
            warn "the running pod does NOT report build ${BUILD_VERSION} — it may still run a cached image."
            warn "Check with: kubectl -n ${NAMESPACE} logs deploy/ai-remediator-agent | grep '\"agent binary\"'"
        fi
    fi
}

gen_password() {
    if have openssl; then openssl rand -base64 24
    else head -c 18 /dev/urandom | base64; fi
}

summary() {
    [ "$DRY_RUN" = true ] && { step "Dry-run complete — no changes were made."; return 0; }
    step "Done."
    info "Agent:   deployment/ai-remediator-agent in namespace ${NAMESPACE}"
    info "Dedup:   Redis backend at ${REDIS_ADDR}"
    [ "${REDIS_PASSWORD_GENERATED:-false}" = true ] && info "         (random password stored in Secret ai-remediator-redis, key 'password')"
    info "Metrics: kubectl -n ${NAMESPACE} port-forward deploy/ai-remediator-agent 9090:9090  ->  http://localhost:9090/metrics"
    if [ "$ENABLE_WEBUI" = true ]; then
        info "Admin GUI: kubectl -n ${NAMESPACE} port-forward svc/ai-remediator-webui 8080:80  ->  http://localhost:8080"
        info "  login: ${WEBUI_USERNAME} / ${WEBUI_PASSWORD}"
        [ "${WEBUI_PASSWORD_GENERATED:-false}" = true ] && warn "GUI password was generated above — store it now; it is only shown once. Front the GUI with TLS in production."
    fi
    info "Try a fault scenario: kubectl apply -f scenarios/critical-oomkilled.yaml  (namespace ${SANDBOX_NS})"
}

# --- uninstall --------------------------------------------------------------
uninstall() {
    step "Uninstalling ai-remediator"
    warn "This deletes namespace '${NAMESPACE}' (including the bundled Redis and its PVC/dedup data), the cluster RBAC, and (if present) '${SANDBOX_NS}'."
    run kubectl delete -f "${DEPLOY_DIR}/rbac-cluster.yaml" --ignore-not-found
    run kubectl delete clusterrole ai-remediator-webui-rbac-admin --ignore-not-found
    run kubectl delete clusterrolebinding ai-remediator-webui-rbac-admin --ignore-not-found
    run kubectl delete namespace "$NAMESPACE" --ignore-not-found
    run kubectl delete namespace "$SANDBOX_NS" --ignore-not-found
    info "Ollama namespace '${OLLAMA_NAMESPACE}' was left untouched. Remove it with:"
    info "  kubectl delete namespace ${OLLAMA_NAMESPACE}"
    step "Uninstall complete."
}

# --- main -------------------------------------------------------------------
main() {
    if [ "$UNINSTALL" = true ]; then
        preflight
        uninstall
        exit 0
    fi
    preflight
    build_image
    install_ollama
    install_redis
    install_agent
    install_scenarios
    rollout_and_verify
    summary
}

main
