#!/bin/sh
# Renders the Cilium chart with the values hydra puts in each cni: hydra
# example's HelmChartProxy, at every chart version hydra allows
# (internal/addons). Cluster API Add-on Provider Helm installs exactly these
# values, so this fails here, not in a cluster, when a render fails, still
# carries a sentinel, or does not carry the cluster's own values into the
# chart: the cluster name, pod CIDR and MTU in cilium-config, and the
# operator's replicas, which must change exactly the two lines the chart
# derives from them (the replica count and the rollout's maxUnavailable).
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
while read -r v; do
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
    { read -r name; read -r cidr; read -r mtu; read -r replicas; } <"${values%.values.yaml}.expect"
    for want in "cluster-name: \"$name\"" "cluster-pool-ipv4-cidr: \"$cidr\"" "mtu: \"$mtu\"" "  replicas: $replicas"; do
      if ! grep -qxF -- "  $want" "$work/out.yaml" && ! grep -qxF -- "$want" "$work/out.yaml"; then
        echo "FAIL: $label: the chart did not render $want" >&2
        status=1
      fi
    done
    # A change to the values alone must restart the agents and the operator
    # (PET-105): both pod templates carry cilium-config's checksum, and a
    # different MTU changes it on both.
    sums() { grep -c 'cilium.io/cilium-configmap-checksum:' "$1" || true; }
    if [ "$(sums "$work/out.yaml")" != 2 ]; then
      echo "FAIL: $label: want cilium-config's checksum on the agent and operator pod templates, found $(sums "$work/out.yaml")" >&2
      status=1
    fi
    "$HELM" template cilium cilium --repo https://helm.cilium.io --version "$v" \
      --kube-version "$KUBE_VERSION" --namespace kube-system -f "$values" --set "MTU=$((mtu - 1))" >"$work/mtu.yaml"
    changed=$(diff "$work/out.yaml" "$work/mtu.yaml" | grep -c '^>.*cilium.io/cilium-configmap-checksum:' || true)
    if [ "$changed" != 2 ]; then
      echo "FAIL: $label: a different MTU changed the checksum on $changed pod templates, want 2 (agents and operator)" >&2
      status=1
    fi
    # The other operator replica count must change only what the chart
    # derives from it; anything more would differ from the canonical values.
    other=$((3 - replicas))
    "$HELM" template cilium cilium --repo https://helm.cilium.io --version "$v" \
      --kube-version "$KUBE_VERSION" --namespace kube-system -f "$values" --set "operator.replicas=$other" >"$work/other.yaml"
    diff "$work/out.yaml" "$work/other.yaml" | grep '^[<>]' | sed 's/^[<>] *//' | sort -u >"$work/diff" || true
    printf '%s\n' "maxUnavailable: 100%" "maxUnavailable: 50%" "replicas: 1" "replicas: 2" >"$work/want-diff"
    if ! cmp -s "$work/diff" "$work/want-diff"; then
      echo "FAIL: $label: operator.replicas changes more than the replica count and maxUnavailable:" >&2
      cat "$work/diff" >&2
      status=1
    fi
    echo "ok: $label ($(grep -c '^kind:' "$work/out.yaml") objects; $name, $cidr, MTU $mtu, operator $replicas)"
  done
done <"$work/versions"
exit "$status"
