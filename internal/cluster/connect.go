package cluster

import (
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Connect builds a client for the management cluster from kubeconfig content.
// Cluster API kinds are handled as unstructured, so only core types are
// registered.
func Connect(kubeconfig []byte) (client.Client, error) {
	rc, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	return client.New(rc, client.Options{Scheme: scheme})
}
