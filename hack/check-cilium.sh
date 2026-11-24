#!/bin/sh
# Renders the Cilium chart with the values hydra puts in each cni: hydra
# example's HelmChartProxy, at every chart version hydra allows
# (internal/addons), and fails if any render fails or still carries a
# sentinel. Cluster API Add-on Provider Helm installs exactly these values, so
# a canonical-values change or a new allowed version that the chart rejects
# fails here, not in a cluster.
#
# Needs helm and network access. Run from anywhere: ./hack/check-cilium.sh
set -eu

KUBE_VERSION=${KUBE_VERSION:-1.35.3}
HELM=${HELM:-helm}
root=$(cd "$(dirname "$0")/.." && pwd)

# A throwaway Helm configuration, so this neither reads nor changes the
# caller's repositories.
work=$(mktemp -d)
trap 'rm -r "$work"' EXIT
export HELM_REPOSITORY_CONFIG="$work/repositories.yaml" HELM_REPOSITORY_CACHE="$work/cache" HELM_CACHE_HOME="$work/cache"

(cd "$root" && go run ./hack/cilium-values "$work" examples/*.yaml)

status=0
for v in $(cat "$work/versions"); do
  for values in "$work"/*.values.yaml; do
    label="cilium $v with $(basename "$values" .values.yaml)"
    if ! "$HELM" template cilium cilium --repo https://helm.cilium.io --version "$v" \
      --kube-version "$KUBE_VERSION" --namespace kube-system -f "$values" >"$work/out.yaml" 2>"$work/err"; then
      echo "FAIL: $label: helm template failed:" >&2
      cat "$work/err" >&2
      status=1
      continue
    fi
    if grep -n -e hydra-sentinel-cluster -e 10.255.254.0/24 -e '"65000"' "$work/out.yaml" >&2; then
      echo "FAIL: $label: a sentinel reached the rendered chart" >&2
      status=1
      continue
    fi
    echo "ok: $label ($(grep -c '^kind:' "$work/out.yaml") objects)"
  done
done
exit "$status"
