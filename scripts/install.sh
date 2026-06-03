#!/usr/bin/env bash
# install.sh — set up the whole ai-remediator stack on a Kubernetes cluster,
# cleanly and idempotently, in the correct order:
#
#   1. (optional) build & push the agent image to your registry
#   2. (optional) install Ollama and pull the LLM model
#   3. apply RBAC (cluster-wide ServiceAccount/ClusterRole + leader-election)
#   4. apply the agent (Namespace, ConfigMap, Secret, Deployment, Service)
#   5. (optional) apply the admin GUI + scenarios RBAC
#   6. override image / model / GUI credentials, then roll out and verify
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
#   --build              Build & push the image first (needs Docker)  [BUILD=true]
#   --skip-ollama        Do not install Ollama / pull the model       [INSTALL_OLLAMA=false]
#   --no-webui           Install the agent without the admin GUI      [ENABLE_WEBUI=false]
#   --webui-user NAME    GUI login username (default: admin)          [WEBUI_USERNAME]
#   --webui-password PW  GUI login password (default: random)         [WEBUI_PASSWORD]
#   --sandbox-ns NS      Namespace allowed to receive fault scenarios [SANDBOX_NS]
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
MODEL="${MODEL:-qwen2.5:7b}"
SANDBOX_NS="${SANDBOX_NS:-incident-lab}"

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

usage() { sed -n '2,48p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }

# --- argument parsing -------------------------------------------------------
while [ "$#" -gt 0 ]; do
    case "$1" in
        --image)            IMAGE="$2"; shift 2 ;;
        --registry)         REGISTRY="$2"; shift 2 ;;
        --model)            MODEL="$2"; shift 2 ;;
        --build)            BUILD=true; shift ;;
        --skip-ollama)      INSTALL_OLLAMA=false; shift ;;
        --no-webui)         ENABLE_WEBUI=false; shift ;;
        --webui-user)       WEBUI_USERNAME="$2"; shift 2 ;;
        --webui-password)   WEBUI_PASSWORD="$2"; shift 2 ;;
        --sandbox-ns)       SANDBOX_NS="$2"; shift 2 ;;
        --no-scenarios)     ENABLE_SCENARIOS=false; shift ;;
        --dry-run)          DRY_RUN=true; shift ;;
        --uninstall)        UNINSTALL=true; shift ;;
        -h|--help)          usage; exit 0 ;;
        *)                  die "unknown flag: $1 (use --help)" ;;
    esac
done

IMAGE="${IMAGE:-${REGISTRY}/k8s-ai-remediator:latest}"

# --- preflight --------------------------------------------------------------
preflight() {
    step "Preflight checks"
    have kubectl || die "kubectl not found in PATH"
    if [ "$DRY_RUN" != true ]; then
        kubectl cluster-info >/dev/null 2>&1 || die "kubectl cannot reach a cluster (check your kubeconfig/context)"
    fi
    [ -f "${DEPLOY_DIR}/agent.yaml" ] || die "deploy/agent.yaml not found — run from a repo checkout"
    [ -f "${DEPLOY_DIR}/rbac-cluster.yaml" ] || die "deploy/rbac-cluster.yaml not found"
    info "kubectl context: $(kubectl config current-context 2>/dev/null || echo '?')"
    info "agent image:     ${IMAGE}"
    info "ollama model:    ${MODEL} ($([ "$INSTALL_OLLAMA" = true ] && echo 'will install Ollama' || echo 'Ollama install skipped'))"
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
    run docker build -t "$IMAGE" "$REPO_ROOT"
    run docker push "$IMAGE"
}

# --- optional: install Ollama and pull the model ---------------------------
install_ollama() {
    [ "$INSTALL_OLLAMA" = true ] || { step "Skipping Ollama install (--skip-ollama)"; return 0; }
    step "Installing Ollama in namespace '${OLLAMA_NAMESPACE}'"
    ensure_namespace "$OLLAMA_NAMESPACE"
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
            limits:   { cpu: "4",    memory: "8Gi" }
          volumeMounts:
            - { name: ollama-data, mountPath: /root/.ollama }
      volumes:
        - { name: ollama-data, emptyDir: {} }
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
    run kubectl -n "$OLLAMA_NAMESPACE" rollout status deployment/ollama --timeout="$ROLLOUT_TIMEOUT"
    info "Pulling the model (first run downloads several GB — this can take a while)..."
    run kubectl -n "$OLLAMA_NAMESPACE" exec deploy/ollama -- ollama pull "$MODEL"
    run kubectl -n "$OLLAMA_NAMESPACE" exec deploy/ollama -- ollama list
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

    info "Setting OLLAMA_MODEL=${MODEL}, WEBUI_ENABLED=${ENABLE_WEBUI}, SCENARIO_SANDBOX_NAMESPACES=${SANDBOX_NS}"
    run kubectl -n "$NAMESPACE" patch configmap ai-remediator-config --type merge \
        -p "{\"data\":{\"OLLAMA_MODEL\":\"${MODEL}\",\"OLLAMA_BASE_URL\":\"http://ollama.${OLLAMA_NAMESPACE}.svc.cluster.local:11434/api\",\"WEBUI_ENABLED\":\"${ENABLE_WEBUI}\",\"SCENARIO_SANDBOX_NAMESPACES\":\"${SANDBOX_NS}\"}}"

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
}

gen_password() {
    if have openssl; then openssl rand -base64 24
    else head -c 18 /dev/urandom | base64; fi
}

summary() {
    [ "$DRY_RUN" = true ] && { step "Dry-run complete — no changes were made."; return 0; }
    step "Done."
    info "Agent:   deployment/ai-remediator-agent in namespace ${NAMESPACE}"
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
    warn "This deletes namespace '${NAMESPACE}', the cluster RBAC, and (if present) '${SANDBOX_NS}'."
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
    install_agent
    install_scenarios
    rollout_and_verify
    summary
}

main
