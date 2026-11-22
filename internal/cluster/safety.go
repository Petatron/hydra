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

	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

var (
	hydraClusterGVK = schema.GroupVersionKind{Group: infraGroup, Version: infraVersion, Kind: "HydraCluster"}
	mdGVK           = schema.GroupVersionKind{Group: capiGroup, Version: capiVersion, Kind: "MachineDeployment"}
)

// ownedKinds are every kind render produces that this CLI may find again by
// the hydra.io/cluster label: to report as orphaned, or to delete.
var ownedKinds = []schema.GroupVersionKind{
	hydraClusterGVK,
	kcpGVK,
	mdGVK,
	{Group: capiGroup, Version: capiVersion, Kind: "MachineHealthCheck"},
	{Group: "bootstrap.cluster.x-k8s.io", Version: capiVersion, Kind: "KubeadmConfigTemplate"},
	{Group: infraGroup, Version: infraVersion, Kind: kindHydraMachineTemplate},
	{Group: "addons.cluster.x-k8s.io", Version: capiVersion, Kind: "ClusterResourceSet"},
	{Group: "", Version: "v1", Kind: kindConfigMap},
}

// CheckManagementCluster fails, before anything is written, when the target
// has no Cluster API: create must not leave a stray namespace on a cluster
// that is not a management cluster.
func (m *Manager) CheckManagementCluster() error {
	if _, err := m.C.RESTMapper().RESTMapping(clusterGVK.GroupKind(), clusterGVK.Version); err != nil {
		return fmt.Errorf("the target is not a Cluster API management cluster (no %s): %w", clusterGVK.GroupVersion(), err)
	}
	return nil
}

// ClusterState is what create and delete need to know about the Cluster
// before acting.
type ClusterState struct {
	Exists      bool
	Terminating bool
	// Endpoint is spec.controlPlaneEndpoint as host:port, if set.
	Endpoint string
	// object is the Cluster as read, for ownership checks.
	object *unstructured.Unstructured
}

// CheckOwner reports whether this CLI may delete the Cluster as cluster:
// labelled for it, or unlabelled with adopt.
func (st ClusterState) CheckOwner(cluster string, adopt bool) error {
	if !st.Exists || st.object == nil {
		return nil
	}
	return checkOwner(st.object, cluster, adopt)
}

// CNIConflict explains why applying hydra's CNI to an existing Cluster would
// be wrong, or returns "". A Cluster without the CNI label never had hydra's
// ClusterResourceSet select it (or had the label removed), so its CNI may have
// come from somewhere else (a cluster built by hand, or created with cni:
// external). Adding the label now would make the set apply hydra's Cilium on
// top of whatever runs there. kubectl is the command prefix that reaches the
// management cluster, so the printed fix never relies on the current context.
func (st ClusterState) CNIConflict(hydraCNI bool, kubectl string) string {
	if !hydraCNI || !st.Exists || st.object == nil || st.object.GetLabels()[render.CNILabelKey] != "" {
		return ""
	}
	ns, name := st.object.GetNamespace(), st.object.GetName()
	return fmt.Sprintf("cluster %s/%s exists without the %s label, so hydra's ClusterResourceSet does not select it, and cni: %s would apply hydra's Cilium on top of whatever CNI runs there. If that CNI is managed elsewhere, set spec.network.cni: %s. If it is hydra's own Cilium and only the label was removed, or no CNI runs there yet, put the label back (the set applies once, so it never installs a second copy over its own): %s label cluster -n %s %s %s=%s",
		ns, name, render.CNILabelKey, config.CNIHydra, config.CNIExternal, kubectl, ns, name, render.CNILabelKey, name)
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
	return fmt.Sprintf("cluster %s/%s still carries the %s label although hydra.yaml says cni: %s, because another field manager also owns it; it keeps hydra's ClusterResourceSet selecting the cluster, which installs hydra's Cilium if it has not applied yet. Remove it, then delete the orphaned %s-cilium ClusterResourceSet and ConfigMap: %s label cluster -n %s %s %s-",
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
