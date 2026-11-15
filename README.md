# hydra

`hydra` is the command-line tool for **Project Hydra**, an open-source on-prem Kubernetes
lifecycle platform built on [Cluster API](https://cluster-api.sigs.k8s.io/), with libvirt/KVM as its
infrastructure provider
([`cluster-api-provider-hydra`](https://github.com/Petatron/cluster-api-provider-hydra)).

You describe a cluster in one file, `hydra.yaml`, and `hydra` turns it into Cluster API resources.
The CLI is a **stateless orchestrator**: every cluster's desired and actual state lives in
Kubernetes, never on your machine. There is no local database, cache or inventory, and an
interrupted command can always be re-run.

> **Status:** early. `hydra cluster render` works today. `validate`, `bootstrap`, `cluster create`
> and the self-hosting pivot are next.

## Install

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

## Pinned components

`hydra version` prints them: Kubernetes `v1.35.3` (default), Cilium `1.19.1`, kube-vip `v0.8.9`.
They change only in a hydra release.

## Contributing

See [`AGENTS.md`](AGENTS.md) for layout and conventions. `make test lint verify` must pass.
Licensed under [Apache 2.0](LICENSE).
