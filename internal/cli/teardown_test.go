package cli

import (
	"bytes"
	"context"
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
		name    string
		answers bool
		wantErr string
	}{
		{"endpoint silent: nothing to do", false, ""},
		{"endpoint answers: refuse", true, "may still run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withFakeKind(t, &fakeKind{})
			orig := endpointAnswers
			endpointAnswers = func(string) (bool, error) { return tc.answers, nil }
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
