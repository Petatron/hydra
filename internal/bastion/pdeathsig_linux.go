package bastion

import (
	"os/exec"
	"syscall"
)

// setParentDeath has the kernel stop ssh if hydra dies without closing the
// tunnel (SIGKILL, or a signal hydra does not handle).
func setParentDeath(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
