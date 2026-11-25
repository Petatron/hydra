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
#   5. both clusters are deleted, whatever happened.
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

# A hydra.yaml for the same cluster name, namespace and pod CIDR: only its
# CNI HelmChartProxy is used. Its machine settings are never applied.
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
  controlPlane:
    replicas: 1
EOF
  "$hydra" cluster render -f "$work/hydra.yaml" >"$work/render.yaml"
  awk '/^# The CNI: Cilium/ {on = 1} on && /^---$/ {exit} on {print}' "$work/render.yaml" >"$work/cni.yaml"
  grep -q '^kind: HelmChartProxy$' "$work/cni.yaml" || fail "render emitted no CNI HelmChartProxy"
}

# hydra's own reading of the CNI, through the real CAAPH objects.
hydra_cni() {
  "$hydra" cluster status -f "$work/hydra.yaml" --management-kubeconfig "$work/mgmt.kubeconfig" >"$work/status" 2>&1 || true
  cat "$work/status"
}

# wait_cilium VERSION REVISION: the release is at VERSION, Ready for the
# current generation, at REVISION, and every node is Ready.
wait_cilium() {
  sel="cluster.x-k8s.io/cluster-name=$wl,helmreleaseproxy.addons.cluster.x-k8s.io/helmchartproxy-name=$wl-cilium"
  i=0
  until [ "$(kubectl get helmreleaseproxy -l "$sel" -o 'jsonpath={range .items[*]}{.spec.version} {.status.revision} {.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}')" = "$1 $2 True" ] &&
    hydra_cni | grep -q "CNI ready"; do
    i=$((i + 1))
    if [ "$i" -ge 60 ]; then
      kubectl get helmreleaseproxy -l "$sel" -o yaml >&2 || true
      cat "$work/status" >&2
      fail "Cilium $1 (revision $2) was not reported Ready within 10m"
    fi
    sleep 10
  done
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
wait_cilium "$TO_VERSION" 2

echo "--- cilium e2e passed"
