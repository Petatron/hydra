#!/bin/sh
# End-to-end test of hydra's Cilium HelmChartProxy (PET-100), with no lab:
#
#   1. a kind management cluster gets Cluster API, the Docker provider (CAPD)
#      and Cluster API Add-on Provider Helm (CAAPH) at hydra's pinned versions;
#   2. CAPD builds a small workload cluster with no CNI, so its nodes stay
#      NotReady;
#   3. the HelmChartProxy `hydra cluster render` emits for cni: hydra is
#      applied, and the nodes must go Ready from CAAPH-installed Cilium, with
#      hydra's own status reading it as installed;
#   4. raising spec.network.ciliumVersion must upgrade the same release;
#   5. changing only Cilium's values (the MTU) must restart the agents and the
#      operator (rollOutCiliumPods, PET-105), with hydra holding CNI ready
#      until they are back;
#   6. both clusters are deleted, whatever happened.
#
# In 3 and 4 hydra's status is polled every few seconds, and whenever it says
# "CNI ready" the cilium DaemonSet must already have rolled out at the
# version (PET-103): CAAPH reports the release Ready once Helm has applied
# it, before the agents restart. hydra reads that rollout through the
# workload cluster's own API, at the address in its kubeconfig Secret: an
# address on kind's Docker network, so hydra runs in a container on that
# network (Docker Desktop's host cannot reach it).
#
# CAPD stands in for the Hydra provider, which needs libvirt: CAAPH only uses
# the Cluster object and its kubeconfig Secret, so what it does does not
# depend on the infrastructure provider. hydra's bootstrap cluster cannot host
# CAPD (CAPD needs the Docker socket inside the kind node), so this stage
# builds its own.
#
# Needs Docker, kubectl and Go. On Linux, CAPD needs inotify limits above
# most distributions' defaults (fs.inotify.max_user_watches=1048576,
# max_user_instances=8192); CI raises them. clusterctl is downloaded at the pinned
# version unless CLUSTERCTL names one. Run from the repo root:
# ./hack/e2e/cilium.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/hydra-cilium-e2e.XXXXXX")
hydra="$work/hydra"

CAPI_VERSION=v1.14.0  # bootstrap.CAPIVersion
CAAPH_VERSION=v0.6.6  # bootstrap.CAAPHVersion
K8S_VERSION=v1.35.8   # the pinned kind node's Kubernetes
NODE_IMAGE=kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0
FROM_VERSION=1.19.1   # the default ciliumVersion
TO_VERSION=1.19.11    # the other allowed version

suffix=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')
mgmt="hydra-cilium-$suffix"
wl="w$suffix"
kind() { (cd "$root" && go run sigs.k8s.io/kind "$@"); }

created=0
cleanup() {
  if [ "$created" = 1 ]; then
    # Delete the workload cluster through Cluster API first, so CAPD removes
    # its containers; then the management cluster.
    kubectl --kubeconfig "$work/mgmt.kubeconfig" delete cluster "$wl" --wait --timeout=3m >/dev/null 2>&1 || true
    leftover=$(docker ps -aq --filter "label=io.x-k8s.kind.cluster=$wl" 2>/dev/null || true)
    if [ -n "$leftover" ]; then
      # shellcheck disable=SC2086 # one container ID per word
      docker rm -f $leftover >/dev/null 2>&1 || true
    fi
    kind delete cluster --name "$mgmt" >/dev/null 2>&1 || true
  fi
  rm -r -- "${work:?}"
}
# On INT or TERM, clean up and stop: a trap that only cleaned up would let
# the script carry on without its work directory. EXIT cleans up otherwise.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

fail() {
  echo "FAIL: $*" >&2
  diagnose >&2 || true
  exit 1
}

# diagnose prints what a failed run needs: the workload cluster's nodes and
# pods, Cilium's events and the agents' logs, and CAAPH's view.
diagnose() {
  [ -s "$work/wl.kubeconfig" ] || return 0
  echo "--- diagnostics"
  wlk get nodes -o wide || true
  wlk get pods -A -o wide || true
  wlk -n kube-system describe daemonset cilium || true
  wlk -n kube-system get events --sort-by=.lastTimestamp || true
  for pod in $(wlk -n kube-system get pods -l k8s-app=cilium -o name 2>/dev/null); do
    echo "--- $pod"
    wlk -n kube-system logs "$pod" --all-containers --tail=80 || true
    wlk -n kube-system logs "$pod" --all-containers --previous --tail=40 2>/dev/null || true
  done
  kubectl get helmreleaseproxy -A -o yaml || true
}

echo "--- building hydra"
(cd "$root" && go build -o "$hydra" ./cmd/hydra)
# The same hydra for Linux, run on kind's network by hydra_cni.
case $(docker info --format '{{.Architecture}}') in
  x86_64 | amd64) docker_arch=amd64 ;;
  aarch64 | arm64) docker_arch=arm64 ;;
  *) fail "unsupported Docker architecture $(docker info --format '{{.Architecture}}')" ;;
esac
(cd "$root" && CGO_ENABLED=0 GOOS=linux GOARCH=$docker_arch go build -o "$work/hydra-linux" ./cmd/hydra)
"$hydra" version >"$work/version"
grep -q "cilium $FROM_VERSION (default; allowed .*$TO_VERSION" "$work/version" || fail "hydra no longer allows $FROM_VERSION -> $TO_VERSION: $(cat "$work/version")"

if [ -n "${CLUSTERCTL:-}" ]; then
  clusterctl=$CLUSTERCTL
else
  echo "--- downloading clusterctl $CAPI_VERSION"
  os=$(uname -s | tr '[:upper:]' '[:lower:]')
  arch=$(uname -m)
  case $arch in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  clusterctl="$work/clusterctl"
  curl -fsSL -o "$clusterctl" "https://github.com/kubernetes-sigs/cluster-api/releases/download/$CAPI_VERSION/clusterctl-$os-$arch"
  chmod +x "$clusterctl"
fi

echo "--- kind management cluster $mgmt, with the Docker socket for CAPD"
cat >"$work/kind.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /var/run/docker.sock
        containerPath: /var/run/docker.sock
EOF
created=1
kind create cluster --name "$mgmt" --image "$NODE_IMAGE" --config "$work/kind.yaml" --kubeconfig "$work/mgmt.kubeconfig" --wait 2m
export KUBECONFIG="$work/mgmt.kubeconfig"
# The management API as a container on kind's network reaches it.
kind get kubeconfig --internal --name "$mgmt" >"$work/mgmt-internal.kubeconfig"

echo "--- clusterctl init: Cluster API $CAPI_VERSION, CAPD, CAAPH $CAAPH_VERSION"
CLUSTER_TOPOLOGY=true "$clusterctl" init --wait-providers \
  --core "cluster-api:$CAPI_VERSION" --bootstrap "kubeadm:$CAPI_VERSION" --control-plane "kubeadm:$CAPI_VERSION" \
  --infrastructure "docker:$CAPI_VERSION" --addon "helm:$CAAPH_VERSION"

echo "--- CAPD workload cluster $wl, no CNI"
CLUSTER_TOPOLOGY=true "$clusterctl" generate cluster "$wl" --flavor development --infrastructure "docker:$CAPI_VERSION" \
  --kubernetes-version "$K8S_VERSION" --control-plane-machine-count 1 --worker-machine-count 1 >"$work/wl.yaml"
kubectl apply -f "$work/wl.yaml" >/dev/null
pods=$(kubectl get cluster "$wl" -o 'jsonpath={.spec.clusterNetwork.pods.cidrBlocks[0]}')
[ -n "$pods" ] || fail "the workload cluster has no pod CIDR"

# The workload kubeconfig names the load balancer by its address on the kind
# Docker network, which a Docker Desktop host cannot reach; its API port is
# also published on the host, and CAPD's certificates cover 127.0.0.1. Every
# call has a timeout, so an unreachable API fails the wait instead of hanging.
wl_kubeconfig() {
  "$clusterctl" get kubeconfig "$wl" >"$work/wl.raw" 2>/dev/null || return 1
  port=$(docker port "$wl-lb" 6443/tcp 2>/dev/null | head -1 | sed 's/.*://')
  [ -n "$port" ] || return 1
  sed "s|^\( *server: \).*|\1https://127.0.0.1:$port|" "$work/wl.raw" >"$work/wl.kubeconfig"
}
wlk() { kubectl --kubeconfig "$work/wl.kubeconfig" --request-timeout=20s "$@"; }

# Wait for both nodes to register; with no CNI they stay NotReady.
i=0
until wl_kubeconfig && [ "$(wlk get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')" = 2 ]; do
  i=$((i + 1))
  [ "$i" -lt 90 ] || fail "two nodes did not register within 15m"
  sleep 10
done
wlk get nodes
if wlk get nodes -o 'jsonpath={.items[*].status.conditions[?(@.type=="Ready")].status}' | grep -q True; then
  fail "a node is Ready before any CNI is installed"
fi

# render_cni VERSION [MTU]: a hydra.yaml for the same cluster name, namespace
# and pod CIDR: only its CNI HelmChartProxy is used. Its machine settings are
# never applied.
render_cni() {
  cat >"$work/hydra.yaml" <<EOF
apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata:
  name: $wl
  namespace: default
spec:
  hypervisor:
    address: hypervisor.e2e.invalid:16514
  storagePool: pool
  image:
    name: img.qcow2
  network:
    controlPlaneEndpoint: 10.123.0.10
    bridges: [br0]
    podCIDR: $pods
    ciliumVersion: $1
${2:+    mtu: $2}
  controlPlane:
    replicas: 1
EOF
  "$hydra" cluster render -f "$work/hydra.yaml" >"$work/render.yaml"
  awk '/^# The CNI: Cilium/ {on = 1} on && /^---$/ {exit} on {print}' "$work/render.yaml" >"$work/cni.yaml"
  grep -q '^kind: HelmChartProxy$' "$work/cni.yaml" || fail "render emitted no CNI HelmChartProxy"
}

# hydra's own reading of the CNI, through the real CAAPH objects and the
# workload cluster's API, from a container on kind's network (the node image
# is already here; hydra is static).
hydra_cni() {
  docker run --rm --network kind -v "$work:/work" --entrypoint /work/hydra-linux "$NODE_IMAGE" \
    cluster status -f /work/hydra.yaml --management-kubeconfig /work/mgmt-internal.kubeconfig >"$work/status" 2>&1 || true
  cat "$work/status"
}

# release_ready VERSION REVISION: CAAPH reports the release at VERSION and
# REVISION Ready, which is all hydra's "CNI ready" meant before PET-103.
release_ready() {
  sel="cluster.x-k8s.io/cluster-name=$wl,helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name=$wl-cilium"
  [ "$(kubectl get helmreleaseproxy -l "$sel" -o 'jsonpath={range .items[*]}{.spec.version} {.status.revision} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')" = "$1 $2 True" ]
}

# rolled_out VERSION: the cilium DaemonSet and the cilium-operator
# Deployment run VERSION's images and have finished rolling out, as `kubectl
# rollout status` reads them: each controller observed the latest spec, and
# every desired pod is updated and available, with no old operator pod left.
# Read just after hydra spoke, so a rollout that completes in between can
# pass; agents take tens of seconds to restart, far longer than that read.
rolled_out() {
  ds=$(wlk -n kube-system get daemonset cilium -o 'jsonpath={.metadata.generation} {.status.observedGeneration} {.status.desiredNumberScheduled} {.status.updatedNumberScheduled} {.status.numberAvailable}|{.spec.template.spec.containers[?(@.name=="cilium-agent")].image}' 2>/dev/null) || return 1
  op=$(wlk -n kube-system get deployment cilium-operator -o 'jsonpath={.metadata.generation} {.status.observedGeneration} {.spec.replicas} {.status.updatedReplicas} {.status.availableReplicas} {.status.replicas}|{.spec.template.spec.containers[?(@.name=="cilium-operator")].image}' 2>/dev/null) || return 1
  for image in "${ds#*|}" "${op#*|}"; do
    case $image in *":v$1@"* | *":v$1") ;; *) return 1 ;; esac
  done
  # shellcheck disable=SC2086 # five numbers; an absent one is read as 0
  set -- ${ds%%|*}
  [ "${2:-0}" -ge "${1:-1}" ] && [ "${3:-0}" -gt 0 ] && [ "${4:-0}" -ge "$3" ] && [ "${5:-0}" -ge "$3" ] || return 1
  # shellcheck disable=SC2086 # six numbers; an absent one is read as 0
  set -- ${op%%|*}
  [ "${2:-0}" -ge "${1:-1}" ] && [ "${3:-0}" -gt 0 ] && [ "${4:-0}" -ge "$3" ] && [ "${5:-0}" -ge "$3" ] && [ "${6:-0}" -le "${4:-0}" ]
}

# wait_cilium VERSION REVISION [MIN_GAP]: the release is at VERSION, Ready
# for the current generation, at REVISION, hydra says CNI ready, and every
# node is Ready. hydra must never say CNI ready while the agents or the
# operator are still rolling out; gap counts the polls where CAAPH already
# reported the release Ready but they had not rolled out, and hydra rightly
# did not say ready. At least MIN_GAP such polls are required: on an upgrade
# the agents restart for tens of seconds after Helm applies the chart, so a
# zero gap means the test never saw the window it exists to check.
wait_cilium() {
  i=0 gap=0
  while :; do
    hydra_cni >/dev/null
    # Counted ready without reading the rollout proves nothing here: hydra
    # runs where the workload API is reachable.
    if grep -q "rollout unverified" "$work/status"; then
      cat "$work/status" >&2
      fail "hydra could not read the workload cluster's API"
    fi
    if grep -q "CNI ready" "$work/status"; then
      rolled_out "$1" || {
        cat "$work/status" >&2
        wlk -n kube-system get daemonset cilium -o wide >&2 || true
        wlk -n kube-system get deployment cilium-operator -o wide >&2 || true
        fail "hydra reported CNI ready before Cilium's agents and operator rolled out at $1"
      }
      release_ready "$1" "$2" || fail "hydra reported CNI ready but the release is not Ready at $1 (revision $2)"
      break
    fi
    if release_ready "$1" "$2" && ! rolled_out "$1"; then
      gap=$((gap + 1))
      echo "    release Ready, Cilium still rolling out; hydra: $(grep -o 'CNI [^·]*' "$work/status" | head -1)"
    fi
    i=$((i + 1))
    if [ "$i" -ge 200 ]; then
      kubectl get helmreleaseproxy -A -o yaml >&2 || true
      cat "$work/status" >&2
      fail "Cilium $1 (revision $2) was not reported ready within ~10m"
    fi
    sleep 2
  done
  echo "    hydra said CNI ready only after Cilium rolled out ($gap polls with the release Ready while it was still rolling)"
  [ "$gap" -ge "${3:-0}" ] || fail "only $gap polls saw the release Ready before Cilium rolled out, want at least ${3:-0}: the check never saw the window"
  sel="cluster.x-k8s.io/cluster-name=$wl,helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name=$wl-cilium"
  [ "$(kubectl get helmreleaseproxy -l "$sel" --no-headers | wc -l | tr -d ' ')" = 1 ] || fail "not exactly one HelmReleaseProxy for $wl-cilium"
  kubectl --kubeconfig "$work/wl.kubeconfig" wait nodes --all --for=condition=Ready --timeout=5m || fail "the nodes did not go Ready on Cilium $1"
  kubectl --kubeconfig "$work/wl.kubeconfig" -n kube-system rollout status daemonset/cilium --timeout=5m || fail "the cilium DaemonSet did not roll out at $1"
  image=$(wlk -n kube-system get daemonset cilium -o 'jsonpath={.spec.template.spec.containers[0].image}')
  case $image in *":v$1@"* | *":v$1") ;; *) fail "the cilium DaemonSet runs $image, want v$1" ;; esac
  echo "    Cilium $1, revision $2, every node Ready ($image)"
}

echo "--- hydra's CNI HelmChartProxy (Cilium $FROM_VERSION) installs Cilium; the nodes go Ready"
render_cni "$FROM_VERSION"
kubectl label cluster "$wl" "hydra.io/cluster=$wl" "cni.hydra.io/cilium=$wl" --overwrite >/dev/null
kubectl apply --server-side --field-manager=hydra-e2e -f "$work/cni.yaml"
wait_cilium "$FROM_VERSION" 1
hydra_cni | sed 's/^/    /'

echo "--- raising ciliumVersion to $TO_VERSION upgrades the same release"
render_cni "$TO_VERSION"
kubectl apply --server-side --field-manager=hydra-e2e -f "$work/cni.yaml"
wait_cilium "$TO_VERSION" 2 1

echo "--- changing only the MTU restarts the agents and the operator, and hydra waits for them"
ds_gen=$(wlk -n kube-system get daemonset cilium -o 'jsonpath={.metadata.generation}')
op_gen=$(wlk -n kube-system get deployment cilium-operator -o 'jsonpath={.metadata.generation}')
render_cni "$TO_VERSION" 1400
kubectl apply --server-side --field-manager=hydra-e2e -f "$work/cni.yaml"
wait_cilium "$TO_VERSION" 3 1
[ "$(wlk -n kube-system get configmap cilium-config -o 'jsonpath={.data.mtu}')" = 1400 ] || fail "cilium-config does not carry the new MTU"
[ "$(wlk -n kube-system get daemonset cilium -o 'jsonpath={.metadata.generation}')" -gt "$ds_gen" ] || fail "the MTU change did not change the agents' pod template"
[ "$(wlk -n kube-system get deployment cilium-operator -o 'jsonpath={.metadata.generation}')" -gt "$op_gen" ] || fail "the MTU change did not change the operator's pod template"
echo "    the agents and the operator restarted with the new config"

echo "--- cilium e2e passed"
