// Command hydra is the Hydra CLI: a stateless orchestrator over Cluster API
// for on-prem clusters (ADR-001).
package main

import (
	"fmt"
	"os"

	"github.com/go-logr/logr"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Petatron/hydra/internal/cli"
)

func main() {
	// controller-runtime logs through a logger that must be set before first
	// use; unset, it prints a stack trace when it first has something to say
	// (an API warning header during clusterctl move, for one). hydra reports
	// through its own output, so its logs go nowhere.
	ctrllog.SetLogger(logr.Discard())
	if err := cli.NewRootCommand(os.Stdout, os.Stderr).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
