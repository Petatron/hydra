// Command hydra is the Hydra CLI: a stateless orchestrator over Cluster API
// for on-prem clusters (ADR-001).
package main

import (
	"fmt"
	"os"

	"github.com/Petatron/hydra/internal/cli"
)

func main() {
	if err := cli.NewRootCommand(os.Stdout, os.Stderr).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
