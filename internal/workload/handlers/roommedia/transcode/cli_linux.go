//go:build linux

package transcode

import (
	"os/exec"
	"syscall"
)

func tieEncoderCLI(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
