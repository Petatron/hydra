//go:build !linux

package bastion

import "os/exec"

// setParentDeath is Linux only; elsewhere hydra closes the tunnel on exit and
// on SIGTERM or SIGHUP.
func setParentDeath(*exec.Cmd) {}
