# hydra

[![CI](https://github.com/Petatron/hydra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/ci.yml)
[![E2E](https://github.com/Petatron/hydra/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/e2e.yml)
[![Cilium drift](https://github.com/Petatron/hydra/actions/workflows/cilium-drift.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/cilium-drift.yml)
[![Go](https://img.shields.io/github/go-mod/go-version/Petatron/hydra?logo=go&logoColor=white)](./go.mod)
[![Cluster API](https://img.shields.io/badge/Cluster%20API-v1.14-326CE5?logo=kubernetes&logoColor=white)](https://cluster-api.sigs.k8s.io/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](./LICENSE)

`hydra` is the command-line tool for **Project Hydra**, an open-source on-prem Kubernetes
lifecycle platform built on [Cluster API](https://cluster-api.sigs.k8s.io/), with libvirt/KVM as its
infrastructure provider
([`cluster-api-provider-hydra`](https://github.com/Petatron/cluster-api-provider-hydra)).

You describe a cluster in one file, `hydra.yaml`, and `hydra` turns it into Cluster API resources.
The CLI is a **stateless orchestrator**: every cluster's desired and actual state lives in
Kubernetes, never on your machine. There is no local database, cache or inventory, and an
interrupted command can always be re-run.

> **Status:** early. `render`, `bootstrap` and `cluster create/plan/status/delete` work today.
> `validate` and the self-hosting pivot are next.

## Install

Release binaries for Linux and macOS (amd64 and arm64, with SHA256 sums in `checksums.txt`) are
published on the [releases page](https://github.com/Petatron/hydra/releases) for each tagged
version, once the first is tagged. Until then, or for an unreleased commit, build it:

```bash
go install github.com/Petatron/hydra/cmd/hydra@latest
```

or build from a checkout with `make build` (the binary lands in `bin/hydra`).

## Describe a cluster

```yaml
apiVersion: hydra.io/v1alpha1
kind: ClusterConfig
metadata:
  name: lab
spec:
  storagePool: vm-pool                 # libvirt pool for machine disks
  image:
    name: ubuntu-24.04-server-cloudimg-amd64.img   # must already be in that pool
  network:
    controlPlaneEndpoint: 192.168.15.11            # outside every DHCP range
    bridges: [br0]
  controlPlane:
    replicas: 3
    vcpus: 2
    memory: 6Gi
    diskSize: 40Gi
```

More complete examples, including a Hydra-managed network, per-cluster hypervisor credentials and
autoscaled node pools, are in [`examples/`](examples/).

| Field | Default | Notes |
|---|---|---|
| `metadata.namespace` | `default` | Management-cluster namespace for the objects |
| `spec.kubernetesVersion` | `v1.35.3` | |
| `spec.hypervisor` | (provider-wide) | `address` + `credentialsSecretName` (default `<name>-libvirt-pki`) |
| `spec.image.bootstrap` | `packages` | `packages` installs containerd and Kubernetes at boot; `prebaked` expects an image that carries them |
| `spec.network.controlPlaneEndpoint` | — | IPv4 or DNS name (port 6443 only). Required unless `managedNetwork` is set, in which case Hydra picks it |
| `spec.network.podCIDR` / `serviceCIDR` | `10.244.0.0/16` / `10.96.0.0/12` | The pod CIDR is also Cilium's IPAM pool and must hold a /24 per node; the service CIDR may be at most a /12 |
| `spec.network.mtu` | `1500` | Cilium MTU |
| `spec.network.nodeInterface` / `vipInterface` | `enp1s0` / derived | Guest NICs for the Node address and the API VIP |
| `spec.controlPlane` | 3 × 4 vCPU / 8Gi / 40Gi | Replicas must be 1, 3 or 5 |
| `spec.controlPlane.schedulable` | `true` if no node pools | Removes the control-plane taint |
| `spec.nodePools[]` | — | `name`, `replicas` or `autoscaling: {min, max}`, size, `labels` (not in the `kubernetes.io` / `k8s.io` namespaces, which the kubelet refuses) |

## Render it

```bash
hydra cluster render -f hydra.yaml            # to stdout
hydra cluster render -f hydra.yaml -o out/    # to out/<name>.yaml
```

The output contains the HydraCluster, Cluster, KubeadmControlPlane (with kube-vip holding the API
endpoint), a MachineHealthCheck, a MachineDeployment for each node pool, and a
**ClusterResourceSet that installs Cilium** once the cluster's API is up.

Machine and bootstrap templates are named after their content (`<cluster>-cp-1a2b3c4d`), so
changing a machine's size or a pool's labels produces a new template and a rolling replacement
rather than an in-place edit Cluster API would refuse or apply only to future machines.

`render` contacts no cluster, and the same input always gives byte-identical output, so you can
review the result, diff it between versions, or commit it to your own GitOps repository. It never
contains secrets: hypervisor credentials are referenced by Secret name only.

## Bootstrap a management cluster

```bash
hydra bootstrap -f hydra.yaml --hypervisor-credentials ./pki   # cacert.pem, clientcert.pem, clientkey.pem
hydra bootstrap kubeconfig -f hydra.yaml > bootstrap.kubeconfig
hydra bootstrap delete -f hydra.yaml
```

`bootstrap` creates a kind cluster named `hydra-bootstrap-<name>` on this machine (Docker, Podman
or nerdctl is needed, but no `kind` or `clusterctl` binary) and installs cert-manager, Cluster API
and the Hydra provider at the versions this CLI pins. It then writes the hypervisor credentials
Secret named in `spec.hypervisor`, labelled so that `clusterctl move` carries it to the cluster's
own control plane later. It takes about 75 seconds from nothing.

- `spec.hypervisor` is required: the temporary cluster has no manager-wide hypervisor settings.
- Nothing is recorded locally. Re-running continues an interrupted run, and a run with nothing
  left to do changes nothing. A provider installed at another version is reported, not changed.
- Your kubeconfig and current context are left alone; use `hydra bootstrap kubeconfig`.
- `delete` refuses while the kind cluster still manages a Cluster API Cluster, because that
  cluster's machines would be left running with nothing managing them. `--force` overrides.
- The controllers run inside the kind node container on this machine, so the hypervisor (TLS, port
  16514) and the new cluster's API endpoint must be reachable from that container, not only from
  this host. An address on `127.0.0.1`, or one only this host can route, will not work.
- `delete` and `kubeconfig` read only `metadata.name`, so a config that fails validation never
  blocks cleanup.
- clusterctl reads release metadata from GitHub; set `GITHUB_TOKEN` if you hit the anonymous
  API rate limit.

## Create, inspect and delete a cluster

```bash
hydra cluster plan   -f hydra.yaml   # what create would change, against the live management cluster
hydra cluster create -f hydra.yaml   # apply, wait for Available, write ./<name>.kubeconfig
hydra cluster status -f hydra.yaml
hydra cluster delete -f hydra.yaml   # asks for the cluster name; --yes skips that
```

These act on `--management-kubeconfig`, or else on the cluster's bootstrap kind cluster; your
current kubectl context is never used.

- `create` server-side applies the rendered objects (field manager `hydra`) and reports the
  controllers' own conditions until the cluster is Available, its CNI applied, and any rollout
  finished. `--timeout` ends the wait, not the creation; rerunning is always safe.
- It refuses to take over objects of the same name that another cluster owns, or that hydra did
  not make (`--adopt` allows the latter), and refuses while the cluster is being deleted.
- `plan` diffs against the live cluster only, and lists objects labelled for the cluster that
  `hydra.yaml` no longer produces (a removed pool, an old template). Neither command deletes them.
- `delete` shows which management cluster it will act on before asking, refuses to delete a
  cluster from itself (a self-hosted management cluster), and then removes everything labelled
  for the cluster once Cluster API has torn down its machines. The credentials Secret is kept.

## Pinned components

`hydra version` prints them: Kubernetes `v1.35.3` (default), Cilium `1.19.1`, kube-vip `v0.8.9`;
and for `bootstrap`, kind node `v1.35.8`, cert-manager `v1.21.1`, Cluster API `v1.14.0` and the Hydra
provider `v0.1.0`.
They change only in a hydra release.

## Contributing

See [`AGENTS.md`](AGENTS.md) for layout and conventions. `make test lint verify` must pass.
Licensed under [Apache 2.0](LICENSE).
