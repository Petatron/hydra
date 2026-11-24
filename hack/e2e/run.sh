#!/bin/sh
# End-to-end test against a real (kind) management cluster, with no lab:
#
#   1. `hydra bootstrap` creates a kind cluster and installs cert-manager,
#      Cluster API and the Hydra provider from its GitHub release;
#   2. every file in examples/ is rendered and server-side dry-run against
#      that cluster, so a template field the CRDs or webhooks reject fails here;
#   3. `hydra cluster apply` creates the e2e cluster's objects, then prunes
#      what hydra.yaml stops listing: an addon and a superseded template are
#      deleted, an addon it does not know is refused before anything is
#      written, and a removed pool is kept until --prune-pools names it. The
#      hypervisor never resolves, so no VM is made;
#   4. the bootstrap cluster is deleted, whatever happened.
#
# Needs Docker (or Podman/nerdctl), kubectl, openssl and Go. Run from the repo
# root: ./hack/e2e/run.sh
set -eu

root=$(cd "$(dirname "$0")/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/hydra-e2e.XXXXXX")
hydra="$work/hydra"

# A name unique to this run, so the test never reuses (and its cleanup never
# deletes) a bootstrap cluster that already exists on this machine.
name="e2e-$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n')"
cfg="$work/hydra.yaml"
sed "s/^  name: e2e$/  name: $name/" "$root/hack/e2e/hydra.yaml" >"$cfg"
grep -q "^  name: $name$" "$cfg"

# Only a cluster this run created is ever deleted.
created=0
cleanup() {
  if [ "$created" = 1 ]; then
    "$hydra" bootstrap delete -f "$cfg" --force >/dev/null 2>&1 || true
  fi
  # ${work:?} refuses to expand if work were ever empty.
  rm -r -- "${work:?}"
}
trap cleanup EXIT INT TERM

echo "--- building hydra"
(cd "$root" && go build -o "$hydra" ./cmd/hydra)

echo "--- throwaway hypervisor credentials (never used to connect)"
pki="$work/pki"
mkdir -p "$pki"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout "$work/ca.key" -out "$pki/cacert.pem" -days 1 -subj /CN=hydra-e2e-ca 2>/dev/null
openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
  -keyout "$pki/clientkey.pem" -out "$work/client.csr" -subj /CN=hydra-e2e 2>/dev/null
openssl x509 -req -in "$work/client.csr" -CA "$pki/cacert.pem" -CAkey "$work/ca.key" \
  -CAcreateserial -out "$pki/clientcert.pem" -days 1 2>/dev/null

if "$hydra" bootstrap kubeconfig -f "$cfg" >/dev/null 2>&1; then
  echo "kind cluster hydra-bootstrap-$name already exists; refusing to touch it" >&2
  exit 1
fi

echo "--- hydra bootstrap (hydra-bootstrap-$name)"
created=1
"$hydra" bootstrap -f "$cfg" --hypervisor-credentials "$pki"

kubeconfig="$work/kubeconfig"
"$hydra" bootstrap kubeconfig -f "$cfg" > "$kubeconfig"
export KUBECONFIG="$kubeconfig"

# No pipelines below: /bin/sh has no pipefail, so a pipeline's status is its
# last command's, and a failing hydra or kubectl ahead of `sed` would pass.
# Each command writes to a file, so `set -e` sees its real exit status.

echo "--- a rerun is a no-op"
rerun="$work/rerun.out"
if ! "$hydra" bootstrap -f "$cfg" >"$rerun" 2>&1; then
  cat "$rerun" >&2
  exit 1
fi
cat "$rerun"
grep -q "reusing it" "$rerun"

echo "--- server-side dry-run of every example"
for example in "$root"/examples/*.yaml; do
  echo "  $(basename "$example")"
  ns=$(awk '/^  namespace:/ {print $2; exit}' "$example")
  ns=${ns:-default}
  kubectl create namespace "$ns" --dry-run=client -o yaml >"$work/ns.yaml"
  kubectl apply -f "$work/ns.yaml" >/dev/null
  "$hydra" cluster render -f "$example" >"$work/render.yaml"
  kubectl apply --server-side --field-manager=hydra-e2e --dry-run=server -f "$work/render.yaml" >"$work/apply.out"
  sed 's/^/    /' "$work/apply.out"
done

# --- hydra cluster apply ----------------------------------------------------

# run NAME CMD...: CMD must succeed; its output is kept in $work/NAME.out.
run() {
  out="$work/$1.out"
  shift
  if ! "$@" >"$out" 2>&1; then
    cat "$out" >&2
    echo "FAIL: expected success: $*" >&2
    exit 1
  fi
  sed 's/^/    /' "$out"
}

# refused NAME CMD...: CMD must fail; its output is kept in $work/NAME.out.
refused() {
  out="$work/$1.out"
  shift
  if "$@" >"$out" 2>&1; then
    cat "$out" >&2
    echo "FAIL: expected a refusal: $*" >&2
    exit 1
  fi
  sed 's/^/    /' "$out"
}

# says NAME TEXT: $work/NAME.out contains TEXT.
says() {
  if ! grep -qF -- "$2" "$work/$1.out"; then
    echo "FAIL: $1 output lacks: $2" >&2
    exit 1
  fi
}

# state KIND NAME: "live", or "gone" when it is deleted or being deleted (a
# finalizer may hold it while its controller cleans up).
ns=e2e
state() {
  kubectl get -n "$ns" "$1" "$2" --ignore-not-found \
    -o 'jsonpath={.metadata.name}/{.metadata.deletionTimestamp}' >"$work/state.out"
  s=$(cat "$work/state.out")
  if [ "$s" = "$2/" ]; then echo live; else echo gone; fi
}

# expect KIND NAME live|gone
expect() {
  got=$(state "$1" "$2")
  if [ "$got" != "$3" ]; then
    echo "FAIL: $1 $ns/$2 is $got, want $3" >&2
    exit 1
  fi
  echo "    $1 $ns/$2: $3"
}

# full lists a node pool and an addon; $cfg is the same cluster without them.
full="$work/full.yaml"
cp "$cfg" "$full"
cat >>"$full" <<'EOF'
  nodePools:
    - name: extra
      replicas: 1
      vcpus: 2
      memory: 4Gi
      diskSize: 20Gi
  addons:
    - name: metrics-server
EOF

echo "--- hydra cluster apply: creates the cluster, its pool and its addon"
run apply-create "$hydra" cluster apply -f "$full" --wait=false
expect machinedeployment "$name-extra" live
expect helmchartproxy "$name-metrics-server" live

echo "--- an addon hydra does not know, and a superseded template"
# The catalogue has no protected addon yet; an uncatalogued addon takes the
# same refusal, and needs the same --allow-remove=<addon>.
cat >"$work/planted.yaml" <<EOF
apiVersion: addons.cluster.x-k8s.io/v1alpha1
kind: HelmChartProxy
metadata:
  name: $name-cert-manager
  namespace: $ns
  labels:
    hydra.io/cluster: $name
    hydra.io/addon: cert-manager
spec:
  clusterSelector:
    matchLabels:
      hydra.io/e2e-selects-nothing: "true"
  repoURL: https://charts.jetstack.io
  chartName: cert-manager
---
apiVersion: infrastructure.cluster.x-k8s.io/v1alpha1
kind: HydraMachineTemplate
metadata:
  name: $name-cp-stale
  namespace: $ns
  labels:
    hydra.io/cluster: $name
spec:
  template:
    spec:
      vcpus: 2
      memory: 4Gi
      diskSize: 20Gi
EOF
run planted kubectl apply -f "$work/planted.yaml"

echo "--- apply refuses before writing anything"
refused apply-refused "$hydra" cluster apply -f "$full" --wait=false
says apply-refused "not applying"
says apply-refused "--allow-remove=cert-manager"
expect hydramachinetemplate "$name-cp-stale" live
expect helmchartproxy "$name-cert-manager" live

echo "--- apply --allow-remove prunes the addon and the unused template"
run apply-allowed "$hydra" cluster apply -f "$full" --wait=false --allow-remove=cert-manager
expect helmchartproxy "$name-cert-manager" gone
expect hydramachinetemplate "$name-cp-stale" gone
expect helmchartproxy "$name-metrics-server" live
expect machinedeployment "$name-extra" live

echo "--- plan shows what dropping the pool and the addon does"
run plan-dropped "$hydra" cluster plan -f "$cfg"
says plan-dropped "- prune HelmChartProxy $ns/$name-metrics-server"
says plan-dropped "needs --prune-pools=extra"

echo "--- apply prunes the dropped addon and keeps the pool"
run apply-dropped "$hydra" cluster apply -f "$cfg" --wait=false
says apply-dropped "needs --prune-pools=extra"
expect helmchartproxy "$name-metrics-server" gone
expect machinedeployment "$name-extra" live

echo "--- apply --prune-pools=extra deletes the pool"
run apply-pool "$hydra" cluster apply -f "$cfg" --wait=false --prune-pools=extra
expect machinedeployment "$name-extra" gone

echo "--- e2e passed"
