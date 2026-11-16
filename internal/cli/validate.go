package cli

import (
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	kindcluster "sigs.k8s.io/kind/pkg/cluster"

	"github.com/Petatron/hydra/internal/bootstrap"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/validate"
)

// detectRuntime names the container runtime kind would use.
func detectRuntime() (string, error) {
	opt, err := kindcluster.DetectNodeProvider()
	if err != nil {
		return "", fmt.Errorf("kind finds no usable container runtime: %w", err)
	}
	// Detection only finds a binary; listing containers is what bootstrap
	// will need to work.
	if _, err := kindcluster.NewProvider(opt).List(); err != nil {
		return "", fmt.Errorf("the container runtime does not answer: %w", err)
	}
	for _, bin := range []string{"docker", "podman", "nerdctl"} {
		if _, err := exec.LookPath(bin); err == nil {
			return bin, nil
		}
	}
	return "a container runtime", nil
}

// validateDeps are the real ways validate looks at the world; tests replace
// them.
var validateDeps = func(cluster string) validate.Deps {
	return validate.Deps{
		Runtime: detectRuntime,
		Connect: validate.ConnectLibvirt,
		Host:    validate.HostProber(),
		Kind:    func() (validate.Prober, error) { return validate.KindProber(bootstrap.KindName(cluster)) },
		Now:     time.Now,
	}
}

func newValidateCommand() *cobra.Command {
	var file, credentials string
	c := &cobra.Command{
		Use:   "validate",
		Short: "Check, read-only, that this machine and the hypervisor are ready for hydra.yaml",
		Long: `validate checks what bootstrap and create will need, before they run:

  - a container runtime kind can use;
  - the hypervisor's libvirt TLS listener (16514) is reachable, and its
    plaintext listener (16509) is closed;
  - with --hypervisor-credentials: the client certificate (keyEncipherment,
    validity), the TLS login, the storage pool and its free space, the base
    image, and each bridge (up, and no port holding an address of its own);
  - the control-plane endpoint is free, and the network it lives on is
    reachable (for a managed network, through its gateway).

Reachability is judged from where Cluster API's controllers run: inside the
bootstrap kind cluster's node when it exists, else from this machine. An
address only this machine can reach, such as its loopback, fails.

Each check prints PASS, WARN or FAIL with what to do about it. The command
exits non-zero if any check fails. It writes nothing anywhere.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(file)
			if err != nil {
				return err
			}
			in := validate.Input{Config: cfg}
			if credentials != "" {
				creds, err := bootstrap.LoadCredentials(credentials)
				if err != nil {
					return err
				}
				in.Credentials = creds.Data()
			}
			ctx, stop := interruptible(cmd)
			defer stop()
			rs := validate.Run(ctx, in, validateDeps(cfg.Metadata.Name))
			if err := printResults(cmd.OutOrStdout(), rs); err != nil {
				return err
			}
			if validate.Failed(rs) {
				return errors.New("validation failed")
			}
			return nil
		},
	}
	c.Flags().StringVarP(&file, "file", "f", "hydra.yaml", "path to hydra.yaml")
	c.Flags().StringVar(&credentials, "hypervisor-credentials", "", "directory holding cacert.pem, clientcert.pem and clientkey.pem, to check the hypervisor itself")
	return c
}

func printResults(w io.Writer, rs []validate.Result) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	counts := map[validate.Status]int{}
	for _, r := range rs {
		counts[r.Status]++
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Status, r.Check, r.Detail); err != nil {
			return err
		}
		if r.Status != validate.Pass && r.Fix != "" {
			if _, err := fmt.Fprintf(tw, "\t\tfix: %s\n", r.Fix); err != nil {
				return err
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	parts := []string{}
	for _, s := range []validate.Status{validate.Pass, validate.Warn, validate.Fail} {
		parts = append(parts, fmt.Sprintf("%d %s", counts[s], strings.ToLower(s.String())))
	}
	_, err := fmt.Fprintln(w, strings.Join(parts, ", "))
	return err
}
