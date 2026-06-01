// Package main provides fya - a Claude print-compatible PTY wrapper.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"

	log "github.com/go-pkgz/lgr"

	"github.com/umputun/fya/app/input"
	"github.com/umputun/fya/app/options"
	"github.com/umputun/fya/app/ready"
	"github.com/umputun/fya/app/stream"
	"github.com/umputun/fya/app/transcript"
	"github.com/umputun/fya/app/turn"
	"github.com/umputun/fya/app/typing"
)

var revision = "unknown"

type turnExecutor interface {
	Run(context.Context, turn.Config) error
}

type turnRunnerFactory func(stdout, stderr io.Writer, cfg options.Config) turnExecutor

func defaultTurnRunner(stdout, stderr io.Writer, cfg options.Config) turnExecutor {
	output := stream.NewWriter(stdout, stream.Config{Format: cfg.OutputFormat})
	return turn.NewRunner(turn.Dependencies{
		ProcessStarter: turn.NewPTYStarter(),
		Readiness: ready.NewDetector(ready.Config{
			Timeout:         cfg.ReadinessTimeout,
			Warn:            stderr,
			NonFatalTimeout: true,
		}),
		Injector: typing.NewInjector(typing.Config{
			WPM:         cfg.TypingWPM,
			Jitter:      cfg.TypingJitter,
			MaxWPMSize:  cfg.MaxWPMSize,
			TurnTimeout: cfg.TurnTimeout,
			Warn:        stderr,
		}),
		Catalog: transcript.NewCatalog(os.Getenv("FYA_CLAUDE_DIR")),
		TailerFactory: func(path string) turn.Tailer {
			return transcript.NewTailer(path)
		},
		Output: output,
	})
}

// request groups the per-invocation inputs to execute/run.
type request struct {
	Args    []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
	Rev     string
	Factory turnRunnerFactory
}

func main() {
	req := request{
		Args:    os.Args[1:],
		Stdin:   os.Stdin,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Rev:     resolveVersion(),
		Factory: defaultTurnRunner,
	}
	if err := execute(context.Background(), req); err != nil {
		log.Printf("[ERROR] %v", err)
		os.Exit(1)
	}
}

func execute(parent context.Context, req request) error {
	parser := options.NewParser()
	cfg, err := parser.Parse(req.Args)
	if err != nil {
		if errors.Is(err, options.ErrHelp) {
			parser.WriteHelp(req.Stdout)
			return nil
		}
		return fmt.Errorf("parse options: %w", err)
	}

	if cfg.Version {
		_, err := fmt.Fprintf(req.Stdout, "fya %s\nversion: %s\ngo: %s\n", req.Rev, req.Rev, runtime.Version())
		if err != nil {
			return fmt.Errorf("write version: %w", err)
		}
		return nil
	}

	setupLog(cfg.Debug)

	defer func() {
		if x := recover(); x != nil {
			log.Printf("[WARN] run time panic:\n%v", x)
			panic(x)
		}
	}()

	ctx, cancel := context.WithCancel(parent)
	defer installSignals(ctx, cancel, req.Stderr)()

	return run(ctx, cfg, req)
}

func run(ctx context.Context, cfg options.Config, req request) error {
	prompt, err := input.NewReader(input.Request{
		Args:               cfg.PromptArgs,
		Stdin:              req.Stdin,
		StdinHasData:       stdinHasData(req.Stdin),
		Stdout:             req.Stdout,
		InputFormat:        cfg.InputFormat,
		ReplayUserMessages: cfg.ReplayUserMessages,
	}).Read()
	if err != nil {
		return fmt.Errorf("read prompt: %w", err)
	}

	if err := req.Factory(req.Stdout, req.Stderr, cfg).Run(ctx, turn.Config{
		ClaudeArgs:   cfg.ClaudeArgs,
		CWD:          cfg.CWD,
		TurnTimeout:  cfg.TurnTimeout,
		IdleTimeout:  cfg.IdleTimeout,
		StreamEvents: cfg.OutputFormat == stream.FormatStreamJSON,
		Prompt:       prompt,
	}); err != nil {
		return fmt.Errorf("run turn: %w", err)
	}
	return nil
}

func stdinHasData(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return true
	}
	stat, err := f.Stat()
	if err != nil {
		return true
	}
	return stat.Mode()&os.ModeCharDevice == 0
}

func resolveVersion() string {
	bi, ok := debug.ReadBuildInfo()
	return resolveBuildVersion(revision, bi, ok)
}

func resolveBuildVersion(rev string, bi *debug.BuildInfo, ok bool) string {
	if rev != "unknown" {
		return rev
	}
	if !ok || bi == nil {
		return rev
	}
	if bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return rev
}

func setupLog(debugMode bool) {
	opts := []log.Option{log.Msec, log.Out(os.Stderr), log.Err(os.Stderr)}
	if debugMode {
		opts = append(opts, log.Debug, log.CallerFunc, log.CallerPkg, log.CallerFile)
	}
	log.Setup(opts...)
}

// installSignals wires SIGINT/SIGTERM to cancel and SIGQUIT to a stack dump
// emitted to stderr (never stdout — stdout is the JSONL channel consumed by
// Ralphex). It returns a cleanup function that stops signal delivery and the
// dispatch goroutine, preventing leaks across repeated execute calls in tests.
func installSignals(ctx context.Context, cancel context.CancelFunc, stderr io.Writer) func() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	done := make(chan struct{})
	go func() {
		defer close(done)
		stacktrace := make([]byte, 8192)
		for {
			select {
			case sig := <-sigChan:
				switch sig {
				case syscall.SIGQUIT:
					length := runtime.Stack(stacktrace, true)
					if stderr != nil {
						_, _ = fmt.Fprintln(stderr, string(stacktrace[:length]))
					} else {
						log.Printf("[INFO] stacktrace:\n%s", stacktrace[:length])
					}
				case syscall.SIGTERM, syscall.SIGINT:
					cancel()
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigChan)
		cancel()
		<-done
	}
}
