package upgrade

// Test-only names for strings the production code also uses, so goconst
// does not count the test copies against the production literals.
const (
	lcMachine      = "Machine"
	lcDrainingNode = "DrainingNode"
)
