package cluster

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Petatron/hydra/internal/addons"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

var (
	hydraClusterGVK = schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: kindHydraCluster}
	mdGVK           = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "MachineDeployment"}
	// hcpGVK is CAAPH's per-addon object (ADR-002).
	hcpGVK = schema.GroupVersionKind{Group: addonsGroup, Version: "v1alpha1", Kind: render.KindHelmChartProxy}
)

// addonsGroup holds ClusterResourceSets and CAAPH's HelmChartProxies.
const addonsGroup = "addons.cluster.x-k8s.io"

// ownedKinds are every kind render produces that this CLI may find again by
// the hydra.io/cluster label: to report as orphaned, or to delete.
//
// delete removes them in two phases. withMachines first, waiting until they
// are gone (so are the machines and VMs); then afterMachines, which nothing
// running references any more. A HelmChartProxy deleted while its cluster
// still runs uninstalls the release there, so addons belong in the second
// phase only (a test keeps it so).
var (
	withMachines  = []schema.GroupVersionKind{hydraClusterGVK, kcpGVK, mdGVK}
	afterMachines = []schema.GroupVersionKind{
		{Group: capiGroup, Version: capiVersion, Kind: "MachineHealthCheck"},
		{Group: bootstrapGroup, Version: capiVersion, Kind: kindKubeadmConfigTemplate},
		{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate},
		{Group: addonsGroup, Version: capiVersion, Kind: "ClusterResourceSet"},
		{Group: "", Version: "v1", Kind: kindConfigMap},
		hcpGVK,
	}
	ownedKinds = slices.Concat(withMachines, afterMachines)
)

// CheckManagementCluster fails, before anything is written, when the target
// has no Cluster API: create must not leave a stray namespace on a cluster
// that is not a management cluster.
func (m *Manager) CheckManagementCluster() error {
	if _, err := m.C.RESTMapper().RESTMapping(clusterGVK.GroupKind(), clusterGVK.Version); err != nil {
		return fmt.Errorf("the target is not a Cluster API management cluster (no %s): %w", clusterGVK.GroupVersion(), err)
	}
	return nil
}

// CheckAddonProvider fails, before anything is written, when objs hold a
// HelmChartProxy but the management cluster has no Cluster API Add-on
// Provider Helm to reconcile it.
func (m *Manager) CheckAddonProvider(objs []*unstructured.Unstructured) error {
	if !slices.ContainsFunc(objs, func(o *unstructured.Unstructured) bool { return o.GetKind() == render.KindHelmChartProxy }) {
		return nil
	}
	_, err := m.C.RESTMapper().RESTMapping(hcpGVK.GroupKind(), hcpGVK.Version)
	if err != nil && !meta.IsNoMatchError(err) {
		return fmt.Errorf("checking the management cluster for Cluster API Add-on Provider Helm: %w", err)
	}
	if err != nil {
		// Since PET-100 cni: hydra installs Cilium through CAAPH too, so a
		// cluster with no spec.addons needs it as well; say which applies.
		var why, undo []string
		if slices.ContainsFunc(objs, func(o *unstructured.Unstructured) bool {
			return o.GetKind() == render.KindHelmChartProxy && o.GetLabels()[render.AddonLabelKey] == addons.Cilium
		}) {
			why, undo = append(why, "cni: hydra installs Cilium through it"), append(undo, "use cni: external")
		}
		if slices.ContainsFunc(objs, func(o *unstructured.Unstructured) bool {
			return o.GetKind() == render.KindHelmChartProxy && o.GetLabels()[render.AddonLabelKey] != addons.Cilium
		}) {
			why, undo = append(why, "hydra.yaml lists addons"), append(undo, "remove spec.addons")
		}
		return fmt.Errorf("%s, but the management cluster has no Cluster API Add-on Provider Helm (no %s %s): `hydra bootstrap` installs it on a new management cluster; on an existing one install it with `clusterctl init --addon helm:%s`, or %s (%w)",
			strings.Join(why, " and "), hcpGVK.GroupVersion(), hcpGVK.Kind, AddonProviderVersion, strings.Join(undo, " and "), err)
	}
	return nil
}

// AddonProviderVersion is the CAAPH release this CLI renders for; it must
// match bootstrap.CAAPHVersion (a test keeps them equal).
const AddonProviderVersion = "v0.6.6"

// ClusterState is what create and delete need to know about the Cluster
// before acting.
type ClusterState struct {
	Exists      bool
	Terminating bool
	// Endpoint is spec.controlPlaneEndpoint as host:port, if set.
	Endpoint string
	// object is the Cluster as read, for ownership checks.
	object *unstructured.Unstructured
	// CNIProxy: hydra's Cilium HelmChartProxy for the cluster exists, so
	// CAAPH keeps Cilium installed while the Cluster carries the CNI label,
	// and uninstalls it when the label goes.
	CNIProxy bool
	// LegacyCNI: a ClusterResourceSet named like the one that installed
	// hydra's Cilium before PET-100 is still there, so Cilium on the cluster
	// has no Helm release.
	LegacyCNI bool
	// ControlPlaneInitialized: the cluster's API has come up, so a CNI may
	// already run there.
	ControlPlaneInitialized bool
}

// CheckOwner reports whether this CLI may delete the Cluster as cluster:
// labelled for it, or unlabelled with adopt.
func (st ClusterState) CheckOwner(cluster string, adopt bool) error {
	if !st.Exists || st.object == nil {
		return nil
	}
	return checkOwner(st.object, cluster, adopt)
}

// CNIConflict explains why applying hydra.yaml's CNI choice to an existing
// Cluster would be wrong, or returns "". adoptCNI is --adopt-cni: the
// operator has checked what runs there and wants hydra's Cilium to take it
// over. kubectl is the command prefix that reaches the management cluster,
// so a printed fix never relies on the current context. The cases:
//
//   - cni: external on a cluster whose Cilium hydra's proxy installs: the
//     apply would drop the Cluster's CNI label, and CAAPH would uninstall
//     Cilium from the running cluster (PET-97). Never allowed.
//   - cni: hydra on a cluster still on a ClusterResourceSet named like the
//     one hydra used before PET-100: its Cilium has no Helm release. The
//     set must go first (a migration); --adopt-cni does not skip that.
//   - cni: hydra where hydra's proxy would newly select a cluster that
//     already runs: the Cluster lacks the CNI label (or carries another
//     value, or lost it), or carries it on a running cluster hydra's proxy
//     never installed. Its CNI may come from somewhere else, and hydra's
//     Cilium would land on top of it, taking over any Cilium objects without
//     a Helm release. Only with --adopt-cni.
func (st ClusterState) CNIConflict(hydraCNI, adoptCNI bool, kubectl string) string {
	if !st.Exists || st.object == nil {
		return ""
	}
	ns, name := st.object.GetNamespace(), st.object.GetName()
	if !hydraCNI {
		if !st.CNIProxy {
			return ""
		}
		return fmt.Sprintf("cluster %s/%s runs hydra's Cilium through its HelmChartProxy %s; cni: %s would remove the Cluster's %s label, and Cluster API Add-on Provider Helm would uninstall Cilium from the running cluster. hydra never does that: keep cni: %s. Handing Cilium to another tool without uninstalling it is a break-glass procedure (README, \"Leaving hydra's Cilium\")",
			ns, name, render.CNIName(name), config.CNIExternal, render.CNILabelKey, config.CNIHydra)
	}
	if st.LegacyCNI {
		return fmt.Sprintf("cluster %s/%s got its Cilium from the ClusterResourceSet %s that hydra v0.5 and earlier rendered, so that Cilium has no Helm release; this hydra installs Cilium through Cluster API Add-on Provider Helm instead, and does not switch a running cluster over on its own. Migrate it (README, \"Clusters on the old ClusterResourceSet\"), or set spec.network.cni: %s to leave its Cilium as it is",
			ns, name, render.CNIName(name), config.CNIExternal)
	}
	if adoptCNI || !st.ControlPlaneInitialized {
		return "" // the operator's say-so, or no CNI can run there yet
	}
	selected := HydraCNISelected(st.object)
	if selected && st.CNIProxy {
		return "" // hydra's own Cilium
	}
	if selected {
		return fmt.Sprintf("cluster %s/%s is running and carries the %s label, but hydra's Cilium HelmChartProxy has never installed its CNI (it was built or adopted with the label, or an older hydra's ClusterResourceSet was removed). cni: %s would install hydra's Cilium over whatever runs there, taking over any Cilium objects that have no Helm release. If that CNI is managed elsewhere, set spec.network.cni: %s. If no CNI runs there, or its Cilium already matches hydra's canonical values at spec.network.ciliumVersion, rerun with --adopt-cni",
			ns, name, render.CNILabelKey, config.CNIHydra, config.CNIExternal)
	}
	has := "exists without the " + render.CNILabelKey + " label"
	if v := st.object.GetLabels()[render.CNILabelKey]; v != "" {
		// Another value selects some other set (or none), never this
		// cluster's: hydra's apply would retarget it onto this one.
		has = fmt.Sprintf("carries the %s label with value %q, not %q", render.CNILabelKey, v, name)
	}
	// Only for a lost label is setting it back a whole fix: hydra's proxy
	// then selects the cluster again. Labelling a running cluster hydra's
	// proxy never installed just reaches the refusal above.
	fix := "rerun with --adopt-cni"
	if st.CNIProxy {
		has += " although hydra's Cilium HelmChartProxy exists, so CAAPH has uninstalled Cilium from it"
		fix = fmt.Sprintf("put the label back and CAAPH installs hydra's Cilium again (%s label --overwrite cluster -n %s %s %s=%s), or rerun with --adopt-cni", kubectl, ns, name, render.CNILabelKey, name)
	}
	return fmt.Sprintf("cluster %s/%s %s, so hydra's Cilium HelmChartProxy does not select it, and cni: %s would install hydra's Cilium on top of whatever CNI runs there. If that CNI is managed elsewhere, set spec.network.cni: %s. If no CNI runs there, or its Cilium already matches hydra's canonical values at spec.network.ciliumVersion, %s",
		ns, name, has, config.CNIHydra, config.CNIExternal, fix)
}

// HydraCNISelected reports whether hydra's CNI HelmChartProxy for this
// Cluster selects it: the proxy's selector is the CNI label with the
// cluster's own name as its value, so any other value selects nothing of
// hydra's here.
func HydraCNISelected(cl *unstructured.Unstructured) bool {
	return cl.GetLabels()[render.CNILabelKey] == cl.GetName()
}

// CNILabelLeft explains a CNI label still on the Cluster although hydra.yaml
// says cni: external, or returns "". Server-side apply removes a label only
// when hydra's field manager is its sole owner; one also owned by another
// manager (a cluster built by hand and adopted, or created by an older hydra)
// stays, and keeps hydra's ClusterResourceSet selecting the cluster: if that
// set has not applied yet, it still installs hydra's Cilium. kubectl is the
// command prefix that reaches the management cluster.
func (st ClusterState) CNILabelLeft(hydraCNI bool, kubectl string) string {
	if hydraCNI || !st.Exists || st.object == nil || st.object.GetLabels()[render.CNILabelKey] == "" {
		return ""
	}
	ns, name := st.object.GetNamespace(), st.object.GetName()
	return fmt.Sprintf("cluster %s/%s still carries the %s label although hydra.yaml says cni: %s, because another field manager also owns it; it keeps any selector of hydra's Cilium (an older hydra's ClusterResourceSet %s-cilium, if one is left) picking the cluster up. Remove it, and delete such a leftover set and its ConfigMap: %s label cluster -n %s %s %s-",
		ns, name, render.CNILabelKey, config.CNIExternal, name, kubectl, ns, name, render.CNILabelKey)
}

// Planned is the state create would leave the Cluster in: the server's
// dry-run result from Plan when it changes the Cluster, the live object
// otherwise.
func (st ClusterState) Planned(changes []Change) ClusterState {
	if st.object == nil {
		return st
	}
	for _, ch := range changes {
		if r := ch.Result; r != nil && r.GroupVersionKind() == clusterGVK && r.GetNamespace() == st.object.GetNamespace() && r.GetName() == st.object.GetName() {
			st.object = r
			return st
		}
	}
	return st
}

// State reads the Cluster object's existence, deletion and endpoint.
func (m *Manager) State(ctx context.Context, namespace, name string) (ClusterState, error) {
	cl := &unstructured.Unstructured{}
	cl.SetGroupVersionKind(clusterGVK)
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cl)
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return ClusterState{}, nil
	}
	if err != nil {
		return ClusterState{}, err
	}
	st := ClusterState{Exists: true, Terminating: cl.GetDeletionTimestamp() != nil, object: cl}
	if st.CNIProxy, err = m.cniProxyExists(ctx, namespace, name); err != nil {
		return ClusterState{}, fmt.Errorf("reading the CNI HelmChartProxy: %w", err)
	}
	// By name alone: whoever made it, a set by that name installed this
	// cluster's Cilium outside Helm.
	if st.LegacyCNI, err = m.exists(ctx, crsGVK, namespace, render.CNIName(name)); err != nil {
		return ClusterState{}, fmt.Errorf("reading the CNI ClusterResourceSet: %w", err)
	}
	st.ControlPlaneInitialized, _, _ = unstructured.NestedBool(cl.Object, "status", "initialization", "controlPlaneInitialized")
	if c := condition(cl, "ControlPlaneInitialized"); c != nil && c["status"] == ConditionTrue {
		st.ControlPlaneInitialized = true
	}
	host, _, _ := unstructured.NestedString(cl.Object, "spec", "controlPlaneEndpoint", "host")
	port, _, _ := unstructured.NestedInt64(cl.Object, "spec", "controlPlaneEndpoint", "port")
	if host != "" {
		st.Endpoint = net.JoinHostPort(host, strconv.FormatInt(port, 10))
	}
	return st, nil
}

// SelfHosted reports whether the management cluster is the cluster itself:
// deleting it there would delete the control plane that runs the deletion,
// leaving half-deleted machines and no controllers to finish.
//
// Nothing is dialled. It compares the management API server with the
// Cluster's endpoint, and the certificate authority of the management
// kubeconfig with the one in the cluster's own kubeconfig Secret. The CA
// matches whatever address the cluster is reached by (a tunnel, a port-forward,
// a DNS name), and a kind management cluster never shares its workload's CA.
// With no kubeconfig Secret there is nothing to compare, and a cluster without
// one cannot be serving the management API.
func (m *Manager) SelfHosted(ctx context.Context, namespace, name string, management []byte, st ClusterState) (bool, string, error) {
	mgmtServer, mgmtCA := kubeconfigIdentity(management)
	if st.Endpoint != "" && mgmtServer != "" && sameHostPort(mgmtServer, st.Endpoint) {
		return true, fmt.Sprintf("the management API server %s is the cluster's own endpoint", mgmtServer), nil
	}
	var sec corev1.Secret
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-kubeconfig"}, &sec)
	if apierrors.IsNotFound(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading Secret %s/%s-kubeconfig: %w", namespace, name, err)
	}
	// Without the management CA the comparison cannot be made, and "not
	// self-hosted" would only be a guess; refuse to guess before a delete.
	if len(mgmtCA) == 0 {
		return false, "", fmt.Errorf("cannot determine the management cluster's certificate authority from its kubeconfig (no certificate-authority-data, and no readable certificate-authority file), so cannot rule out that it is the cluster itself")
	}
	_, wlCA := kubeconfigIdentity(sec.Data["value"])
	if len(wlCA) == 0 {
		return false, "", fmt.Errorf("the kubeconfig Secret %s/%s-kubeconfig holds no readable certificate authority, so cannot rule out that the management cluster is the cluster itself", namespace, name)
	}
	if bytes.Equal(mgmtCA, wlCA) {
		return true, "the management kubeconfig trusts the cluster's own certificate authority (it manages itself)", nil
	}
	return false, "", nil
}

// KubeconfigCA is the certificate authority a kubeconfig's current context
// trusts, or nil when it has none readable.
func KubeconfigCA(kubeconfig []byte) []byte {
	_, ca := kubeconfigIdentity(kubeconfig)
	return ca
}

// SameCA reports whether two kubeconfigs trust the same certificate
// authority. Either one lacking a readable CA is an error, never "different".
func SameCA(a, b []byte) (bool, error) {
	_, ca := kubeconfigIdentity(a)
	_, cb := kubeconfigIdentity(b)
	if len(ca) == 0 || len(cb) == 0 {
		return false, fmt.Errorf("a kubeconfig has no readable certificate authority, so cannot tell which cluster it belongs to")
	}
	return bytes.Equal(ca, cb), nil
}

// kubeconfigIdentity returns the current context's server URL and CA bytes.
// A CA given as a file path (certificate-authority) is read from that file;
// an unreadable one yields no CA.
func kubeconfigIdentity(kubeconfig []byte) (string, []byte) {
	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return "", nil
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok {
		return "", nil
	}
	cl, ok := cfg.Clusters[ctx.Cluster]
	if !ok {
		return "", nil
	}
	if len(cl.CertificateAuthorityData) > 0 {
		return cl.Server, cl.CertificateAuthorityData
	}
	if cl.CertificateAuthority != "" {
		if b, err := os.ReadFile(cl.CertificateAuthority); err == nil {
			return cl.Server, b
		}
	}
	return cl.Server, nil
}

func sameHostPort(server, endpoint string) bool {
	u, err := url.Parse(server)
	if err != nil {
		return false
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(u.Hostname(), port) == endpoint
}

// CheckOwnership refuses to apply over objects this cluster did not render:
// an existing object whose hydra.io/cluster label names another cluster, or
// that has no such label (made by hand or by another tool). Server-side apply
// with forced ownership would otherwise take it over, and a later delete
// would remove it. adopt allows unlabelled objects (e.g. a cluster first
// built by hand); a label naming another cluster is always refused.
func (m *Manager) CheckOwnership(ctx context.Context, objs []*unstructured.Unstructured, cluster string, adopt bool) error {
	var problems []string
	for _, o := range objs {
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(o.GroupVersionKind())
		err := m.C.Get(ctx, client.ObjectKeyFromObject(o), live)
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", describe(o), err)
		}
		owner, labelled := live.GetLabels()[render.ClusterLabelKey]
		switch {
		case labelled && owner != cluster:
			problems = append(problems, fmt.Sprintf("%s belongs to cluster %q", describe(o), owner))
		case !labelled && !adopt:
			problems = append(problems, fmt.Sprintf("%s exists and was not made by hydra (pass --adopt to take it over)", describe(o)))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("refusing to apply over objects that are not this cluster's:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// Orphans lists objects labelled for this cluster that the current render no
// longer produces: a removed node pool, a superseded template. They are
// reported, never deleted implicitly.
func (m *Manager) Orphans(ctx context.Context, objs []*unstructured.Unstructured, namespace, cluster string) ([]string, error) {
	rendered := map[string]bool{}
	for _, o := range objs {
		rendered[o.GetKind()+"/"+o.GetName()] = true
	}
	var out []string
	for _, gvk := range ownedKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk.GroupVersion().WithKind(gvk.Kind + "List"))
		err := m.C.List(ctx, list, client.InNamespace(namespace), client.MatchingLabels{render.ClusterLabelKey: cluster})
		if meta.IsNoMatchError(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("listing %s: %w", gvk.Kind, err)
		}
		for i := range list.Items {
			o := &list.Items[i]
			if !rendered[o.GetKind()+"/"+o.GetName()] {
				out = append(out, describe(o))
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// exists reports whether the named object exists. A kind the management
// cluster does not serve counts as absent; any other read error is returned.
func (m *Manager) exists(ctx context.Context, gvk schema.GroupVersionKind, namespace, name string) (bool, error) {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(gvk)
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, o)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// cniProxyExists reports whether hydra's CNI HelmChartProxy for the cluster
// (<cluster>-cilium) exists. CAAPH selects by the CNI label, not by hydra's
// cluster label, so a proxy by that name owns the cluster's Cilium release
// whether hydra's label is on it or not; only one labelled for another
// cluster is someone else's.
func (m *Manager) cniProxyExists(ctx context.Context, namespace, cluster string) (bool, error) {
	o := &unstructured.Unstructured{}
	o.SetGroupVersionKind(hcpGVK)
	err := m.C.Get(ctx, client.ObjectKey{Namespace: namespace, Name: render.CNIName(cluster)}, o)
	switch {
	case apierrors.IsNotFound(err), meta.IsNoMatchError(err):
		return false, nil
	case err != nil:
		return false, err
	}
	l := o.GetLabels()[render.ClusterLabelKey]
	return l == "" || l == cluster, nil
}
