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
cfg="$root/hack/e2e/hydra.yaml"

cleanup() {
  "$hydra" bootstrap delete -f "$cfg" --force >/dev/null 2>&1 || true
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

echo "--- hydra bootstrap"
"$hydra" bootstrap -f "$cfg" --hypervisor-credentials "$pki"

kubeconfig="$work/kubeconfig"
"$hydra" bootstrap kubeconfig -f "$cfg" > "$kubeconfig"
export KUBECONFIG="$kubeconfig"

echo "--- a rerun is a no-op"
"$hydra" bootstrap -f "$cfg" 2>&1 | grep -q "reusing it"

echo "--- server-side dry-run of every example"
for example in "$root"/examples/*.yaml; do
  echo "  $(basename "$example")"
  ns=$(awk '/^  namespace:/ {print $2; exit}' "$example")
  ns=${ns:-default}
  kubectl create namespace "$ns" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
  "$hydra" cluster render -f "$example" |
    kubectl apply --server-side --field-manager=hydra-e2e --dry-run=server -f - |
    sed 's/^/    /'
done

echo "--- e2e passed"
