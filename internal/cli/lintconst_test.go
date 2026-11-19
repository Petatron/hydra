package cli

// Test-only names for strings the production code also uses, so goconst
// does not count the test copies against the production literals.
const (
	lcBootstrap = "bootstrap"
	lcCreate    = "create"
	lcStatus    = "status"
	lcUpgrade   = "upgrade"
	lcDocker    = "docker"
	lcValidate  = "validate"
)
