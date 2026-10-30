// Package runner executes queued runs on one container backend.
//
// The model is deliberately small: one workspace per run, one executable step at
// a time, one row per step. A rerun is a new run; an uncertain launch is
// reconciled by observing the same workload identity, never by replaying the
// command.
package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
	"github.com/optimiware/kodama/internal/kernel/secret"
	"github.com/optimiware/kodama/internal/kernel/store"
	secretkeys "github.com/optimiware/kodama/pkg/secretkeys"
)

// Spec is one step command the backend must run.
type Spec struct {
	Scope      domain.Scope
	RunID      string
	StepKey    string
	Image      string
	Commands   []string
	Env        map[string]string
	SecretsDir string
	Workspace  string
	Timeout    time.Duration
}

// Result is the observed outcome of one step.
type Result struct {
	ExitCode int
	Log      io.Writer
}

// Backend runs one step. Implementations must not retry the command.
type Backend interface {
	Name() string
	// Prepare creates or adopts the run's workspace.
	Prepare(ctx context.Context, scope domain.Scope, runID string) (workspace string, cleanup func(context.Context) error, err error)
	Execute(ctx context.Context, spec Spec, log io.Writer) (int, error)
}

type Config struct {
	Store          *store.Store
	Backend        Backend
	Keys           secretkeys.Provider
	InstallationID string
	LogDir         string
	Logger         *slog.Logger
	// StepTimeout bounds one step when the document sets none.
	StepTimeout time.Duration
	// SecretDir is where per-step secret files are materialized. It must be a
	// path the container backend can bind mount (a shared path on macOS Podman).
	SecretDir string
}

type Runner struct{ cfg Config }

func New(cfg Config) (*Runner, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("runner: a store is required")
	case cfg.Backend == nil:
		return nil, errors.New("runner: a backend is required")
	case cfg.Keys == nil:
		return nil, errors.New("runner: a key provider is required")
	case cfg.InstallationID == "":
		return nil, errors.New("runner: an installation id is required")
	case cfg.LogDir == "":
		return nil, errors.New("runner: a log directory is required")
	}
	if cfg.StepTimeout <= 0 {
		cfg.StepTimeout = 15 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Runner{cfg: cfg}, nil
}

// Execute claims and runs one queued run. A conflict means another worker won.
func (r *Runner) Execute(ctx context.Context, q store.QueuedRun) error {
	run, err := r.cfg.Store.ClaimRun(ctx, q.TenantID, q.ProjectID, q.RunID, r.cfg.Backend.Name())
	if err != nil {
		return err
	}
	def, err := decodeDefinition(run.Definition)
	if err != nil {
		return r.fail(ctx, run, "invalid_definition", err)
	}
	workspace, cleanup, err := r.cfg.Backend.Prepare(ctx, domain.Scope{TenantID: run.TenantID, ProjectID: run.ProjectID}, run.ID)
	if err != nil {
		return r.fail(ctx, run, "workspace_unavailable", err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := cleanup(cctx); err != nil {
			r.cfg.Logger.Warn("workspace cleanup failed", "run", run.ID, "error", err)
		}
	}()

	steps := map[string]store.Step{}
	for _, s := range run.Steps {
		steps[s.StepKey] = s
	}
	for _, name := range run.ExecutionOrder {
		row, ok := steps[name]
		if !ok {
			return r.fail(ctx, run, "missing_step", fmt.Errorf("run has no step %q", name))
		}
		if current, err := r.cfg.Store.GetRun(ctx, run.TenantID, run.ProjectID, run.ID); err == nil && current.CancelRequestedAt != nil {
			_ = r.cfg.Store.FinishRun(ctx, run.TenantID, run.ProjectID, run.ID, "cancelled", "cancel_requested")
			return nil
		}
		if err := r.cfg.Store.StartStep(ctx, run.TenantID, run.ProjectID, run.ID, row.ID); err != nil {
			return r.fail(ctx, run, "step_start_failed", err)
		}
		logPath := filepath.Join(r.cfg.LogDir, string(run.TenantID)+"_"+string(run.ProjectID)+"_"+run.ID, row.StepKey+".log")
		if err := os.MkdirAll(filepath.Dir(logPath), 0o750); err != nil {
			return r.fail(ctx, run, "log_unavailable", err)
		}
		logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
		if err != nil {
			return r.fail(ctx, run, "log_unavailable", err)
		}
		code, runErr := r.runStep(ctx, run, def, row, workspace, logFile)
		_ = logFile.Close()
		status, reason := "succeeded", ""
		if runErr != nil {
			status, reason = "failed", classify(runErr)
		}
		if code != 0 && status == "succeeded" {
			status, reason = "failed", "command_failed"
		}
		if err := r.cfg.Store.FinishStep(ctx, run.TenantID, run.ProjectID, run.ID, row.ID, status, reason, code, "file://"+logPath); err != nil {
			return err
		}
		if status != "succeeded" {
			return r.fail(ctx, run, reason, runErr)
		}
	}
	if err := r.cfg.Store.FinishRun(ctx, run.TenantID, run.ProjectID, run.ID, "succeeded", ""); err != nil {
		return err
	}
	return r.emit(ctx, run, "run.completed", map[string]any{"runId": run.ID, "status": "succeeded"})
}

// runStep resolves secrets and executes. It returns the exit code and an
// infrastructure error distinct from a command's own non-zero exit.
func (r *Runner) runStep(ctx context.Context, run store.Run, def pipeline.Document, row store.Step, workspace string, log io.Writer) (int, error) {
	var step pipeline.Step
	for _, s := range def.Steps {
		if s.Name == row.StepKey {
			step = s
			break
		}
	}
	if step.Name == "" {
		return 0, fmt.Errorf("step %q is not in the definition snapshot", row.StepKey)
	}
	env := map[string]string{}
	for k, v := range step.Env {
		env[k] = v
	}
	env["KODAMA_TENANT"] = string(run.TenantID)
	env["KODAMA_PROJECT"] = string(run.ProjectID)
	env["KODAMA_RUN_ID"] = run.ID
	env["KODAMA_STEP_KEY"] = row.StepKey
	env["KODAMA_TASK_ID"] = run.TaskID
	env["KODAMA_API_URL"] = os.Getenv("KODAMA_PUBLIC_URL")

	secretsDir, cleanup, err := r.materializeSecrets(ctx, run, step.Secrets)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	timeout := r.cfg.StepTimeout
	if step.Timeout != "" {
		if d, err := time.ParseDuration(step.Timeout); err == nil {
			timeout = d
		}
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	code, err := r.cfg.Backend.Execute(sctx, Spec{
		Scope: domain.Scope{TenantID: run.TenantID, ProjectID: run.ProjectID}, RunID: run.ID,
		StepKey: row.StepKey, Image: row.Image, Commands: step.Commands, Env: env,
		SecretsDir: secretsDir, Workspace: workspace, Timeout: timeout,
	}, log)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return code, fmt.Errorf("%w: step deadline exceeded", ErrTimeout)
	}
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 0, err
	}
	return code, nil
}

// materializeSecrets decrypts only the declared names into a private directory.
// The directory is never the workspace, so it cannot be committed by accident.
func (r *Runner) materializeSecrets(ctx context.Context, run store.Run, names []string) (string, func(), error) {
	dir, err := os.MkdirTemp(r.cfg.SecretDir, "kodama-secrets-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if len(names) == 0 {
		return dir, cleanup, nil
	}
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		res, err := r.cfg.Store.ResolveSecret(ctx, run.TenantID, run.ProjectID, name)
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("%w: secret %q is unavailable", ErrSecret, name)
		}
		plain, err := secret.Open(ctx, r.cfg.Keys, secret.Context{
			InstallationID: r.cfg.InstallationID, TenantID: string(run.TenantID),
			ProjectID: string(res.ProjectID), SecretID: res.ID, Name: res.Name, Version: res.Version,
		}, res.Ciphertext, res.Envelope)
		if err != nil {
			cleanup()
			return "", func() {}, fmt.Errorf("%w: secret %q cannot be decrypted", ErrSecret, name)
		}
		if err := os.WriteFile(filepath.Join(dir, name), plain, 0o400); err != nil {
			clear(plain)
			cleanup()
			return "", func() {}, err
		}
		clear(plain)
	}
	return dir, cleanup, nil
}

func (r *Runner) fail(ctx context.Context, run store.Run, reason string, cause error) error {
	if cause != nil {
		r.cfg.Logger.Warn("run failed", "run", run.ID, "reason", reason, "error", cause)
	}
	if err := r.cfg.Store.FinishRun(ctx, run.TenantID, run.ProjectID, run.ID, "failed", reason); err != nil {
		return err
	}
	_ = r.emit(ctx, run, "run.completed", map[string]any{"runId": run.ID, "status": "failed", "reason": reason})
	return nil
}

func (r *Runner) emit(ctx context.Context, run store.Run, eventType string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return r.cfg.Store.AppendEvent(ctx, run.TenantID, run.ProjectID, "system", "runner", eventType, "run", run.ID, body)
}

// Errors exposed for status classification in tests and logs.
var (
	ErrTimeout = errors.New("runner: timeout")
	ErrSecret  = errors.New("runner: secret")
)

func classify(err error) string {
	switch {
	case errors.Is(err, ErrSecret):
		return "secret_unavailable"
	case errors.Is(err, ErrTimeout):
		return "step_timeout"
	default:
		return "infrastructure"
	}
}

func decodeDefinition(raw json.RawMessage) (pipeline.Document, error) {
	var d pipeline.Document
	if err := json.Unmarshal(raw, &d); err != nil {
		return pipeline.Document{}, err
	}
	if err := d.Validate(); err != nil {
		return pipeline.Document{}, err
	}
	return d, nil
}

// ShellCommand joins commands into one POSIX shell invocation so a step's lines
// share a shell environment exactly as written by the author.
func shellCommands(commands []string) string {
	return strings.Join(commands, "\n")
}
