//go:build windows

package ptyrun

import (
	"os"
	"os/exec"
)

func (*Driver) startPTY(_ *exec.Cmd, _, _ uint16) (*os.File, error) {
	return nil, ErrUnsupported
}

func (p *Process) killProcessGroup() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	return p.cmd.Process.Kill()
}

func (p *Process) terminateProcessGroup() error {
	return p.killProcessGroup()
}
