package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Petatron/hydra/internal/validate"
)

type silentProber struct{}

func (silentProber) Where() string    { return "this machine" }
func (silentProber) OnThisHost() bool { return true }
func (silentProber) TCP(context.Context, string, int) (validate.Outcome, error) {
	return validate.NoAnswer, nil
}

func TestValidateExitsNonZeroOnAFailure(t *testing.T) {
	orig := validateDeps
	t.Cleanup(func() { validateDeps = orig })
	for _, tc := range []struct {
		name    string
		runtime error
		wantErr bool
	}{
		{"a failing check", errors.New("docker is not running"), true},
		{"warnings only", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validateDeps = func(string) validate.Deps {
				return validate.Deps{
					Runtime: func() (string, error) { return "docker", tc.runtime },
					Host:    silentProber{},
					Now:     time.Now,
				}
			}
			dir := t.TempDir()
			f := filepath.Join(dir, "hydra.yaml")
			src, err := os.ReadFile("../../examples/hydra-mgmt.yaml")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f, src, 0o600); err != nil {
				t.Fatal(err)
			}
			var out, errOut bytes.Buffer
			root := NewRootCommand(&out, &errOut)
			root.SetArgs([]string{"validate", "-f", f})
			err = root.Execute()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err %v\n%s", err, out.String())
			}
			if !strings.Contains(out.String(), "container runtime") || !strings.Contains(out.String(), "fix:") {
				t.Errorf("output lacks a check or its fix line:\n%s", out.String())
			}
		})
	}
}
