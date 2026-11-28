//go:build !linux

package transcode

import "os/exec"

func tieEncoderCLI(*exec.Cmd) {}
