# hydra

[![CI](https://github.com/Petatron/hydra/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/ci.yml)
[![E2E](https://github.com/Petatron/hydra/actions/workflows/e2e.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/e2e.yml)
[![Cilium chart](https://github.com/Petatron/hydra/actions/workflows/cilium-chart.yml/badge.svg?branch=main)](https://github.com/Petatron/hydra/actions/workflows/cilium-chart.yml)
[![Coverage](https://codecov.io/gh/Petatron/hydra/branch/main/graph/badge.svg)](https://codecov.io/gh/Petatron/hydra)
[![Go](https://img.shields.io/github/go-mod/go-version/Petatron/hydra?logo=go&logoColor=white)](./go.mod)
[![Cluster API](https://img.shields.io/badge/Cluster%20API-v1.14-326CE5?logo=kubernetes&logoColor=white)](https://cluster-api.sigs.k8s.io/)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](./LICENSE)

`hydra` is the command-line tool for **Project Hydra**, an open-source on-prem Kubernetes
lifecycle platform built on [Cluster API](https://cluster-api.sigs.k8s.io/), with libvirt/KVM as its
infrastructure provider
([`cluster-api-provider-hydra`](https://github.com/Petatron/cluster-api-provider-hydra)).

You describe a cluster in one file, `hydra.yaml` (its control plane, node pools, CNI and platform
addons), and `hydra` turns it into Cluster API resources, keeps the live cluster matching it, and
removes what you take out of it, within safe limits.
The CLI is a **stateless orchestrator**: every cluster's desired and actual state lives in
Kubernetes, never on your machine. There is no local database, cache or inventory, and an
interrupted command can always be re-run.

> **Status:** early, latest release v0.6.0. `validate`, `render`, `bootstrap`,
> `cluster create/apply/plan/status/delete`, platform addons (`spec.addons`), Cilium through CAAPH
> (`cni: hydra`, `spec.network.ciliumVersion`), the self-hosting pivot
> and self-hosted teardown work today.

## Install

Release binaries for Linux and macOS (amd64 and arm64), with SHA256 sums in `checksums.txt`, are on
the [releases page](https://github.com/Petatron/hydra/releases). For example, on Linux amd64:

```bash
v=v0.6.0; os=linux; arch=amd64
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

# 3. Day 2: change hydra.yaml (pool sizes, an addon), preview, apply, against the cluster itself
hydra cluster plan   -f hydra.yaml --management-kubeconfig ./<name>.kubeconfig
hydra cluster apply  -f hydra.yaml --management-kubeconfig ./<name>.kubeconfig
hydra cluster status -f hydra.yaml --management-kubeconfig ./<name>.kubeconfig

# 4. Tear it down: move it into a bootstrap cluster, delete it there, remove kind
hydra cluster delete -f hydra.yaml --self-hosted
```

- `create --self-hosted` refuses to build when it cannot show the cluster does not already exist
  (this machine cannot reach the endpoint, or Hydra chooses it). Pass `--fresh` once you have
  checked.
- `apply` also removes what you took out of `hydra.yaml`: a dropped addon is uninstalled, a removed
  pool only with `--prune-pools=<pool>`. `plan` shows each one first; see
  [Create, inspect and delete a cluster](#create-inspect-and-delete-a-cluster).
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
| `spec.network.cni` | `hydra` | `hydra` installs Cilium through CAAPH; `external` installs no CNI, for a CNI managed elsewhere (see below) |
| `spec.network.mtu` | `1500` | Cilium MTU. Only with `cni: hydra` |
| `spec.network.ciliumVersion` | `1.19.1` | Cilium chart version, one hydra allows (`1.19.1`, `1.19.11`); changing it upgrades Cilium. Only with `cni: hydra` |
| `spec.network.nodeInterface` / `vipInterface` | `enp1s0` / derived | Guest NICs for the Node address and the API VIP |
| `spec.controlPlane` | 3 × 4 vCPU / 8Gi / 40Gi | Replicas must be 1, 3 or 5 |
| `spec.controlPlane.schedulable` | `true` if no node pools | Removes the control-plane taint |
| `spec.nodePools[]` | — | `name`, `replicas` or `autoscaling: {min, max}`, size, `labels` (not in the `kubernetes.io` / `k8s.io` namespaces, which the kubelet refuses) |
| `spec.addons[]` | — | Platform addons from hydra's catalogue: `name`, optional `version` (one the catalogue allows), optional `params` (see below) |

## Render it

```bash
hydra cluster render -f hydra.yaml            # to stdout
hydra cluster render -f hydra.yaml -o out/    # to out/<name>.yaml
```

The output contains the HydraCluster, Cluster, KubeadmControlPlane (with kube-vip holding the API
endpoint), a MachineHealthCheck, a MachineDeployment for each node pool, and a
**HelmChartProxy that installs Cilium** once the cluster's API is up.

Cilium is configured from [`internal/addons/cilium-values.yaml`](internal/addons/cilium-values.yaml),
the canonical values for every Hydra cluster. It is a template: the settings every cluster shares
are cluster-pool IPAM, a VXLAN tunnel, kube-proxy kept, and Hubble certificates issued in the
cluster by a CronJob. Four values are sentinels filled in per cluster: the cluster name
(`metadata.name`), the pod CIDR (`podCIDR`), the MTU (`mtu`), and the operator's replicas, which are
2 when the cluster always has at least two nodes (control plane plus each pool's fixed size or
autoscaling minimum) and 1 otherwise. To install Cilium another way (with `cni: external`), copy the
shared settings and set those four from the cluster's hydra.yaml, so a later hand-over changes
nothing. Never pass the file to Helm unchanged: it would install the sentinels.

### Cilium: `cni: hydra`

`cni: hydra` (the default) renders the HelmChartProxy `<cluster>-cilium`, which
[Cluster API Add-on Provider Helm](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm)
(CAAPH) installs on the cluster and keeps at `spec.network.ciliumVersion` (ADR-002). So the
management cluster needs CAAPH: `hydra bootstrap` installs it, and `create` refuses without it.

- **The proxy selects the Cluster by its `cni.hydra.io/cilium` label**, valued with the cluster's
  name. hydra sets the label and never removes it, because **removing it uninstalls Cilium from the
  running cluster**, and nodes still read Ready afterwards. Do not remove it by hand.
- **Upgrading Cilium is a hydra.yaml change:** set `ciliumVersion` and run `apply` (`plan` shows the
  change first). CAAPH upgrades the same release in place. The allowed versions are in the
  catalogue (`internal/addons`); `hack/check-cilium.sh` renders the chart with hydra's values at
  each of them, weekly and on every change.
- `create`, `apply` and the pivot wait for CAAPH to report Cilium installed at the version asked for,
  **and then for its agents and operator to roll out at that version.** CAAPH reports the release
  Ready once Helm has applied it, before any agent restarts, so hydra reads the `kube-system/cilium`
  DaemonSet and `cilium-operator` Deployment through the cluster's kubeconfig Secret.
  `status` shows `CNI installing` with CAAPH's reason, then `CNI rolling out (n/m)` (agents updated
  and available, of those desired), then `CNI ready`; `CNI uninstalled` if the Cluster lost its
  label. A pivot refuses to start, or to finish, on a cluster without its CNI rolled out.
- **Reading the rollout needs the workload cluster's API**, at the address in its kubeconfig Secret
  (a self-hosted cluster is read through the management connection, `--bastion` included). Where
  hydra cannot reach it:
  - a `create` or `apply` that **changed the CNI** (created its proxy, or changed `ciliumVersion`
    or its values) fails after a short retry with `CNI rollout unknown`, once everything else is
    done: hydra cannot prove Cilium rolled out. The change itself is applied. hydra records it on
    the proxy (annotation `hydra.io/cni-rollout-pending`), so **every later `create`, `apply`,
    pivot and `status` stays strict**, unrelated changes included, until a `create` or `apply`
    run from where the API is reachable sees the rollout complete and removes the record
    (`status` only reads it, and shows `CNI rollout unknown, change pending verification since
    generation N`). `--bastion` forwards only the management API, so for a cluster that does
    not manage itself that apply must run on a host that routes to the API address in the
    cluster's kubeconfig Secret. Someone who has checked the rollout another way can remove the
    record by hand on the management cluster:
    `kubectl -n <namespace> annotate helmchartproxy <cluster>-cilium hydra.io/cni-rollout-pending-`. An apply
    interrupted between writing the proxy and recording the change leaves no record;
  - any other `create` or `apply`, and `status`, count the CNI ready on its release alone and say
    so: `CNI ready (rollout unverified: <error>)`;
  - a pivot refuses: it is run by hand, from where the cluster is reachable.
- hydra trusts the release only once CAAPH has carried the proxy's latest spec into it (the proxy's
  `observedGeneration` and its `HelmReleaseProxySpecsUpToDate` condition), so a change to Cilium's
  values alone is not reported done by the old release.
- **Pin `ciliumVersion`.** Without it a cluster takes the catalogue's default, so a hydra release
  that moves the default upgrades Cilium on your next `apply` (`plan` shows it first), and `upgrade`
  refuses until that has been applied.
- **hydra's Cilium is never put on a running cluster without your say-so.** For a cluster that
  already runs, `create` and `apply` refuse `cni: hydra` unless hydra's proxy installed its CNI:
  when the Cluster lacks the label, or carries it but hydra's proxy never installed anything (built
  or adopted with the label, or an older hydra's ClusterResourceSet removed). Check what runs there,
  then rerun with **`--adopt-cni`**: hydra's Cilium is installed over it, taking over Cilium objects
  that have no Helm release. `plan --adopt-cni` previews it. A new cluster, or one whose API has not
  come up yet, needs no flag.
- The proxy sets `takeOwnership`, so CAAPH adopts Cilium objects that exist without a Helm release
  (a cluster whose Cilium was installed another way) instead of failing. CAAPH applies options only
  when it first installs, so this is set from the first render.
- **The CNI is never removed**: not by `apply`, not with `--allow-remove`, and `create`, `apply` and
  `upgrade` refuse to switch a cluster whose Cilium this proxy installs to `cni: external`, since
  the switch drops the label. `plan` shows the refusal.

#### Leaving hydra's Cilium

Handing Cilium to another tool without uninstalling it is a break-glass procedure, proven in kind
(PET-97) and not automated: with CAAPH running, deleting the proxy or the label uninstalls Cilium
even if its finalizers are removed first. On the management cluster:

1. Scale CAAPH to 0 (`kubectl -n caaph-system scale deployment caaph-controller-manager --replicas 0`).
2. Set every webhook in `caaph-validating-webhook-configuration` and
   `caaph-mutating-webhook-configuration` to `failurePolicy: Ignore`; with CAAPH down they would
   otherwise block the next step.
3. Remove the finalizers from the cluster's Cilium HelmReleaseProxy and HelmChartProxy, delete
   both, and remove the Cluster's `cni.hydra.io/cilium` label.
4. Restore the webhooks to `Fail` and scale CAAPH back to 1.
5. Set `cni: external` in hydra.yaml. Cilium keeps running, with its Helm release, for the new owner
   to adopt.

#### Clusters on the old ClusterResourceSet

hydra v0.5 and earlier installed Cilium through a ClusterResourceSet (`<cluster>-cilium`), which
left no Helm release on the cluster. On such a cluster this hydra refuses `create`, `apply`,
`upgrade` and the pivot with `cni: hydra`, and never switches it over on its own. To keep its
Cilium as it is, set `cni: external`. To move it under CAAPH:

1. Check that the running Cilium matches the canonical values above at the version you will set in
   `ciliumVersion`, so the takeover changes nothing.
2. Delete the old ClusterResourceSet and its ConfigMap on the management cluster
   (`kubectl -n <ns> delete clusterresourceset,configmap <cluster>-cilium`). This leaves Cilium
   running: a ClusterResourceSet never deletes what it applied.
3. `hydra cluster plan --adopt-cni`, then `hydra cluster apply --adopt-cni`. CAAPH takes the
   existing objects over (`takeOwnership`), and any value that differs is set right at CAAPH's next
   resync (up to 10 minutes).

This takeover was proven in kind for a Cilium without a Helm release (PET-97), not yet for one
applied by a ClusterResourceSet on hardware.

### A CNI managed elsewhere: `cni: external`

With `spec.network.cni: external`, the output has no Cilium HelmChartProxy and no
`cni.hydra.io/cilium` label on the Cluster; nothing else changes. Use it when Argo CD, Helm or a
person owns the CNI, including a cluster built before hydra managed it.

- The external CNI must use `podCIDR` as its pod address pool: kubeadm is given that CIDR either way.
- Nodes stay NotReady until the CNI is installed, so `create` keeps waiting until it is. Install it
  while `create` waits, or pass `--wait=false` and follow with `status`.
- `create` refuses `cni: hydra` for an existing Cluster that lacks the CNI label (or carries it with
  a value other than the cluster's name), because hydra's Cilium would then be installed on top of
  the CNI already there, unless `--adopt-cni` says to (see above). `plan` warns about the same case.
- A cluster whose Cilium hydra installs through CAAPH cannot be switched to `external` (see above).
  For one still on the old ClusterResourceSet, `external` leaves Cilium running: the set applied it
  once and never removes it. Server-side apply drops the Cluster's `cni.hydra.io/cilium` label only
  when hydra's field manager is its sole owner. A label that stays keeps the old set selecting the
  cluster, so `create` fails after applying (rerunning is safe), `upgrade` refuses before rolling
  any machine, and `plan` warns when its dry-run keeps the label; all three print the
  `kubectl label` command that removes it, aimed at the management cluster.

### Platform addons: `spec.addons`

```yaml
spec:
  addons:
    - name: metrics-server            # from hydra's catalogue
      version: 3.14.0                 # optional; one the catalogue allows
      params:
        kubeletInsecureTLS: true      # optional; each addon declares its own
```

Each addon becomes a `HelmChartProxy` (`<cluster>-<addon>`) that
[Cluster API Add-on Provider Helm](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm)
(CAAPH) installs on the cluster and keeps in step with hydra.yaml (ADR-002). `hydra bootstrap`
installs CAAPH; `create` refuses a hydra.yaml with addons on a management cluster without it, and
`status` shows each addon's state.

- Only catalogued addons, versions and params are accepted, checked by `validate`, `render` and
  `create`; raw Helm values are not. The catalogue is in
  [`internal/addons`](internal/addons/addons.go):

  | Addon | Versions | Params |
  |---|---|---|
  | `metrics-server` | `3.13.1`, `3.14.0` (default) | `kubeletInsecureTLS` (default `true`): kubeadm's kubelets serve self-signed certificates, which metrics-server cannot verify without this |

- A version change in hydra.yaml upgrades the addon on the next `create`. A change made on the
  workload cluster behind CAAPH's back is put right at CAAPH's next resync (up to 10 minutes).
- **Pin `version` for anything you care about.** An addon without one takes the catalogue's
  default, so a hydra release that moves the default upgrades the addon on your next `create`
  (`plan` shows the change first).
- `create` does not wait for addons: they settle after the cluster is Available. `status` shows
  each one, and the reason when it is not Ready. A pivot waits for every addon to be Ready on the
  target before it deletes the bootstrap cluster.
- **Removing an addon from hydra.yaml and running `apply` uninstalls it**; `create` leaves its
  `HelmChartProxy` in place. Protected addons need `--allow-remove=<addon>` (see below); the CNI is
  never removed. Deleting a
  `HelmChartProxy` by hand, or removing the Cluster's `hydra.io/cluster` label, also uninstalls the
  addon within seconds, and CAAPH has no way to keep it.

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
the Hydra provider and Cluster API Add-on Provider Helm (for `spec.addons`) at the versions this
CLI pins. It then writes the hypervisor credentials
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
hydra cluster plan   -f hydra.yaml   # what create/apply would change, against the live management cluster
hydra cluster create -f hydra.yaml   # apply, wait for Available, write ./<name>.kubeconfig
hydra cluster apply  -f hydra.yaml   # create, then prune what hydra.yaml no longer lists
hydra cluster status -f hydra.yaml
hydra cluster delete -f hydra.yaml   # asks for the cluster name; --yes skips that
```

These act on `--management-kubeconfig`, or else on the cluster's bootstrap kind cluster; your
current kubectl context is never used.

### A management cluster behind a jump host: `--bastion`

When the management cluster's API is reachable only from inside its network, as with a private EKS
or GKE cluster, go through an SSH jump host:

```bash
hydra cluster plan -f hydra.yaml --management-kubeconfig ./mgmt.kubeconfig --bastion ops@jump.example.com
```

`--bastion user@host[:port]` works on `create`, `apply`, `plan`, `status`, `delete` and `upgrade`,
and needs `--management-kubeconfig`. It does the following:

- hydra starts the system `ssh -N -L` to the API server in the kubeconfig's current context, talks to
  the API through a local port, and stops ssh when the command ends, or when hydra gets SIGTERM,
  SIGHUP or SIGINT, even one sent to hydra alone (on Linux, also if hydra is killed outright). ssh's connection sharing (`ControlMaster`) is
  turned off for the tunnel, so no shared master keeps the forward open afterwards. Your agent is
  never forwarded to the bastion (`ForwardAgent=no`), and a bastion whose `~/.ssh/config` entry has
  a `LocalForward`, `RemoteForward` or `DynamicForward` is refused, since those would stay open
  beside the tunnel; give `--bastion` a `Host` entry without them;
- TLS is still verified against the API server's own name (the kubeconfig's `tls-server-name`, or
  else the server's host), so the jump host only relays encrypted traffic. A kubeconfig with
  `insecure-skip-tls-verify` or `proxy-url` is refused;
- if the tunnel drops while a command runs, hydra says so with ssh's reason. The command then fails
  or times out as for any unreachable API; run it again, which is always safe;
- because it is your own `ssh`, `~/.ssh/config` applies: keys, agent, `known_hosts`, `ProxyJump`,
  and a `ProxyCommand` through Tailscale, Cloudflare Access, AWS SSM or GCP IAP;
- ssh runs with `BatchMode=yes` and never prompts, so a missing key or an unknown host key fails fast
  with ssh's own message (accept the host key once by hand, or pin it in `known_hosts`);
- safety checks that ask which cluster the kubeconfig belongs to (such as `delete` refusing a
  self-hosted cluster) still use the kubeconfig as given, never the tunnel's `127.0.0.1`.

Only the management API goes through the tunnel; hydra does not contact the workload cluster's API.
`create --self-hosted` and `delete --self-hosted` refuse it, and `pivot` does not take it. The
`kubectl --kubeconfig …` commands hydra suggests in its messages do not go through the tunnel; run
them from a host that reaches the API, or through your own `ssh -L`.

- `create` server-side applies the rendered objects (field manager `hydra`) and reports the
  controllers' own conditions until the cluster is Available, its CNI applied (or, with
  `cni: external`, its nodes Ready), and any rollout finished. `--timeout` ends the wait, not the creation; rerunning is always safe.
- It refuses to take over objects of the same name that another cluster owns, or that hydra did
  not make (`--adopt` allows the latter), and refuses while the cluster is being deleted.
- `plan` diffs against the live cluster only, and lists objects labelled for the cluster that
  `hydra.yaml` no longer produces, each with what `apply` would do with it: prune (`-`), keep (`?`)
  or refuse (`!`). `create` never deletes them; `apply` prunes what is safe:

  | No longer in hydra.yaml | `apply` |
  |---|---|
  | hydra's `HelmChartProxy` for a removed addon (`<cluster>-<addon>`; deleting it uninstalls the addon) | deletes it; a protected or uncatalogued addon only with `--allow-remove=<addon>`, otherwise apply refuses before writing anything |
  | Any other proxy labelled for the cluster (a duplicate, one made by hand) | refuses unless named as `--allow-remove=proxy/<name>`: a duplicate shares the release of the addon hydra.yaml still lists |
  | A superseded machine or bootstrap template | deletes it once no machine, MachineSet or control plane uses it (a later apply removes one still in use) |
  | A node pool (`MachineDeployment`) | keeps it unless named with `--prune-pools=<pool>`, because deleting it deletes its machines and VMs; a value that names more than one removed pool (a pool name can equal another pool's MachineDeployment name) refuses, and `plan` prints a value that names each alone |
  | The CNI's `HelmChartProxy` (`<cluster>-cilium`), or any proxy labelled for the CNI | refuses, even with `--allow-remove`: removing it uninstalls Cilium from the running cluster |
  | The control plane, `HydraCluster`, health check, the old CNI ClusterResourceSet and ConfigMap | never |

  `plan` takes `--prune-pools` and `--allow-remove` too, to show exactly what `apply` with them would
  do. Like `create`, `apply` refuses a Kubernetes version change (use `upgrade`). An object that
  changes while apply runs is left, and apply exits non-zero; run it again. Do not run two applies
  for one cluster at once, nor roll a pool back by hand while one runs: hydra keeps no lock
  (ADR-001), and a template that becomes used again between apply's check and its delete could be
  removed.
- **Replacing a control-plane machine keeps the API up (PET-88).** The control-plane template asks
  the provider (v0.2.0 or later) to run `/usr/local/sbin/hydra-pre-drain` in the guest before
  Cluster API drains the machine. If that node holds the control-plane VIP, the script stops
  kube-vip there, waits until another control-plane node holds kube-vip's lease, then drops the
  VIP from the node. That happens before the node's etcd member is removed, so the API stays
  served throughout.
- **Existing clusters roll once.** A cluster created with an earlier hydra rolls its control plane
  once on its next `create`, because the control-plane template changed. That migration roll is
  not protected: the machines it deletes came from the old template and have no hook. Every roll
  after it is.
- **Upgrade the provider first.** Against an older provider, `create` refuses with the field the
  provider does not know, and changes nothing of that object.
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
and for `bootstrap`, kind node `v1.35.8`, cert-manager `v1.21.1`, Cluster API `v1.14.0`, the Hydra
provider `v0.2.0` and Cluster API Add-on Provider Helm `v0.6.6`. Addon chart versions are in the
catalogue (`internal/addons`).
They change only in a hydra release.

## Contributing

See [`AGENTS.md`](AGENTS.md) for layout and conventions. `make test lint verify` must pass.
`make e2e` (needs Docker) bootstraps a kind management cluster, server-side dry-runs every example
against it, and runs `hydra cluster apply` through creating, refusing and pruning; CI runs it on
every PR that touches code.
Licensed under [Apache 2.0](LICENSE).
