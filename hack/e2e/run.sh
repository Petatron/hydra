#!/bin/sh
# End-to-end test against a real (kind) management cluster, with no lab:
#
#   1. `hydra bootstrap` creates a kind cluster and installs cert-manager,
#      Cluster API and the Hydra provider from its GitHub release;
#   2. every file in examples/ is rendered and server-side dry-run against
#      that cluster, so a template field the CRDs or webhooks reject fails here;
#   3. the bootstrap cluster is deleted, whatever happened.
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

echo "--- e2e passed"
