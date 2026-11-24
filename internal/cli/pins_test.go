package cli

import (
	"testing"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/cluster"
)

// The CAAPH release bootstrap installs is the one create's refusal message
// tells the user to install by hand.
func TestCAAPHPinsAgree(t *testing.T) {
	if bootstrap.CAAPHVersion != cluster.AddonProviderVersion {
		t.Errorf("bootstrap pins CAAPH %s, cluster says %s", bootstrap.CAAPHVersion, cluster.AddonProviderVersion)
	}
}
