//go:build windows

package mcp

import "os/exec"

func setProcAttrs(cmd *exec.Cmd) {}

func terminate(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func kill(cmd *exec.Cmd) { terminate(cmd) }
