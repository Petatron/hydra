package cli

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestDeleteSelfHostedRefusesAManagementKubeconfig(t *testing.T) {
	var out, errOut bytes.Buffer
	root := NewRootCommand(&out, &errOut)
	root.SetArgs([]string{"cluster", "delete", "--self-hosted", "--management-kubeconfig", "x", "-f", "../../examples/workload-managed-network.yaml"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("got %v", err)
	}
}

func TestTeardownNothingLocal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		answers  bool
		probeErr error
		wantErr  string
	}{
		{"endpoint refuses: nothing to do", false, nil, ""},
		{"endpoint answers: refuse", true, nil, "may still run"},
		{"endpoint unreachable: cannot tell, refuse", false, errors.New("i/o timeout"), "cannot reach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeKind(t, &fakeKind{})
			orig := endpointAnswers
			endpointAnswers = func(string) (bool, error) { return tc.answers, tc.probeErr }
			t.Cleanup(func() { endpointAnswers = orig })
			var errOut bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetErr(&errOut)
			o := teardownOptions{clusterKubeconfig: filepath.Join(t.TempDir(), "missing.kubeconfig")}
			err := runTeardown(context.Background(), cmd, selfHostedConfig("10.0.0.12"), o)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatal(err)
			case tc.wantErr == "" && !strings.Contains(errOut.String(), "found nothing to tear down"):
				t.Errorf("output %q", errOut.String())
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Fatalf("want %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// Hydra chose the endpoint (a managed network): with nothing local there is
// nothing to probe, so nothing proves the cluster is gone.
func TestTeardownNothingLocalWithoutAnEndpointRefuses(t *testing.T) {
	withFakeKind(t, &fakeKind{})
	cmd := &cobra.Command{}
	cmd.SetErr(&bytes.Buffer{})
	o := teardownOptions{clusterKubeconfig: filepath.Join(t.TempDir(), "missing.kubeconfig")}
	err := runTeardown(context.Background(), cmd, selfHostedConfig(""), o)
	if err == nil || !strings.Contains(err.Error(), "no endpoint to check") {
		t.Fatalf("got %v", err)
	}
}
