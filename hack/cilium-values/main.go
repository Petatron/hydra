// Command cilium-values writes the Cilium values hydra renders for each
// cni: hydra example, and the chart versions hydra allows, for
// hack/check-cilium.sh to render against the real chart.
//
//	go run ./hack/cilium-values <out-dir> <hydra.yaml>...
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Petatron/hydra/internal/addons"
	"github.com/Petatron/hydra/internal/config"
	"github.com/Petatron/hydra/internal/render"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "cilium-values:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) < 2 {
		return errors.New("usage: cilium-values <out-dir> <hydra.yaml> [<hydra.yaml>]")
	}
	out := args[0]
	cilium, ok := addons.Lookup(addons.Cilium)
	if !ok {
		return fmt.Errorf("the catalogue has no %s", addons.Cilium)
	}
	if err := os.WriteFile(filepath.Join(out, "versions"), []byte(strings.Join(cilium.Versions, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	n := 0
	for _, f := range args[1:] {
		c, err := config.Load(f)
		if err != nil {
			return err
		}
		if !render.HydraCNI(c) {
			continue
		}
		v, err := render.CiliumValues(c)
		if err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		name := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) + ".values.yaml"
		if err := os.WriteFile(filepath.Join(out, name), []byte(v), 0o600); err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return fmt.Errorf("no cni: hydra cluster among %s", strings.Join(args[1:], " "))
	}
	return nil
}
