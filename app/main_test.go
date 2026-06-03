package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/fya/app/options"
	"github.com/umputun/fya/app/schemaoutput"
	"github.com/umputun/fya/app/stream"
	"github.com/umputun/fya/app/turn"
)

func TestRevisionDefault(t *testing.T) {
	assert.Equal(t, "unknown", revision)
}

func TestExecuteVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args: []string{"--version"}, stdout: &stdout, stderr: &stderr, factory: neverFactory(t),
	}))

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "fya test-rev")
	assert.Contains(t, stdout.String(), "version: test-rev")
	assert.Empty(t, stderr.String())
}

func TestDefaultTurnRunner(t *testing.T) {
	var stdout, stderr bytes.Buffer

	runner := defaultTurnRunner(turnRunnerRequest{Stdout: &stdout, Stderr: &stderr, Options: options.Config{}, Stream: stream.Config{}})

	assert.NotNil(t, runner)
}

func TestExecuteMissingPrompt(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args: []string{"--print"}, stdout: &stdout, stderr: &stderr, factory: neverFactory(t),
	}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prompt is required")
}

func TestExecuteHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args: []string{"--help"}, stdout: &stdout, stderr: &stderr, factory: neverFactory(t),
	}))

	require.NoError(t, err)
	assert.Contains(t, stdout.String(), "Usage:")
	assert.Empty(t, stderr.String())
}

func TestRunEnablesStreamEventsForStreamJSON(t *testing.T) {
	var got turn.Config
	var stdout, stderr bytes.Buffer
	cfg := optionsConfig("hello")
	cfg.OutputFormat = "stream-json"

	err := run(t.Context(), cfg, testRequest(testReq{stdout: &stdout, stderr: &stderr, factory: captureConfigFactory(&got)}))

	require.NoError(t, err)
	assert.True(t, got.StreamEvents)
}

func TestRunSilentKeepsStreamEvents(t *testing.T) {
	var got turn.Config
	var stdout, stderr bytes.Buffer
	cfg := optionsConfig("hello")
	cfg.OutputFormat = "stream-json"
	cfg.Silent = true

	err := run(t.Context(), cfg, testRequest(testReq{stdout: &stdout, stderr: &stderr, factory: captureConfigFactory(&got)}))

	require.NoError(t, err)
	assert.True(t, got.StreamEvents)
}

func TestRunPropagatesTurnError(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := run(t.Context(), optionsConfig("hello"), testRequest(testReq{
		stdout: &stdout, stderr: &stderr, factory: factoryReturning(errors.New("turn failed")),
	}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "turn failed")
}

func TestRunAppendsJSONSchemaInstruction(t *testing.T) {
	var gotPrompt string
	var stdout, stderr bytes.Buffer
	schema := `{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`
	cfg := optionsConfig("hello")
	cfg.OutputFormat = "json"
	cfg.JSONSchema = schema

	err := run(t.Context(), cfg, testRequest(testReq{stdout: &stdout, stderr: &stderr, factory: capturePromptFactory(&gotPrompt)}))

	require.NoError(t, err)
	assert.Equal(t, "hello"+schemaoutput.Instruction(schema), gotPrompt)
}

func TestRunPassesJSONSchemaValidatorToStreamConfig(t *testing.T) {
	var got stream.Config
	var stdout, stderr bytes.Buffer
	schema := `{"type":"object","required":["summary"],"properties":{"summary":{"type":"string"}}}`
	cfg := optionsConfig("hello")
	cfg.OutputFormat = "json"
	cfg.JSONSchema = schema
	factory := func(req turnRunnerRequest) turnExecutor {
		got = req.Stream
		return turnRunnerFunc(func(context.Context, turn.Config) error { return nil })
	}

	err := run(t.Context(), cfg, testRequest(testReq{stdout: &stdout, stderr: &stderr, factory: factory}))

	require.NoError(t, err)
	require.NotNil(t, got.ValidateStructuredOutput)
	raw, err := got.ValidateStructuredOutput(`{"summary":"done"}`)
	require.NoError(t, err)
	assert.JSONEq(t, `{"summary":"done"}`, string(raw))
}

func TestRunKeepsPromptUnchangedWithoutJSONSchema(t *testing.T) {
	var gotPrompt string
	var stdout, stderr bytes.Buffer

	err := run(t.Context(), optionsConfig("hello"), testRequest(testReq{
		stdout: &stdout, stderr: &stderr, factory: capturePromptFactory(&gotPrompt),
	}))

	require.NoError(t, err)
	assert.Equal(t, "hello", gotPrompt)
}

func TestRunLeavesStructuredValidatorUnsetWithoutJSONSchema(t *testing.T) {
	var got stream.Config
	var stdout, stderr bytes.Buffer
	cfg := optionsConfig("hello")
	cfg.OutputFormat = "json"
	factory := func(req turnRunnerRequest) turnExecutor {
		got = req.Stream
		return turnRunnerFunc(func(context.Context, turn.Config) error { return nil })
	}

	err := run(t.Context(), cfg, testRequest(testReq{stdout: &stdout, stderr: &stderr, factory: factory}))

	require.NoError(t, err)
	assert.Equal(t, stream.FormatJSON, got.Format)
	assert.Nil(t, got.ValidateStructuredOutput)
}

func TestRunRejectsInvalidJSONSchemaBeforeTurn(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cfg := optionsConfig("hello")
	cfg.OutputFormat = "json"
	cfg.JSONSchema = `{"type":"not-a-json-schema-type"}`

	err := run(t.Context(), cfg, testRequest(testReq{stdout: &stdout, stderr: &stderr, factory: neverFactory(t)}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "prepare structured output")
}

func TestExecuteForwardsClaudeArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args:    []string{"--dangerously-skip-permissions", "--output-format", "stream-json", "--verbose", "--print", "hello"},
		stdout:  &stdout,
		stderr:  &stderr,
		factory: factoryReturning(errors.New("turn failed")),
	}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "turn failed")
}

func TestExecutePromptReturnsTurnError(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args: []string{"--print", "hello"}, stdout: &stdout, stderr: &stderr, factory: factoryReturning(errors.New("turn failed")),
	}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "turn failed")
	assert.Empty(t, stdout.String(), "no banner on run error")
}

func TestExecuteDoesNotWriteBannerForSuccessfulRun(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args: []string{"--print", "hello"}, stdout: &stdout, stderr: &stderr, factory: factoryReturning(nil),
	}))

	require.NoError(t, err)
	assert.Empty(t, stdout.String(), "clean stdout for runner output only")
}

func TestRunPositionalPromptDoesNotBlockOnOpenStdin(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	defer w.Close()

	var stdout, stderr bytes.Buffer
	var gotPrompt string
	done := make(chan error, 1)
	go func() {
		req := request{Stdin: r, Stdout: &stdout, Stderr: &stderr, Factory: capturePromptFactory(&gotPrompt)}
		done <- run(t.Context(), optionsConfig("hello"), req)
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
		assert.Equal(t, "hello", gotPrompt)
	case <-time.After(200 * time.Millisecond):
		_ = w.Close()
		_ = r.Close()
		t.Fatal("run blocked reading open stdin despite positional prompt")
	}
}

func TestExecuteInvalidFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer

	err := execute(t.Context(), testRequest(testReq{
		args: []string{"--bad-flag"}, stdout: &stdout, stderr: &stderr, factory: neverFactory(t),
	}))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag")
}

func TestSetupLog(_ *testing.T) {
	setupLog(false)
	setupLog(true)
}

func TestStdinHasDataNonFile(t *testing.T) {
	assert.True(t, stdinHasData(bytes.NewReader([]byte("prompt"))), "non-file reader assumed to have data")
}

// regular file via t.TempDir → not a char device → stdinHasData should be true.
func TestStdinHasDataRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.txt")
	require.NoError(t, os.WriteFile(path, []byte("hi"), 0o600))
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()

	assert.True(t, stdinHasData(f))
}

// pipe is also not a char device → stdinHasData should be true.
func TestStdinHasDataPipe(t *testing.T) {
	r, w, err := os.Pipe()
	require.NoError(t, err)
	defer r.Close()
	defer w.Close()

	assert.True(t, stdinHasData(r))
}

// closed file → Stat fails → fallback returns true (assume there's data).
func TestStdinHasDataStatError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.txt")
	require.NoError(t, os.WriteFile(path, []byte("hi"), 0o600))
	f, err := os.Open(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	assert.True(t, stdinHasData(f), "stat error falls back to assuming data is present")
}

func TestResolveVersionPrefersRevision(t *testing.T) {
	old := revision
	t.Cleanup(func() { revision = old })
	revision = "custom"

	assert.Equal(t, "custom", resolveVersion())
}

func TestResolveBuildVersion(t *testing.T) {
	tests := []struct {
		name string
		rev  string
		info *debug.BuildInfo
		ok   bool
		want string
	}{
		{name: "revision wins", rev: "custom", info: &debug.BuildInfo{}, ok: true, want: "custom"},
		{name: "module version", rev: "unknown", info: &debug.BuildInfo{Main: debug.Module{Version: "v1.2.3"}}, ok: true, want: "v1.2.3"},
		{name: "vcs revision", rev: "unknown", info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "1234567890"}}}, ok: true, want: "1234567"},
		{name: "short vcs revision ignored", rev: "unknown", info: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "123"}}}, ok: true, want: "unknown"},
		{name: "missing build info", rev: "unknown", info: nil, ok: false, want: "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, resolveBuildVersion(tt.rev, tt.info, tt.ok))
		})
	}
}

func optionsConfig(args ...string) options.Config {
	return options.Config{PromptArgs: args}
}

type testReq struct {
	args    []string
	stdout  io.Writer
	stderr  io.Writer
	factory turnRunnerFactory
}

// testRequest builds a request with a fixed Rev so tests don't have to repeat it.
func testRequest(req testReq) request {
	return request{
		Args:    req.args,
		Stdin:   bytes.NewReader(nil),
		Stdout:  req.stdout,
		Stderr:  req.stderr,
		Rev:     "test-rev",
		Factory: req.factory,
	}
}

func factoryReturning(err error) turnRunnerFactory {
	return func(turnRunnerRequest) turnExecutor {
		return turnRunnerFunc(func(context.Context, turn.Config) error { return err })
	}
}

func capturePromptFactory(prompt *string) turnRunnerFactory {
	return func(turnRunnerRequest) turnExecutor {
		return turnRunnerFunc(func(_ context.Context, cfg turn.Config) error {
			*prompt = cfg.Prompt
			return nil
		})
	}
}

func captureConfigFactory(got *turn.Config) turnRunnerFactory {
	return func(turnRunnerRequest) turnExecutor {
		return turnRunnerFunc(func(_ context.Context, cfg turn.Config) error {
			*got = cfg
			return nil
		})
	}
}

// neverFactory builds a factory that fails the test if it is ever invoked. It is
// used for code paths (--version, --help, parse errors, missing prompt) that
// must short-circuit before reaching the turn runner.
func neverFactory(t *testing.T) turnRunnerFactory {
	t.Helper()
	return func(turnRunnerRequest) turnExecutor {
		t.Fatal("turn runner factory invoked unexpectedly")
		return nil
	}
}

type turnRunnerFunc func(context.Context, turn.Config) error

func (fn turnRunnerFunc) Run(ctx context.Context, cfg turn.Config) error {
	return fn(ctx, cfg)
}
