package ptyrun

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartFailure(t *testing.T) {
	_, err := NewDriver(Config{Command: "definitely-not-a-real-fya-test-command"}).Start(t.Context())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "start pty command")
}

func TestStartNilContext(t *testing.T) {
	var ctx context.Context
	_, err := NewDriver(Config{Command: helperCommand(t), Args: helperArgs()}).Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "context is nil")
}

func TestOutputDraining(t *testing.T) {
	p := startHelper(t, "output")

	require.NoError(t, p.Wait())
	got := p.Output()
	assert.Contains(t, got, "hello")
	assert.Contains(t, got, "world")
}

func TestWrite(t *testing.T) {
	p := startHelper(t, "read")

	_, err := p.Write([]byte("hello\n"))
	require.NoError(t, err)
	require.NoError(t, p.Wait())
	assert.Contains(t, p.Output(), "got:hello")
}

func TestEnvFiltering(t *testing.T) {
	p := startHelperWithConfig(t, Config{
		Command: helperCommand(t),
		Args:    helperArgs(),
		Env: append(helperEnv("env"),
			"CLAUDECODE=1",
			"ANTHROPIC_API_KEY=dummy",
			"FYA_CLAUDE_DIR=/tmp/fya",
			"FYA=1",
			"DEBUG=1",
			"_=/tmp/fya",
		),
	})

	require.NoError(t, p.Wait())
	got := p.Output()
	assert.NotContains(t, got, "cc=1", "CLAUDECODE must be filtered out of child env")
	assert.NotContains(t, got, "fya=/tmp/fya", "FYA_* vars must be filtered out of child env")
	assert.NotContains(t, got, "debug=1", "DEBUG is consumed by fya and must not be forwarded")
	assert.NotContains(t, got, "underscore=/tmp/fya", "shell '_' can reveal the wrapper path")
	assert.Contains(t, got, "key=dummy", "ANTHROPIC_API_KEY must be preserved")
}

func TestStartSetsChildDirAndPWD(t *testing.T) {
	dir := t.TempDir()
	p := startHelperWithConfig(t, Config{
		Command: helperCommand(t),
		Args:    helperArgs(),
		Dir:     dir,
		Env:     append(helperEnv("cwd"), "PWD=/stale"),
	})

	require.NoError(t, p.Wait())
	got := p.Output()
	assert.Contains(t, got, "cwd="+dir)
	assert.Contains(t, got, "pwd="+dir)
}

func TestStartSetsInheritedChildDirAndPWD(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PTYRUN_TEST_HELPER", "1")
	t.Setenv("PTYRUN_TEST_MODE", "cwd")
	t.Setenv("PWD", "/stale")
	p := startHelperWithConfig(t, Config{Command: helperCommand(t), Args: helperArgs(), Dir: dir})

	require.NoError(t, p.Wait())
	got := p.Output()
	assert.Contains(t, got, "cwd="+dir)
	assert.Contains(t, got, "pwd="+dir)
}

func TestContextCancellationKillsProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	p := startHelperWithContext(t, ctx, Config{Command: helperCommand(t), Args: helperArgs(), Env: helperEnv("sleep")})

	cancel()
	done := make(chan error, 1)
	go func() { done <- p.Wait() }()

	select {
	case err := <-done:
		require.Error(t, err, "process must exit with an error after context cancel")
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit after context cancellation")
	}
}

func TestCloseSendsCtrlC(t *testing.T) {
	p := startHelper(t, "wait-int")
	waitForOutput(t, p, "waiting interrupt")

	require.NoError(t, p.Close())
	require.NoError(t, p.Wait())
	assert.Contains(t, p.Output(), "got interrupt")
}

func TestCloseFallsBackToKill(t *testing.T) {
	p := startHelper(t, "ignore-signals")
	waitForOutput(t, p, "ignoring signals")

	require.NoError(t, p.closeWithGrace(10*time.Millisecond, 10*time.Millisecond))
	err := p.Wait()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signal: killed")
}

func TestFilterEnv(t *testing.T) {
	got := Config{Env: []string{
		"A=1",
		"CLAUDECODE=1",
		"ANTHROPIC_API_KEY=dummy",
		"FYA=1",
		"FYA_CLAUDE_DIR=/tmp/fya",
		"fya_custom=1",
		"DEBUG=1",
		"_=/tmp/fya",
	}}.filteredEnv()

	assert.Equal(t, []string{"A=1", "ANTHROPIC_API_KEY=dummy"}, got)
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.withDefaults()

	assert.Equal(t, defaultCommand, cfg.Command)
	assert.Equal(t, uint16(defaultRows), cfg.Rows)
	assert.Equal(t, uint16(defaultCols), cfg.Cols)
	assert.Equal(t, defaultBufferLimit, cfg.BufferLimit)
}

func TestConfigDefaultsCopiesSlices(t *testing.T) {
	args := []string{"--verbose"}
	env := []string{"A=1"}
	cfg := Config{Args: args, Env: env}.withDefaults()
	args[0] = "--mutated"
	env[0] = "A=mutated"

	assert.Equal(t, []string{"--verbose"}, cfg.Args)
	assert.Equal(t, []string{"A=1"}, cfg.Env)
}

func TestConfigDefaultsPreservesExplicitEmptyEnv(t *testing.T) {
	cfg := Config{Env: []string{}}.withDefaults()

	assert.NotNil(t, cfg.Env)
	assert.Empty(t, cfg.filteredEnv([]string{"SHOULD_NOT_INHERIT=1"}))
}

func TestProcessNilSafeMethods(t *testing.T) {
	var p *Process

	assert.Empty(t, p.Output())
	require.NoError(t, p.Close())
	require.NoError(t, p.Kill())

	_, err := p.Write([]byte("x"))
	require.Error(t, err)
	require.Error(t, p.Wait())
}

func TestProcessDoneNilSafe(t *testing.T) {
	var p *Process

	select {
	case <-p.Done():
	case <-time.After(time.Second):
		t.Fatal("Done did not return closed channel for nil process")
	}
}

func TestProcessDoneReturnsExitChannel(t *testing.T) {
	exited := make(chan struct{})
	p := &Process{exited: exited}

	var want <-chan struct{} = exited
	assert.Equal(t, want, p.Done())
	close(exited)
}

func TestProcessKillWithoutProcess(t *testing.T) {
	p := &Process{}

	assert.NoError(t, p.Kill())
}

func TestProcessMethodsWithoutLivePTY(t *testing.T) {
	p := &Process{
		output:    &tailBuffer{data: make([]byte, 0, 32), limit: 32},
		waitDone:  make(chan error, 1),
		drainDone: make(chan struct{}),
		exited:    make(chan struct{}),
	}
	p.output.Write([]byte("captured"))
	p.waitDone <- nil
	close(p.drainDone)
	close(p.exited)

	assert.Equal(t, "captured", p.Output())
	assert.NoError(t, p.Wait())
}

func TestProcessWaitError(t *testing.T) {
	p := &Process{
		waitDone:  make(chan error, 1),
		drainDone: make(chan struct{}),
	}
	p.waitDone <- errors.New("boom")
	close(p.drainDone)

	err := p.Wait()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wait pty command")
}

func TestWatchCancelExitsWhenProcessAlreadyExited(t *testing.T) {
	p := &Process{exited: make(chan struct{})}
	close(p.exited)

	done := make(chan struct{})
	go func() {
		p.watchCancel(t.Context())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watchCancel did not exit")
	}
}

func TestPTYHelperProcess(_ *testing.T) {
	if os.Getenv("PTYRUN_TEST_HELPER") != "1" {
		return
	}
	switch os.Getenv("PTYRUN_TEST_MODE") {
	case "output":
		fmt.Println("hello")
		fmt.Println("world")
	case "read":
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		fmt.Printf("got:%s", line)
	case "env":
		fmt.Printf("cc=%s key=%s fya=%s debug=%s underscore=%s\n",
			os.Getenv("CLAUDECODE"),
			os.Getenv("ANTHROPIC_API_KEY"),
			os.Getenv("FYA_CLAUDE_DIR"),
			os.Getenv("DEBUG"),
			os.Getenv("_"),
		)
	case "cwd":
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Printf("cwd-error:%v\n", err)
			break
		}
		fmt.Printf("cwd=%s pwd=%s\n", cwd, os.Getenv("PWD"))
	case "wait-int":
		interrupt := make(chan os.Signal, 1)
		signal.Notify(interrupt, os.Interrupt)
		defer signal.Stop(interrupt)
		fmt.Println("waiting interrupt")
		<-interrupt
		fmt.Println("got interrupt")
	case "ignore-signals":
		signal.Ignore(os.Interrupt, syscall.SIGTERM)
		fmt.Println("ignoring signals")
		time.Sleep(10 * time.Second)
	case "sleep":
		time.Sleep(10 * time.Second)
	default:
		fmt.Println("unknown helper mode")
	}
	os.Exit(0)
}

func startHelper(t *testing.T, mode string) *Process {
	t.Helper()
	return startHelperWithConfig(t, Config{Command: helperCommand(t), Args: helperArgs(), Env: helperEnv(mode)})
}

func startHelperWithConfig(t *testing.T, cfg Config) *Process {
	t.Helper()
	return startHelperWithContext(t, t.Context(), cfg)
}

func startHelperWithContext(t *testing.T, ctx context.Context, cfg Config) *Process {
	t.Helper()
	// preferred path for CI / sandboxed runners that block fork: set FYA_SKIP_PTY=1
	// and the PTY-subprocess tests skip up front. The runtime check below catches
	// the same case after the fact when the env var isn't set.
	if os.Getenv("FYA_SKIP_PTY") != "" {
		t.Skip("PTY tests disabled via FYA_SKIP_PTY")
	}
	p, err := NewDriver(cfg).Start(ctx)
	if err != nil {
		if errors.Is(err, ErrUnsupported) {
			t.Skip("PTY unsupported on this platform")
		}
		// fallback for sandboxed environments (e.g. Claude Code bash tool) that
		// block fork; fork errors as "operation not permitted". Set FYA_SKIP_PTY
		// to skip these tests explicitly.
		if errors.Is(err, syscall.EPERM) {
			t.Skipf("PTY subprocess execution blocked by sandbox (set FYA_SKIP_PTY to skip explicitly): %v", err)
		}
		t.Fatalf("Start returned error: %v", err)
	}
	t.Cleanup(func() { _ = p.Kill() })
	return p
}

func waitForOutput(t *testing.T, p *Process, text string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for output %q; got %q", text, p.Output())
		case <-ticker.C:
			if strings.Contains(p.Output(), text) {
				return
			}
		}
	}
}

func helperCommand(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	require.NoError(t, err, "test executable not found")
	return path
}

func helperArgs() []string {
	return []string{"-test.run=TestPTYHelperProcess", "--"}
}

func helperEnv(mode string) []string {
	return []string{
		"PTYRUN_TEST_HELPER=1",
		"PTYRUN_TEST_MODE=" + mode,
		"PATH=" + os.Getenv("PATH"),
	}
}
