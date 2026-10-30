package entrypoint

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"syscall"

	"github.com/optimiware/kodama/internal/domain"
)

// CommandName is the subcommand the composition root dispatches to Run.
const CommandName = "step"

// Command describes the `kodama step` subcommand for the composition root,
// which owns main dispatch (it is wired outside this package).
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int
}

// StepCommand returns the `kodama step` registration.
func StepCommand() Command {
	return Command{Name: CommandName, Summary: "Run one attempt's command inside a Job Pod", Run: Run}
}

// Run parses args (the arguments after "step") and the environment, runs the
// step and returns its exit code (see the Exit constants).
func Run(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	var output bytes.Buffer
	cfg, err := ParseConfig(args, getenv, &output)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			_, _ = io.Copy(stdout, &output)
			return ExitOK
		}
		if output.Len() > 0 {
			_, _ = io.Copy(stderr, &output)
		}
		fmt.Fprintln(stderr, "kodama step:", err)
		return ExitUsage
	}
	// The entrypoint's own log lines and the mirrored command output share the
	// container's stderr through one bounded, nonblocking stream: a stalled
	// stderr drops diagnostics but can never block the step's lifecycle.
	container := newContainerStream(stderr)
	defer container.Close(streamFlushWait)
	cfg.Logger = slog.New(slog.NewJSONHandler(container, &slog.HandlerOptions{Level: cfg.LogLevel}))
	cfg.ContainerLog = container
	// SIGTERM (kubelet) and SIGINT stop the command's process group and leave
	// a short budget to report the interruption; see Step.Run.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return Execute(ctx, cfg)
}

// Execute runs one step with the real HTTP adapter. cfg must be complete
// (ParseConfig output, or a hand-built Config); defaults fill the rest.
func Execute(ctx context.Context, cfg Config) int {
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Error("invalid configuration", "error", err.Error())
		}
		return ExitUsage
	}
	if cfg.SourceSetRun() {
		// v3: the run_shared wire with a source set. Every source is verified, never
		// checked out; the attempt's Git grant pushes explicitly.
		client, err := NewClientV3(cfg.ServerURL, domain.AttemptID(cfg.AttemptID), cfg.CAFile, cfg.Retry, cfg.Logger)
		if err != nil {
			if cfg.Logger != nil {
				cfg.Logger.Error("cannot create runtime client", "error", err.Error())
			}
			return ExitUsage
		}
		return NewStepV3(cfg, client, client, client).Run(ctx)
	}
	if cfg.SharedRun() {
		// run_shared: the v2 routes and the begin fence; the workspace is verified,
		// never checked out, and no checkout credential is read.
		client, err := NewClientV2(cfg.ServerURL, domain.AttemptID(cfg.AttemptID), cfg.CAFile, cfg.Retry, cfg.Logger)
		if err != nil {
			if cfg.Logger != nil {
				cfg.Logger.Error("cannot create runtime client", "error", err.Error())
			}
			return ExitUsage
		}
		return NewStepV2(cfg, client, client, client).Run(ctx)
	}
	client, err := NewClient(cfg.ServerURL, domain.AttemptID(cfg.AttemptID), cfg.CAFile, cfg.Retry, cfg.Logger)
	if err != nil {
		if cfg.Logger != nil {
			cfg.Logger.Error("cannot create runtime client", "error", err.Error())
		}
		return ExitUsage
	}
	return NewStep(cfg, client, client).Run(ctx)
}
