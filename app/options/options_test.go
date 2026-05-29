package options

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseConsumedFlags(t *testing.T) {
	cfg, err := NewParser().Parse([]string{
		"--print",
		"--output-format=stream-json",
		"--input-format", "stream-json",
		"--replay-user-messages",
		"--idle-timeout", "3s",
		"--turn-timeout=5m",
		"--cwd", "/tmp/repo",
		"--typing-wpm", "125",
		"--typing-jitter=0.35",
		"--max-wpm-size=200",
		"--readiness-timeout", "9s",
		"--dbg",
		"hello",
	})

	require.NoError(t, err)
	assert.True(t, cfg.ReplayUserMessages)
	assert.True(t, cfg.Debug)
	assert.Equal(t, "stream-json", cfg.OutputFormat)
	assert.Equal(t, "stream-json", cfg.InputFormat)
	assert.Equal(t, 3*time.Second, cfg.IdleTimeout)
	assert.Equal(t, 5*time.Minute, cfg.TurnTimeout)
	assert.Equal(t, 9*time.Second, cfg.ReadinessTimeout)
	assert.Equal(t, "/tmp/repo", cfg.CWD)
	assert.Equal(t, 125, cfg.TypingWPM)
	assert.InEpsilon(t, 0.35, cfg.TypingJitter, 1e-9)
	assert.Equal(t, 200, cfg.MaxWPMSize)
	assert.Empty(t, cfg.ClaudeArgs)
	assert.Equal(t, []string{"hello"}, cfg.PromptArgs)
}

func TestParseDefaultsPrintText(t *testing.T) {
	cfg, err := NewParser().Parse([]string{"-p", "hello"})

	require.NoError(t, err)
	assert.Equal(t, "text", cfg.OutputFormat)
	assert.Equal(t, "text", cfg.InputFormat)
	assert.Equal(t, 100, cfg.TypingWPM)
	assert.InEpsilon(t, 0.20, cfg.TypingJitter, 1e-9)
	assert.Equal(t, 100, cfg.MaxWPMSize)
}

func TestParseForwardedFlags(t *testing.T) {
	args := []string{
		"--dangerously-skip-permissions",
		"--model", "opus",
		"--effort=high",
		"--permission-mode", "bypassPermissions",
		"--allowed-tools", "Bash(git *)", "Edit",
		"--allowedTools=Read",
		"--disallowed-tools", "WebFetch",
		"--add-dir", "../repo-a", "../repo-b",
		"--mcp-config", "mcp.json",
		"--settings", `{"foo":true}`,
		"--verbose",
		"--bare",
		"--plugin-dir", "plugins/a", "plugins/b.zip",
		"--",
		"prompt",
	}

	cfg, err := NewParser().Parse(args)

	require.NoError(t, err)
	wantClaude := []string{
		"--dangerously-skip-permissions",
		"--model", "opus",
		"--effort=high",
		"--permission-mode", "bypassPermissions",
		"--allowed-tools", "Bash(git *)", "Edit",
		"--allowedTools=Read",
		"--disallowed-tools", "WebFetch",
		"--add-dir", "../repo-a", "../repo-b",
		"--mcp-config", "mcp.json",
		"--settings", `{"foo":true}`,
		"--verbose",
		"--bare",
		"--plugin-dir", "plugins/a", "plugins/b.zip",
	}
	assert.Equal(t, wantClaude, cfg.ClaudeArgs)
	assert.Equal(t, []string{"prompt"}, cfg.PromptArgs)
}

func TestParseShortForwardedFlags(t *testing.T) {
	cfg, err := NewParser().Parse([]string{"-c", "-r", "session", "-d", "api", "-w", "branch", "-n", "name", "prompt"})

	require.NoError(t, err)
	assert.Equal(t, []string{"-c", "-r", "session", "-d", "api", "-w", "branch", "-n", "name"}, cfg.ClaudeArgs)
	assert.Equal(t, []string{"prompt"}, cfg.PromptArgs)
}

// -v is Claude's verbose short flag — fya must forward it rather than consume it
// as a version banner.
func TestParseShortV_ForwardsToClaude(t *testing.T) {
	cfg, err := NewParser().Parse([]string{"-v", "prompt"})

	require.NoError(t, err)
	assert.Equal(t, []string{"-v"}, cfg.ClaudeArgs)
	assert.Equal(t, []string{"prompt"}, cfg.PromptArgs)
	assert.False(t, cfg.Version, "-v must not trigger fya's version banner")
}

func TestParseDoubleDashPrompt(t *testing.T) {
	cfg, err := NewParser().Parse([]string{"--print", "--", "--not-a-flag", "prompt"})

	require.NoError(t, err)
	assert.Equal(t, []string{"--not-a-flag", "prompt"}, cfg.PromptArgs)
}

func TestParseRejectsUnknownFlag(t *testing.T) {
	_, err := NewParser().Parse([]string{"--bad-flag"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown flag: --bad-flag")
}

func TestParseRejectsInvalidFormats(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "output", args: []string{"--output-format", "xml"}, want: "Invalid value"},
		{name: "input", args: []string{"--input-format=json"}, want: "Invalid value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewParser().Parse(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestParseRejectsMissingForwardedValue(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "long", args: []string{"--model"}, want: "flag --model requires a value"},
		{name: "short", args: []string{"-n"}, want: "flag -n requires a value"},
		{name: "variadic", args: []string{"--add-dir", "--verbose"}, want: "flag --add-dir requires a value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewParser().Parse(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestParseValidatesWrapperControls(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "wpm", args: []string{"--typing-wpm=0"}, want: "typing-wpm must be positive"},
		{name: "jitter", args: []string{"--typing-jitter=-0.1"}, want: "typing-jitter must be non-negative"},
		{name: "max wpm size", args: []string{"--max-wpm-size=-1"}, want: "max-wpm-size must be non-negative"},
		{name: "turn timeout", args: []string{"--turn-timeout=0"}, want: "turn-timeout must be positive"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewParser().Parse(tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestParseHelp(t *testing.T) {
	_, err := NewParser().Parse([]string{"--help"})

	assert.ErrorIs(t, err, ErrHelp)
}

func TestWriteHelp(t *testing.T) {
	var b strings.Builder

	NewParser().WriteHelp(&b)

	assert.Contains(t, b.String(), "Usage:")
}
