# hydra

[![CI](https://github.com/Petatron/hydra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/ci.yml)
[![E2E](https://github.com/Petatron/hydra/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/e2e.yml)
[![Cilium drift](https://github.com/Petatron/hydra/actions/workflows/cilium-drift.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/cilium-drift.yml)
[![Coverage](https://codecov.io/gh/Petatron/hydra/branch/main/graph/badge.svg)](https://codecov.io/gh/Petatron/hydra)
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

> **Status:** early, latest release v0.2.0. `validate`, `render`, `bootstrap`,
> `cluster create/plan/status/delete`, the self-hosting pivot and self-hosted teardown work today.

## Install

Release binaries for Linux and macOS (amd64 and arm64), with SHA256 sums in `checksums.txt`, are on
the [releases page](https://github.com/Petatron/hydra/releases). For example, on Linux amd64:

```bash
v=v0.2.0; os=linux; arch=amd64
curl -fsSLO https://github.com/Petatron/hydra/releases/download/$v/hydra-$os-$arch
curl -fsSLO https://github.com/Petatron/hydra/releases/download/$v/checksums.txt
grep " hydra-$os-$arch\$" checksums.txt | sha256sum -c -   # macOS: shasum -a 256 -c -
mkdir -p ~/.local/bin && install -m 0755 hydra-$os-$arch ~/.local/bin/hydra
~/.local/bin/hydra version   # add ~/.local/bin to PATH to call it as plain `hydra`
```

For the latest commit on main, `go install github.com/Petatron/hydra/cmd/hydra@main`. For your own
checkout, `go install ./cmd/hydra`, or `make build` (the binary lands in `bin/hydra`).

## Quickstart: a self-hosted cluster and back

**What the machine running hydra needs:**

- A container runtime kind can use: Docker, Podman or nerdctl, usable without sudo.
- Network reach, from that runtime's containers, to the hypervisor's libvirt TLS port (16514) and
  to the cluster's control-plane endpoint. The bootstrap cluster's controllers run in a container
  and dial from there; `hydra validate` checks this.
- **On a host that is itself a Kubernetes node or a hypervisor**, install Docker so it cannot change
  the iptables `FORWARD` policy, which those hosts rely on. Before installing Docker:
  `echo '{"ip-forward-no-drop": true}' | sudo tee /etc/docker/daemon.json`.
- The hypervisor's TLS client credentials in a directory: `cacert.pem`, `clientcert.pem`,
  `clientkey.pem`.

**The whole path:**

```bash
# 1. Describe the cluster in hydra.yaml (see examples/), then check the host and hypervisor
hydra validate -f hydra.yaml --hypervisor-credentials ./libvirt-pki

# 2. Build it and make it manage itself (bootstrap kind, create, pivot, delete kind)
hydra cluster create -f hydra.yaml --self-hosted --hypervisor-credentials ./libvirt-pki

# 3. Day 2: change hydra.yaml (pool sizes, say), preview, apply, against the cluster itself
hydra cluster plan   -f hydra.yaml --management-kubeconfig ./<name>.kubeconfig
hydra cluster create -f hydra.yaml --management-kubeconfig ./<name>.kubeconfig
hydra cluster status -f hydra.yaml --management-kubeconfig ./<name>.kubeconfig

# 4. Tear it down: move it into a bootstrap cluster, delete it there, remove kind
hydra cluster delete -f hydra.yaml --self-hosted
```

- `create --self-hosted` refuses to build when it cannot show the cluster does not already exist
  (this machine cannot reach the endpoint, or Hydra chooses it). Pass `--fresh` once you have
  checked.
- Every step can be interrupted and run again: each command finds where things are and continues.
  A move interrupted part way leaves the Cluster on both sides; the command says so, and
  `--finish-split` finishes it.

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

## Check the host first

```bash
hydra validate -f hydra.yaml --hypervisor-credentials ./libvirt-pki
```

`validate` checks, read-only, what bootstrap and create will need:
- a container runtime kind can use;
- the hypervisor's libvirt TLS listener (16514), and that its plaintext one (16509) is closed;
- the client certificate (keyEncipherment, validity) and the TLS login;
- the storage pool and its free space, and the base image;
- each bridge: up, and no port holding an address of its own;
- the control-plane endpoint: free, on a network that is reachable (for a managed network,
  through its gateway).

Reachability is judged from where Cluster API's controllers run: inside the bootstrap cluster's
kind node when it exists, else from this machine. Silence from the endpoint proves it free only
once its network is shown reachable; otherwise the check warns rather than passes. Every result is
PASS, WARN or FAIL with a fix, and the command exits non-zero on any FAIL. Without
`--hypervisor-credentials` the checks that need a libvirt login are reported as not checked.

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

## Upgrade Kubernetes

```bash
hydra cluster upgrade -f hydra.yaml --to v1.36.6 --dry-run   # the next stage and what it changes
hydra cluster upgrade -f hydra.yaml --to v1.36.6             # control plane, then each pool
```

Changing `kubernetesVersion` and rerunning `create` would roll the control plane and every pool at
once, so a worker could briefly run a kubelet newer than the API server. `create` refuses that
change on a running cluster, and `plan` flags it. `upgrade` does it in order instead:

- **Control plane first.** It sets the version on the KubeadmControlPlane only, and waits until
  every control-plane machine runs it and the cluster is Available.
- **Then each pool**, in `hydra.yaml`'s order: the pool's new KubeadmConfigTemplate and its
  MachineDeployment. Machines being drained are shown, including pods a PodDisruptionBudget
  holds; drains are waited on, never forced.
- **Resumable.** Each run reads the live versions and continues from there. An interrupted
  upgrade is finished by running the same command again; a rollout already under way is waited
  on, never re-applied.
- **It refuses** a downgrade, a jump of more than one minor version, a cluster that is not settled,
  `hydra.yaml` edits beyond the version (apply them with `create` first), a version whose
  kubelet, kubeadm and kubectl pkgs.k8s.io does not carry, and, when a pool is autoscaled, a minor
  with no Cluster Autoscaler release. Each refusal says what to do. `--skip-remote-checks` skips the two internet checks.
- Clusters with `bootstrap: prebaked` images are not supported yet: the image carries one version.

`hydra.yaml` may say the running version or the target while the upgrade runs. Afterwards, set
`kubernetesVersion` to the target; then `plan` shows no changes. A control plane cannot go back a
minor version: the way back is restoring etcd from a backup taken before the upgrade.

## Make a cluster manage itself

```bash
hydra cluster create -f hydra.yaml --self-hosted   # bootstrap kind, create, pivot, delete kind
hydra cluster pivot  -f hydra.yaml                 # or pivot an existing cluster on its own
```

`pivot` moves the cluster's Cluster API objects onto the cluster itself with `clusterctl move`.
The VMs are untouched; the cluster's own Hydra provider takes them over.

- Before moving it checks that the cluster is Available with no rollout, has its namespace to
  itself (no other Cluster, and nothing else `clusterctl move` could carry), is not paused, every machine has recorded its identity, the credentials Secret and
  Hydra CRDs carry the labels `clusterctl move` needs, and a dry run passes.
- It proves the target is the cluster itself by its certificate authority before moving. Run
  against a cluster that already manages itself, it says so and moves nothing.
- Commands it prints use real kubeconfig files: the cluster's is written to `./<name>.kubeconfig`
  when it came only from the Secret, and the bootstrap cluster's comes from
  `hydra bootstrap kubeconfig`.
- After moving it waits for the cluster to verify on itself. If that fails, the cluster stays on
  the target and the error says how to inspect it and how to undo the move by hand. pivot never
  moves a cluster back on its own: a VM may already have been taken over, and moving that machine
  back would strand it.
- Rerunning is safe: it finds where the objects are. A cluster already on the target is only
  verified. A paused cluster is reported with the commands to run; pivot never unpauses one on its
  own. A copy on both sides (a move interrupted part way) is reported too, and `--finish-split`
  finishes it from the takeover evidence, never from which command is running (on the source
  every machine holds its VM, on the copy clusterctl was building none does). Interrupted while
  creating (the source intact), it unpauses the source and moves again. Interrupted while deleting
  (the target complete, the source depleted), it never resumes the source, whose controllers would
  recreate the machines already deleted: it removes the source's leftovers as clusterctl does,
  finalizers stripped so nothing is torn down, and resumes the complete copy. It refuses when the
  evidence is unclear, or says the split came from a move the other way.
- `create --self-hosted` never builds a second copy: if the cluster's kubeconfig reaches it
  already managing itself, there is nothing to do; if it reaches a cluster that is running but not
  yet self-hosted, or the endpoint already answers, it refuses. When hydra cannot check (Hydra
  chooses the endpoint, or this machine cannot reach it) and nothing local shows whether the cluster
  exists, pass `--fresh`.
- After a pivot, pass the cluster's kubeconfig as `--management-kubeconfig` to every command.

### Tear a self-hosted cluster down

```bash
hydra cluster delete -f hydra.yaml --self-hosted [--cluster-kubeconfig ./<name>.kubeconfig]
```

A cluster cannot delete itself (its controllers run on the machines being deleted), so this moves it
into its bootstrap kind cluster and deletes it from there:

- It proves the kubeconfig is the cluster itself, and that the cluster is hydra's to delete
  (`--adopt` for one hydra did not make), before anything moves.
- It brings kind up without hypervisor credentials (they come back with the move), and refuses a
  kind whose copy of the namespace already holds something.
- It pauses the cluster's MachineHealthChecks, moves it, and waits until kind's provider has taken
  every VM over: deleting a machine before that would wedge it.
- It deletes the cluster from kind, then removes `./<name>.kubeconfig` (only if it is this
  cluster's) and kind.
- It leaves a record in kind before moving, so an interrupted teardown is finished by running the
  same command again; when the cluster cannot be reached and no record shows a teardown, it
  refuses rather than guess.
- It does not require the cluster to be healthy, but its API must answer: nothing can be moved from
  a dead control plane.

## Pinned components

`hydra version` prints them: Kubernetes `v1.35.3` (default), Cilium `1.19.1`, kube-vip `v0.8.9`;
and for `bootstrap`, kind node `v1.35.8`, cert-manager `v1.21.1`, Cluster API `v1.14.0` and the Hydra
provider `v0.1.0`.
They change only in a hydra release.

## Contributing

See [`AGENTS.md`](AGENTS.md) for layout and conventions. `make test lint verify` must pass.
Licensed under [Apache 2.0](LICENSE).
