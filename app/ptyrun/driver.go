// Package ptyrun starts interactive commands inside a PTY and captures terminal output.
package ptyrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/go-pkgz/lgr"
)

const (
	defaultCommand     = "claude"
	defaultRows        = 40
	defaultCols        = 120
	defaultBufferLimit = 64 * 1024
	defaultCloseGrace  = 2 * time.Second
	defaultTermGrace   = 1 * time.Second
	ctrlC              = "\x03"
)

// ErrUnsupported is returned by Driver.Start on platforms without PTY support
// (Windows in v1).
var ErrUnsupported = errors.New("pty runner is unsupported on this platform")

// Config configures a Driver. Unset numeric fields fall back to sane defaults
// via withDefaults; Env, when nil, inherits the command environment minus
// wrapper/private variables that should not be visible to the child Claude
// process. When Dir is set, child PWD is normalized to the same directory.
type Config struct {
	Command     string
	Args        []string
	Dir         string
	Env         []string
	Rows        uint16
	Cols        uint16
	BufferLimit int
}

// Driver builds Processes from a Config. The same Driver may start multiple
// processes; each Start produces an independent Process.
type Driver struct {
	cfg Config
}

// Process is a running PTY-wrapped command. The Output buffer accumulates
// terminal output up to BufferLimit; Close and Kill are idempotent.
type Process struct {
	cmd       *exec.Cmd
	tty       *os.File
	output    *tailBuffer
	waitDone  chan error
	drainDone chan struct{}
	exited    chan struct{}
	killOnce  sync.Once
	closeOnce sync.Once
	killErr   error
	closeErr  error
}

// NewDriver returns a Driver configured with cfg after applying defaults.
func NewDriver(cfg Config) *Driver {
	return &Driver{cfg: cfg.withDefaults()}
}

// Start launches the configured command inside a fresh PTY and returns a
// Process whose drain/wait goroutines are already running. Canceling ctx
// kills the process group.
func (d *Driver) Start(ctx context.Context) (*Process, error) {
	if ctx == nil {
		return nil, errors.New("context is nil")
	}

	cmd := exec.CommandContext(ctx, d.cfg.Command, d.cfg.Args...)
	cmd.Dir = d.cfg.Dir
	cmd.Env = d.cfg.filteredEnv(cmd.Environ())

	tty, err := startPTY(cmd, d.cfg.Rows, d.cfg.Cols)
	if err != nil {
		return nil, fmt.Errorf("start pty command %q: %w", d.cfg.Command, err)
	}

	p := &Process{
		cmd:       cmd,
		tty:       tty,
		output:    &tailBuffer{data: make([]byte, 0, d.cfg.BufferLimit), limit: d.cfg.BufferLimit},
		waitDone:  make(chan error, 1),
		drainDone: make(chan struct{}),
		exited:    make(chan struct{}),
	}
	go p.drain()
	go p.wait()
	go p.watchCancel(ctx)

	return p, nil
}

func (c Config) withDefaults() Config {
	if c.Command == "" {
		c.Command = defaultCommand
	}
	if c.Rows == 0 {
		c.Rows = defaultRows
	}
	if c.Cols == 0 {
		c.Cols = defaultCols
	}
	if c.BufferLimit <= 0 {
		c.BufferLimit = defaultBufferLimit
	}
	return c
}

func (c Config) filteredEnv(inherited ...[]string) []string {
	env := c.Env
	if env == nil && len(inherited) > 0 {
		env = inherited[0]
	}
	if env == nil {
		env = os.Environ()
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if ok && c.isPrivateEnvKey(key) {
			continue
		}
		filtered = append(filtered, entry)
	}
	if c.Dir != "" {
		filtered = c.withPWD(filtered)
	}
	return filtered
}

func (c Config) withPWD(env []string) []string {
	pwd := c.Dir
	if abs, err := filepath.Abs(c.Dir); err == nil {
		pwd = abs
	}
	return c.withEnvValue(env, "PWD", pwd)
}

func (Config) withEnvValue(env []string, key, value string) []string {
	entry := key + "=" + value
	for i, current := range env {
		currentKey, _, ok := strings.Cut(current, "=")
		if ok && currentKey == key {
			env[i] = entry
			return env
		}
	}
	return append(env, entry)
}

func (Config) isPrivateEnvKey(key string) bool {
	upper := strings.ToUpper(key)
	return upper == "FYA" ||
		strings.HasPrefix(upper, "FYA_") ||
		upper == "CLAUDECODE" ||
		upper == "DEBUG" ||
		key == "_"
}

// Write sends raw bytes to the PTY master so the wrapped command sees them as
// input. Returns an error if the process is not started.
func (p *Process) Write(data []byte) (int, error) {
	if p == nil || p.tty == nil {
		return 0, errors.New("pty process is not started")
	}
	n, err := p.tty.Write(data)
	if err != nil {
		return n, fmt.Errorf("write pty: %w", err)
	}
	return n, nil
}

// Output returns a snapshot of the captured terminal output up to BufferLimit.
// Older bytes past the limit are dropped.
func (p *Process) Output() string {
	if p == nil || p.output == nil {
		return ""
	}
	return p.output.String()
}

// Done returns a channel that is closed once the process exits. A nil or
// unstarted Process returns an already-closed channel so consumers can use it
// safely without nil checks.
func (p *Process) Done() <-chan struct{} {
	if p == nil || p.exited == nil {
		done := make(chan struct{})
		close(done)
		return done
	}
	return p.exited
}

// Wait blocks until the process exits and the output drainer has finished.
func (p *Process) Wait() error {
	if p == nil {
		return errors.New("pty process is nil")
	}
	err := <-p.waitDone
	<-p.drainDone
	if err != nil {
		return fmt.Errorf("wait pty command: %w", err)
	}
	return nil
}

// Close asks the PTY process to exit like an interactive user would: first
// send Ctrl-C through the terminal, then SIGTERM, then SIGKILL as a last
// resort. Wait should still be called after Close to reap the child process.
func (p *Process) Close() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		p.closeErr = p.closeWithGrace(defaultCloseGrace, defaultTermGrace)
	})
	return p.closeErr
}

func (p *Process) closeWithGrace(closeGrace, termGrace time.Duration) error {
	if p.waitForExit(0) {
		return nil
	}

	var errs []error
	if _, err := p.Write([]byte(ctrlC)); err != nil {
		errs = append(errs, fmt.Errorf("send ctrl-c: %w", err))
	}
	if p.waitForExit(closeGrace) {
		return errors.Join(errs...)
	}

	if err := terminateProcessGroup(p.cmd.Process); err != nil {
		errs = append(errs, fmt.Errorf("terminate pty command: %w", err))
	}
	if p.waitForExit(termGrace) {
		return errors.Join(errs...)
	}

	if err := p.Kill(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (p *Process) waitForExit(d time.Duration) bool {
	if p == nil {
		return true
	}
	done := p.Done()
	if d <= 0 {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// Kill terminates the whole process group with SIGKILL. It is idempotent.
func (p *Process) Kill() error {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	p.killOnce.Do(func() {
		p.killErr = killProcessGroup(p.cmd.Process)
	})
	if p.killErr != nil {
		return fmt.Errorf("kill pty command: %w", p.killErr)
	}
	return nil
}

func (p *Process) drain() {
	defer close(p.drainDone)
	buf := make([]byte, 4096)
	for {
		n, err := p.tty.Read(buf)
		if n > 0 {
			p.output.Write(buf[:n])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("[DEBUG] pty drain stopped: %v", err)
			}
			return
		}
	}
}

func (p *Process) wait() {
	err := p.cmd.Wait()
	if closeErr := p.tty.Close(); closeErr != nil {
		log.Printf("[DEBUG] close pty tty: %v", closeErr)
	}
	p.waitDone <- err
	close(p.exited)
}

func (p *Process) watchCancel(ctx context.Context) {
	select {
	case <-ctx.Done():
		if err := p.Kill(); err != nil {
			log.Printf("[DEBUG] kill pty command after context cancel: %v", err)
		}
	case <-p.exited:
	}
}
