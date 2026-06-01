// Package options parses fya CLI arguments and separates wrapper flags from Claude launch flags.
package options

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"
)

// ErrHelp is returned by Parser.Parse when --help is requested so the caller can
// print usage instead of treating help as a parse error.
var ErrHelp = errors.New("help requested")

var (
	consumedBool = map[string]struct{}{
		"print":                {},
		"replay-user-messages": {},
		"silent":               {},
		"dbg":                  {},
		"version":              {},
	}
	consumedValue = map[string]struct{}{
		"output-format":     {},
		"input-format":      {},
		"idle-timeout":      {},
		"turn-timeout":      {},
		"cwd":               {},
		"typing-wpm":        {},
		"typing-jitter":     {},
		"max-wpm-size":      {},
		"readiness-timeout": {},
	}
	forwardBool = map[string]struct{}{
		"allow-dangerously-skip-permissions":     {},
		"bare":                                   {},
		"brief":                                  {},
		"chrome":                                 {},
		"continue":                               {},
		"dangerously-skip-permissions":           {},
		"disable-slash-commands":                 {},
		"exclude-dynamic-system-prompt-sections": {},
		"fork-session":                           {},
		"ide":                                    {},
		"mcp-debug":                              {},
		"no-chrome":                              {},
		"no-session-persistence":                 {},
		"strict-mcp-config":                      {},
		"verbose":                                {},
	}
	forwardValue = map[string]struct{}{
		"agent":                              {},
		"agents":                             {},
		"append-system-prompt":               {},
		"debug-file":                         {},
		"effort":                             {},
		"fallback-model":                     {},
		"json-schema":                        {},
		"max-budget-usd":                     {},
		"model":                              {},
		"name":                               {},
		"permission-mode":                    {},
		"remote-control-session-name-prefix": {},
		"session-id":                         {},
		"setting-sources":                    {},
		"settings":                           {},
		"system-prompt":                      {},
	}
	forwardOptionalValue = map[string]struct{}{
		"debug":          {},
		"from-pr":        {},
		"remote-control": {},
		"resume":         {},
		"tmux":           {},
		"worktree":       {},
	}
	forwardVariadic = map[string]struct{}{
		"add-dir":          {},
		"allowed-tools":    {},
		"allowedTools":     {},
		"betas":            {},
		"disallowed-tools": {},
		"disallowedTools":  {},
		"file":             {},
		"mcp-config":       {},
		"plugin-dir":       {},
		"plugin-url":       {},
		"tools":            {},
	}
)

// Config is the parsed fya CLI configuration after splitting wrapper flags,
// forwarded Claude flags, and positional prompt args.
type Config struct {
	OutputFormat       string
	InputFormat        string
	ReplayUserMessages bool
	Silent             bool
	IdleTimeout        time.Duration
	TurnTimeout        time.Duration
	CWD                string
	TypingWPM          int
	TypingJitter       float64
	MaxWPMSize         int
	ReadinessTimeout   time.Duration
	Debug              bool
	Version            bool
	ClaudeArgs         []string
	PromptArgs         []string
}

type rawOptions struct {
	Print              bool          `short:"p" long:"print" description:"run one print-compatible turn (always on; accepted for drop-in compatibility)"`
	OutputFormat       string        `long:"output-format" choice:"text" choice:"json" choice:"stream-json" default:"text" description:"output format"`
	InputFormat        string        `long:"input-format" choice:"text" choice:"stream-json" default:"text" description:"input format"`
	ReplayUserMessages bool          `long:"replay-user-messages" description:"re-emit stream-json user messages on stdout"`
	Silent             bool          `long:"silent" description:"accepted for compatibility; synthetic tool progress is disabled by default"`
	IdleTimeout        time.Duration `long:"idle-timeout" default:"2s" description:"transcript idle duration before considering a turn complete"`
	TurnTimeout        time.Duration `long:"turn-timeout" default:"30m" description:"maximum wall-clock duration for one turn"`
	CWD                string        `long:"cwd" default:"." description:"working directory for the Claude session"`
	TypingWPM          int           `long:"typing-wpm" default:"100" description:"prompt typing speed in words per minute"`
	TypingJitter       float64       `long:"typing-jitter" default:"0.20" description:"per-character typing delay jitter ratio (0 disables jitter)"`
	MaxWPMSize         int           `long:"max-wpm-size" default:"100" description:"paste prompt at once instead of typing when it is longer than N words (0 always types)"`
	ReadinessTimeout   time.Duration `long:"readiness-timeout" default:"30s" description:"maximum wait for Claude input readiness"`
	Debug              bool          `long:"dbg" env:"DEBUG" description:"enable fya debug logging (named --dbg to avoid collision with claude --debug)"`
	Version            bool          `short:"V" long:"version" description:"show version info"`
}

// Parser parses fya CLI arguments.
type Parser struct{}

// NewParser returns a Parser ready to use.
func NewParser() *Parser {
	return &Parser{}
}

// Parse splits args into a Config plus a list of args to forward to the
// interactive Claude command. It returns ErrHelp when --help is requested.
func (p *Parser) Parse(args []string) (Config, error) {
	var raw rawOptions
	flagsParser := p.flagsParser(&raw)
	_, err := flagsParser.ParseArgs(args)
	if err != nil {
		var flagsErr *flags.Error
		if errors.As(err, &flagsErr) && flagsErr.Type == flags.ErrHelp {
			return Config{}, ErrHelp
		}
		return Config{}, fmt.Errorf("parse args: %w", err)
	}

	claudeArgs, promptArgs, err := newSplitter(args).split()
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		OutputFormat:       raw.OutputFormat,
		InputFormat:        raw.InputFormat,
		ReplayUserMessages: raw.ReplayUserMessages,
		Silent:             raw.Silent,
		IdleTimeout:        raw.IdleTimeout,
		TurnTimeout:        raw.TurnTimeout,
		CWD:                raw.CWD,
		TypingWPM:          raw.TypingWPM,
		TypingJitter:       raw.TypingJitter,
		MaxWPMSize:         raw.MaxWPMSize,
		ReadinessTimeout:   raw.ReadinessTimeout,
		Debug:              raw.Debug,
		Version:            raw.Version,
		ClaudeArgs:         claudeArgs,
		PromptArgs:         promptArgs,
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// WriteHelp emits the go-flags-rendered help text to w.
func (p *Parser) WriteHelp(w io.Writer) {
	var raw rawOptions
	flagsParser := p.flagsParser(&raw)
	flagsParser.WriteHelp(w)
}

func (*Parser) flagsParser(raw *rawOptions) *flags.Parser {
	p := flags.NewParser(raw, flags.HelpFlag|flags.PassDoubleDash|flags.IgnoreUnknown)
	p.Name = "fya"
	p.Usage = "[OPTIONS] [PROMPT]"
	return p
}

func (c Config) validate() error {
	if c.TypingWPM <= 0 {
		return errors.New("typing-wpm must be positive")
	}
	if c.TypingJitter < 0 {
		return errors.New("typing-jitter must be non-negative")
	}
	if c.MaxWPMSize < 0 {
		return errors.New("max-wpm-size must be non-negative")
	}
	if c.IdleTimeout < 0 {
		return errors.New("idle-timeout must be non-negative")
	}
	if c.TurnTimeout <= 0 {
		return errors.New("turn-timeout must be positive")
	}
	if c.ReadinessTimeout < 0 {
		return errors.New("readiness-timeout must be non-negative")
	}
	return nil
}

type splitter struct {
	args   []string
	claude []string
	prompt []string
}

func newSplitter(args []string) *splitter {
	return &splitter{args: args}
}

func (s *splitter) split() ([]string, []string, error) {
	for i := 0; i < len(s.args); i++ {
		arg := s.args[i]
		if arg == "--" {
			s.prompt = append(s.prompt, s.args[i+1:]...)
			break
		}
		if arg == "-" || !strings.HasPrefix(arg, "-") {
			s.prompt = append(s.prompt, arg)
			continue
		}
		if strings.HasPrefix(arg, "--") {
			next, err := s.splitLong(i)
			if err != nil {
				return nil, nil, err
			}
			i = next
			continue
		}
		next, err := s.splitShort(i)
		if err != nil {
			return nil, nil, err
		}
		i = next
	}
	return s.claude, s.prompt, nil
}

func (s *splitter) splitLong(i int) (int, error) {
	name, hasValue := s.longName(s.args[i])
	switch {
	case s.has(consumedBool, name):
		return s.skipLongBool(name, hasValue, i)
	case s.has(consumedValue, name):
		return s.skipLongValue(name, hasValue, i)
	case s.has(forwardBool, name):
		return s.appendLongBool(name, hasValue, i)
	case s.has(forwardValue, name):
		return s.appendLongValue(name, hasValue, i)
	case s.has(forwardOptionalValue, name):
		return s.appendLongOptionalValue(hasValue, i)
	case s.has(forwardVariadic, name):
		return s.appendLongVariadic(name, hasValue, i)
	default:
		return 0, fmt.Errorf("unknown flag: --%s", name)
	}
}

func (s *splitter) skipLongBool(name string, hasValue bool, i int) (int, error) {
	if hasValue {
		return 0, fmt.Errorf("flag --%s does not take a value", name)
	}
	return i, nil
}

func (s *splitter) skipLongValue(name string, hasValue bool, i int) (int, error) {
	if hasValue {
		return i, nil
	}
	if i+1 >= len(s.args) {
		return 0, fmt.Errorf("flag --%s requires a value", name)
	}
	return i + 1, nil
}

func (s *splitter) appendLongBool(name string, hasValue bool, i int) (int, error) {
	if hasValue {
		return 0, fmt.Errorf("flag --%s does not take a value", name)
	}
	s.claude = append(s.claude, s.args[i])
	return i, nil
}

func (s *splitter) appendLongValue(name string, hasValue bool, i int) (int, error) {
	if hasValue {
		s.claude = append(s.claude, s.args[i])
		return i, nil
	}
	if i+1 >= len(s.args) || s.isFlag(s.args[i+1]) {
		return 0, fmt.Errorf("flag --%s requires a value", name)
	}
	s.claude = append(s.claude, s.args[i], s.args[i+1])
	return i + 1, nil
}

func (s *splitter) appendLongOptionalValue(hasValue bool, i int) (int, error) {
	s.claude = append(s.claude, s.args[i])
	if hasValue || i+1 >= len(s.args) || s.isFlag(s.args[i+1]) {
		return i, nil
	}
	s.claude = append(s.claude, s.args[i+1])
	return i + 1, nil
}

func (s *splitter) appendLongVariadic(name string, hasValue bool, i int) (int, error) {
	if hasValue {
		s.claude = append(s.claude, s.args[i])
		return i, nil
	}
	if i+1 >= len(s.args) || s.isFlag(s.args[i+1]) {
		return 0, fmt.Errorf("flag --%s requires a value", name)
	}
	s.claude = append(s.claude, s.args[i])
	for i+1 < len(s.args) && !s.isFlag(s.args[i+1]) {
		i++
		s.claude = append(s.claude, s.args[i])
	}
	return i, nil
}

func (s *splitter) splitShort(i int) (int, error) {
	arg := s.args[i]
	switch arg {
	case "-p", "-V", "-h":
		return i, nil
	case "-v", "-c":
		// -v belongs to Claude, not fya version output; -c is Claude continue.
		s.claude = append(s.claude, arg)
		return i, nil
	case "-d", "-r", "-w":
		s.claude = append(s.claude, arg)
		if i+1 >= len(s.args) || s.isFlag(s.args[i+1]) {
			return i, nil
		}
		s.claude = append(s.claude, s.args[i+1])
		return i + 1, nil
	case "-n":
		if i+1 >= len(s.args) || s.isFlag(s.args[i+1]) {
			return 0, fmt.Errorf("flag %s requires a value", arg)
		}
		s.claude = append(s.claude, arg, s.args[i+1])
		return i + 1, nil
	default:
		return 0, fmt.Errorf("unknown flag: %s", arg)
	}
}

func (*splitter) longName(arg string) (string, bool) {
	name := strings.TrimPrefix(arg, "--")
	before, _, ok := strings.Cut(name, "=")
	if ok {
		return before, true
	}
	return name, false
}

func (*splitter) isFlag(s string) bool {
	return strings.HasPrefix(s, "-") && s != "-"
}

func (*splitter) has(names map[string]struct{}, name string) bool {
	_, ok := names[name]
	return ok
}
