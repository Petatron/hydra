package pivot

// Test-only names for strings the production code also uses, so goconst
// does not count the test copies against the production literals.
const (
	lcCluster              = "Cluster"
	lcHydraMachine         = "HydraMachine"
	lcNowhere              = "nowhere"
	lcHydraMachineTemplate = "HydraMachineTemplate"
	lcClusterClass         = "ClusterClass"
	lcKubeadmControlPlane  = "KubeadmControlPlane"
	lcCRD                  = "CustomResourceDefinition"
	lcMachineHealthCheck   = "MachineHealthCheck"
	lcSecret               = "Secret"
	lcConfigMap            = "ConfigMap"
)
