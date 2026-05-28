//go:build !windows

package ptyrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

func (*Driver) startPTY(cmd *exec.Cmd, rows, cols uint16) (*os.File, error) {
	tty, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: rows, Cols: cols})
	if err != nil {
		return nil, fmt.Errorf("start pty: %w", err)
	}
	return tty, nil
}

// killProcessGroup sends SIGKILL to the negative pid (process group). If the
// group is already gone it returns nil; if the group-kill fails for some other
// reason it falls back to killing just the leader and wraps the original error
// when the fallback also fails.
func (p *Process) killProcessGroup() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	process := p.cmd.Process
	groupErr := syscall.Kill(-process.Pid, syscall.SIGKILL)
	if groupErr == nil {
		return nil
	}
	if errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	if killErr := process.Kill(); killErr != nil {
		return fmt.Errorf("kill process group: %w (fallback also failed: %w)", groupErr, killErr)
	}
	return nil
}

func (p *Process) terminateProcessGroup() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	process := p.cmd.Process
	groupErr := syscall.Kill(-process.Pid, syscall.SIGTERM)
	if groupErr == nil {
		return nil
	}
	if errors.Is(groupErr, syscall.ESRCH) {
		return nil
	}
	if termErr := process.Signal(syscall.SIGTERM); termErr != nil {
		return fmt.Errorf("terminate process group: %w (fallback also failed: %w)", groupErr, termErr)
	}
	return nil
}
