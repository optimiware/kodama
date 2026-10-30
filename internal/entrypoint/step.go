package entrypoint

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	runtimev2 "github.com/optimiware/kodama/contracts/runtime/v2"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

// Exit codes of `kodama step`. Zero means the control plane durably
// acknowledged the attempt's result, whatever its outcome: a failed command is
// reported through the result, not through the process status.
const (
	ExitOK             = 0
	ExitError          = 1  // unexpected internal error
	ExitUsage          = 2  // bad flags, environment or bootstrap files
	ExitAdmission      = 10 // admission refused or unreachable; command never launched
	ExitRevoked        = 11 // authority lost (revoked, cancelled, token not renewable); child stopped, no result accepted
	ExitResultConflict = 12 // the server holds a different result; not rewritten
	ExitFinalization   = 13 // bounded finalization failed without durable acknowledgment
)

var (
	errAuthorityLost = errors.New("runtime authority lost")
	errTimeout       = errors.New("command timeout")
)

// abortError ends the step without a result submission outcome.
type abortError struct {
	exit int
	err  error
}

func (e *abortError) Error() string { return e.err.Error() }

// Step runs one attempt. Build it with NewStep.
type Step struct {
	cfg Config
	rt  Runtime
	// v2 is set only by NewStepV2: the run_shared wire's admission, context and
	// begin. A Step with a nil v2 is the v1 attempt_private entrypoint, unchanged.
	v2  RuntimeV2
	up  Uploader
	log *slog.Logger
	git *git

	mu     sync.Mutex
	issued [][]byte // admission credential and every runtime token this process held
	gen    uint64   // incremented whenever issued grows (the mirror's redaction set follows it)
}

// NewStep wires the orchestrator to its ports. cfg is completed with defaults.
func NewStep(cfg Config, rt Runtime, up Uploader) *Step {
	cfg.applyDefaults()
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))
	}
	return &Step{
		cfg: cfg, rt: rt, up: up, log: log,
		git: &git{bin: cfg.GitBinary, caFile: cfg.GitCAFile, home: cfg.ScratchDir, pathEnv: cfg.ChildEnv["PATH"]},
	}
}

// verdict is the decided outcome before it is normalized into a Result.
type verdict struct {
	outcome  runtimev1.Outcome
	exit     *int64
	reason   string
	question *string
	source   *runtimev1.GitCommit
	outputs  map[string]runtimev1.Value
}

func failed(reason string, exit *int64) verdict {
	return verdict{outcome: runtimev1.OutcomeFailed, reason: reason, exit: exit}
}

type attempt struct {
	s      *Step
	parent context.Context
	runCtx context.Context
	c      runtimev1.Context
	// c2 is the ContextV2 of a run_shared attempt (c is its v1 projection, which the
	// shared output/source code reads); nil on the v1 wire. sealed is the private
	// Git over the shared workspace, open from verification to the end of the step.
	c2        *runtimev2.ContextV2
	sealed    *sealedGit
	artifacts map[domain.ArtifactID]runtimev1.ArtifactRef
	names     map[string]bool
	diag      []domain.ArtifactID
	diagUsed  int64 // diagnostic bytes uploaded or attempted with unknown outcome
	notes     strings.Builder
	needles   [][]byte
	credForms [][]byte // representations of the checkout credential (redacted from logs, not scanned in source)
	fin       context.Context
	finCancel context.CancelFunc
}

// authorityLost reports whether the attempt's runtime authority ended
// (revoked, or the token could not be renewed). After that no further runtime
// action is taken and no result is submitted.
func (a *attempt) authorityLost() bool {
	return errors.Is(context.Cause(a.runCtx), errAuthorityLost)
}

func (a *attempt) note(format string, args ...any) {
	if a.notes.Len() < 64<<10 {
		fmt.Fprintf(&a.notes, format+"\n", args...)
	}
}

// finalCtx bounds everything after the command: diagnostics, outputs, source
// and the result. After an interrupt of the entrypoint it is detached from the
// canceled parent and given the short interrupt budget.
func (a *attempt) finalCtx() context.Context {
	if a.fin == nil {
		base, d := a.runCtx, a.s.cfg.FinalizeTimeout
		if a.parent.Err() != nil {
			base, d = context.WithoutCancel(a.parent), a.s.cfg.InterruptTimeout
		}
		a.fin, a.finCancel = context.WithTimeout(base, d)
	}
	return a.fin
}

// Run executes the attempt lifecycle and returns the process exit code.
func (s *Step) Run(ctx context.Context) int {
	cred, err := readTrimmed(s.cfg.AdmissionCredentialFile)
	if err != nil {
		s.log.Error("cannot read admission credential", "error", err.Error())
		return ExitUsage
	}
	s.remember(cred)
	nonce, err := newNonce()
	if err != nil {
		s.log.Error("nonce", "error", err.Error())
		return ExitError
	}
	admitted, err := s.admit(ctx, runtimev1.AdmitRequest{
		AdmissionCredential: cred, Nonce: nonce, ClientVersion: s.cfg.ClientVersion, Job: s.cfg.Job,
	})
	if err != nil {
		s.log.Error("admission failed", "error", err.Error())
		if isRevoked(err) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	if string(admitted.c.AttemptID) != s.cfg.AttemptID {
		s.log.Error("admitted context belongs to another attempt")
		return ExitAdmission
	}
	s.writeToken(admitted.token.Token)
	s.log.Info("admitted", "attempt", s.cfg.AttemptID, "tokenExpiresAt", admitted.token.ExpiresAt)

	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	renewDone := make(chan struct{})
	go func() {
		defer close(renewDone)
		s.renewLoop(runCtx, cancel, admitted.token)
	}()
	defer func() { cancel(nil); <-renewDone }()

	fetched, err := s.fetch(runCtx)
	if err != nil {
		s.log.Error("context fetch failed", "error", err.Error())
		if isRevoked(err) || errors.Is(context.Cause(runCtx), errAuthorityLost) {
			return ExitRevoked
		}
		return ExitAdmission
	}
	// The HTTP adapter validates responses; another Runtime implementation
	// must not be able to make the entrypoint dereference a missing source or
	// launch a command from a malformed context.
	if err := fetched.validate(); err != nil {
		s.log.Error("the runtime returned an invalid context", "error", err.Error())
		return ExitAdmission
	}
	a := &attempt{
		s: s, parent: ctx, runCtx: runCtx, c: fetched.c, c2: fetched.c2,
		artifacts: map[domain.ArtifactID]runtimev1.ArtifactRef{}, names: map[string]bool{},
	}
	defer func() {
		if a.finCancel != nil {
			a.finCancel()
		}
		if a.sealed != nil {
			a.sealed.Close()
		}
	}()
	var v verdict
	var abort *abortError
	if !reflect.DeepEqual(admitted.c, fetched.c) || !reflect.DeepEqual(admitted.c2, fetched.c2) {
		a.note("context returned by admit differs from GET /context; refusing to launch")
		v = failed(ReasonContextMismatch, nil)
	} else {
		v, abort = a.execute()
	}
	if abort != nil {
		s.log.Error("step aborted", "exit", abort.exit, "error", abort.err.Error())
		return abort.exit
	}
	return a.finalize(v)
}

func (a *attempt) execute() (verdict, *abortError) {
	c, cfg := a.c, a.s.cfg
	argv := append(append([]string{}, c.Command.Command...), c.Command.Args...)
	if len(argv) == 0 || argv[0] == "" {
		a.note("the context declares no command")
		return failed(ReasonCommandStartFailed, nil), nil
	}
	dirs := []string{cfg.WorkspaceDir, cfg.OutputsDir}
	if a.c2 != nil {
		// The run workspace is the controller's mount and is never created here: a
		// missing /workspace is a lost scratch, reported by the verification below.
		dirs = dirs[1:]
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			a.note("cannot prepare %s: %v", d, err)
			return failed(ReasonCommandStartFailed, nil), nil
		}
	}
	if err := os.Remove(cfg.ResultsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
		a.note("cannot clear previous result manifest: %v", err)
		return failed(ReasonCommandStartFailed, nil), nil
	}

	var begin *runtimev2.BeginRequest
	if a.c2 != nil {
		// run_shared: no clone, reset or clean and no checkout credential. The
		// controller prepared the scratch once; this only looks at it.
		req, v := a.verifyWorkspace()
		if v != nil {
			return *v, nil
		}
		begin = req
	} else if c.Workspace.Access != runtimev1.AccessNone {
		if v := a.checkout(); v != nil {
			return *v, nil
		}
	}

	secrets, err := materializeSecrets(a.runCtx, a.s.rt, c.Secrets, cfg.SecretsDir)
	defer secrets.cleanup()
	if err != nil {
		if ab := a.classify(a.runCtx, err); ab != nil {
			return verdict{}, ab
		}
		a.note("secret delivery failed: %v", redactErr(err))
		return failed(ReasonSecretUnavailable, nil), nil
	}
	// Independent copies: secrets.cleanup zeroes the originals when execute
	// returns, but the notes are redacted afterwards in finalize.
	for _, v := range secrets.values {
		a.needles = append(a.needles, append([]byte(nil), v...))
	}

	// Authority revoked (or the entrypoint interrupted) while preparing: the
	// command must never be launched.
	if a.authorityLost() {
		return verdict{}, &abortError{ExitRevoked, context.Cause(a.runCtx)}
	}
	if a.parent.Err() != nil {
		a.note("the entrypoint was interrupted before the command was launched")
		return failed(ReasonInterrupted, nil), nil
	}

	if begin != nil {
		// The command-begin fence: the server records, durably and ordered against
		// cancellation, that this attempt may run user code. The command starts only
		// after the acknowledgment; a refusal or an unacknowledged call starts nothing.
		if ab := a.recordBegin(*begin); ab != nil {
			return verdict{}, ab
		}
	}

	logCap := commandLogCapacity(c.Limits.MaxDiagnosticBytes)
	logw, err := newBoundedLog(cfg.ScratchDir, "kodama-command-*.log", logCap)
	if err != nil {
		a.note("cannot create diagnostic log: %v", err)
		return failed(ReasonCommandStartFailed, nil), nil
	}
	// The container-log mirror sees exactly the bytes the capture sees, through
	// the same redaction set (see mirror.go). It cannot delay or fail the capture.
	var out io.Writer = logw
	mir := newContainerMirror(cfg.ContainerLog, dataCapacity(logCap), a.logNeedlesGen, a.s.log)
	if mir != nil {
		out = mirrorTee{capture: logw, mirror: mir}
	}
	timeout := time.Duration(c.Limits.TimeoutSeconds) * time.Second
	childCtx, cancelChild := context.WithTimeoutCause(a.runCtx, timeout, errTimeout)
	a.s.log.Info("launching command", "argv0", argv[0], "timeoutSeconds", c.Limits.TimeoutSeconds)
	launched := time.Now()
	res := runProcess(childCtx, procSpec{argv: argv, dir: cfg.WorkspaceDir, env: a.childEnv(secrets), out: out, grace: cfg.KillGrace})
	a.s.log.Info("command finished", "durationMs", time.Since(launched).Milliseconds())
	cause := context.Cause(childCtx)
	cancelChild()
	if mir != nil {
		// The held tail is released only for output observed to its end.
		mir.Close(res.started && res.drainErr == nil && !res.drainIncomplete && cause == nil, cfg.KillGrace)
	}
	// The copier has stopped (or the pipe was abandoned): freeze the capture
	// once, then sanitize and upload only the frozen bytes.
	logPath, _, state := logw.freeze()
	defer os.Remove(logPath)

	if a.authorityLost() {
		return verdict{}, &abortError{ExitRevoked, context.Cause(a.runCtx)}
	}
	interrupted := a.parent.Err() != nil
	ev := captureEvidence{state: state, drainErr: res.drainErr, incomplete: res.drainIncomplete, stopped: cause != nil}
	if res.startErr == nil {
		if ab := a.uploadCommandLog(logPath, ev); ab != nil {
			return verdict{}, ab
		}
	}

	switch {
	case res.startErr != nil:
		a.note("command could not be started: %v", res.startErr)
		return failed(ReasonCommandStartFailed, nil), nil
	case interrupted:
		a.note("the entrypoint was interrupted; the command was stopped")
		return failed(ReasonInterrupted, nil), nil
	case errors.Is(cause, errTimeout):
		a.note("the command exceeded its %ds timeout and was stopped", c.Limits.TimeoutSeconds)
		return failed(ReasonTimeout, nil), nil
	case res.exitCode == nil:
		a.note("the command was terminated by a signal")
		return failed(ReasonCommandFailed, nil), nil
	case *res.exitCode != 0:
		a.note("the command exited with status %d", *res.exitCode)
		return failed(ReasonExitNonzero, res.exitCode), nil
	}
	return a.afterSuccessfulExit(res.exitCode)
}

// afterSuccessfulExit validates declared outputs and, for a writer, publishes
// source. Exit zero alone is never success.
func (a *attempt) afterSuccessfulExit(exit *int64) (verdict, *abortError) {
	zero := exit
	m, err := readManifest(a.s.cfg.ResultsFile)
	if err != nil {
		var oe *outputError
		errors.As(err, &oe)
		a.note("%v", err)
		return failed(oe.reason, zero), nil
	}
	if m != nil {
		switch runtimev1.Outcome(*m.Outcome) {
		case runtimev1.OutcomeFailed:
			reason := ReasonCommandReportedFail
			if m.Reason != nil {
				reason = *m.Reason
			}
			if err := (runtimev1.Result{Outcome: runtimev1.OutcomeFailed, Reason: &reason, Outputs: map[string]runtimev1.Value{}, DiagnosticArtifactIDs: []domain.ArtifactID{}}).Validate(); err != nil {
				a.note("manifest reason is not a valid reason code")
				return failed(ReasonManifestInvalid, zero), nil
			}
			a.note("the command reported failure through its manifest (reason %s)", reason)
			return failed(reason, zero), nil
		case runtimev1.OutcomeNeedsInput:
			return verdict{outcome: runtimev1.OutcomeNeedsInput, exit: zero, question: m.Question}, nil
		}
	}
	outputs, err := a.collect(m)
	if err != nil {
		var ab *abortError
		var oe *outputError
		switch {
		case errors.As(err, &ab):
			return verdict{}, ab
		case errors.As(err, &oe):
			a.note("%v", oe)
			return failed(oe.reason, zero), nil
		default:
			a.note("output transfer failed: %v", redactErr(err))
			reason := ReasonOutputUploadFailed
			if apiCode(err) == runtimev1.CodeOutputInvalid {
				reason = ReasonOutputInvalid
			}
			return failed(reason, zero), nil
		}
	}
	v := verdict{outcome: runtimev1.OutcomeCompleted, exit: zero, outputs: outputs}
	if a.c.Workspace.Access == runtimev1.AccessWrite {
		commit, fail, ab := a.publish()
		if ab != nil {
			return verdict{}, ab
		}
		if fail != nil {
			fail.exit = zero
			return *fail, nil
		}
		v.source = &commit
	}
	return v, nil
}

func (a *attempt) checkout() *verdict {
	cfg, src := a.s.cfg, a.c.Workspace.Source
	var cred *checkoutCredential
	if data, err := os.ReadFile(cfg.CheckoutCredentialFile); err == nil {
		if cred, err = parseCheckoutCredential(data); err != nil {
			a.note("checkout credential: %v", err)
			v := failed(ReasonCheckoutFailed, nil)
			return &v
		}
		a.s.git.redactors = []string{cred.header(), cred.username + ":" + cred.password, base64.StdEncoding.EncodeToString([]byte(cred.username + ":" + cred.password)), cred.password}
		a.needles = append(a.needles, []byte(cred.password))
		for _, form := range a.s.git.redactors {
			a.credForms = append(a.credForms, []byte(form))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		a.note("checkout credential: cannot read file")
		v := failed(ReasonCheckoutFailed, nil)
		return &v
	}
	a.s.log.Info("checking out", "repository", src.RepositoryURL, "commit", src.Commit, "authenticated", cred != nil)
	if err := a.s.git.checkout(a.runCtx, cfg.WorkspaceDir, src.RepositoryURL, src.Commit, cred); err != nil {
		a.note("checkout of %s failed: %v", src.Commit, err)
		a.s.log.Error("checkout failed", "error", err.Error())
		v := failed(ReasonCheckoutFailed, nil)
		return &v
	}
	return nil
}

// classify turns an error into an abort when authority is gone or the bounded
// finalization budget is spent; otherwise it returns nil.
func (a *attempt) classify(ctx context.Context, err error) *abortError {
	switch {
	case isRevoked(err) || a.authorityLost():
		return &abortError{ExitRevoked, err}
	case ctx.Err() != nil:
		return &abortError{ExitFinalization, err}
	}
	return nil
}

func (a *attempt) childEnv(secrets *secretSet) []string {
	cfg, c := a.s.cfg, a.c
	env := map[string]string{}
	for k, v := range cfg.ChildEnv {
		env[k] = v
	}
	env["KODAMA_ATTEMPT_ID"] = string(c.AttemptID)
	env["KODAMA_RUN_ID"] = string(c.RunID)
	env["KODAMA_STEP_ID"] = c.StepID
	env["KODAMA_WORKSPACE"] = cfg.WorkspaceDir
	env["KODAMA_OUTPUTS_DIR"] = cfg.OutputsDir
	env["KODAMA_RESULTS_FILE"] = cfg.ResultsFile
	// The command is told the directory its files are actually in: the
	// process-owned child of the volume (when secrets were declared), never the
	// volume root.
	env["KODAMA_SECRETS_DIR"] = cfg.SecretsDir
	if secrets.dir != "" {
		env["KODAMA_SECRETS_DIR"] = secrets.dir
	}
	for name, v := range c.Inputs {
		key := "KODAMA_INPUT_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		switch {
		case v.IsString():
			env[key] = v.String()
		case v.IsBool():
			b, _ := v.MarshalJSON()
			env[key] = string(b)
		default:
			b, _ := v.MarshalJSON()
			env[key] = string(b)
		}
	}
	for k, v := range c.Command.Env {
		env[k] = v
	}
	for k, v := range secrets.env {
		env[k] = v
	}
	// The same volume-ownership exception the entrypoint's own git uses. A step
	// that already configures git through the environment keeps its own values.
	if _, ok := env["GIT_CONFIG_COUNT"]; !ok && cfg.WorkspaceDir != "" {
		env["GIT_CONFIG_COUNT"] = "1"
		env["GIT_CONFIG_KEY_0"] = "safe.directory"
		env["GIT_CONFIG_VALUE_0"] = cfg.WorkspaceDir
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// uploadCommandLog sanitizes the frozen capture and registers it as the
// `command.log` diagnostic, then registers the completeness sidecar bound to
// the acknowledged artifact. A failed upload does not fail the step;
// revocation or an exhausted finalization budget aborts it. Without a verified
// log there is no sidecar; without a sidecar completeness is unknown.
func (a *attempt) uploadCommandLog(rawPath string, ev captureEvidence) *abortError {
	capacity := commandLogCapacity(a.c.Limits.MaxDiagnosticBytes)
	fl, err := sanitizeCapture(rawPath, a.s.cfg.ScratchDir, a.logNeedles(), capacity, ev)
	if err != nil {
		// Never fall back to the raw bytes.
		a.note("the command output was not retained: it could not be sanitized")
		a.s.log.Warn("command log withheld", "reason", sanitizeReason(err))
		return nil
	}
	defer fl.cleanup()
	if fl.size == 0 && (fl.completeness != contract.Complete || a.diagnosticLeft() < sidecarReserve) {
		// Nothing to keep, or no room to say it was complete.
		return nil
	}
	id, digest, ab := a.uploadDiagnostic(fl.path, contract.CommandLogName, contract.LogMediaType, fl.size)
	if ab != nil || id == "" {
		return ab
	}
	meta, err := commandLogMetadata(id, digest, fl)
	if err != nil {
		a.s.log.Warn("log completeness metadata unavailable", "error", err.Error())
		return nil
	}
	if int64(len(meta)) > a.diagnosticLeft() {
		a.s.log.Warn("log completeness metadata does not fit the diagnostic budget")
		return nil
	}
	f, err := os.CreateTemp(a.s.cfg.ScratchDir, "kodama-command-meta-*.json")
	if err != nil {
		return nil
	}
	defer os.Remove(f.Name())
	_, werr := f.Write(meta)
	if cerr := f.Close(); werr != nil || cerr != nil {
		return nil
	}
	name, err := contract.MetadataName(contract.CommandLogName)
	if err != nil {
		return nil
	}
	_, _, ab = a.uploadDiagnostic(f.Name(), name, contract.MediaType, int64(len(meta)))
	return ab
}

func sanitizeReason(err error) string {
	if errors.Is(err, errRedactionSet) {
		return "redaction set not usable"
	}
	return "collection failed"
}

// uploadDiagnostic uploads one diagnostic file within the aggregate budget and
// cites it once verified. It returns the artifact ID and digest only for a
// verified upload.
func (a *attempt) uploadDiagnostic(path, name, mediaType string, size int64) (domain.ArtifactID, runtimev1.Digest, *abortError) {
	ctx := a.finalCtx()
	if size > a.diagnosticLeft() {
		a.s.log.Warn("diagnostic exceeds the remaining budget", "name", name)
		return "", "", nil
	}
	f, err := os.Open(path)
	if err != nil {
		a.s.log.Warn("diagnostic unavailable", "error", err.Error())
		return "", "", nil
	}
	defer f.Close()
	digest, err := hashFile(f, size)
	if err != nil {
		a.s.log.Warn("diagnostic unreadable", "error", err.Error())
		return "", "", nil
	}
	// Count the bytes before the attempt: a lost response leaves the
	// reservation outcome unknown, so it is conservatively spent.
	a.diagUsed += size
	mt, sz := mediaType, size
	id, err := a.uploadFile(ctx, f, runtimev1.ArtifactReserveRequest{
		Name: name, Kind: runtimev1.KindFile, Role: runtimev1.RoleDiagnostic, Digest: digest, MediaType: &mt, SizeBytes: &sz,
	})
	if err != nil {
		if ab := a.classify(ctx, err); ab != nil {
			return "", "", ab
		}
		a.s.log.Warn("diagnostic upload failed", "name", name, "error", redactErr(err))
		return "", "", nil
	}
	a.diag = append(a.diag, id)
	return id, digest, nil
}

// uploadFile reserves, transfers and completes one file artifact and records
// it as usable.
func (a *attempt) uploadFile(ctx context.Context, f *os.File, req runtimev1.ArtifactReserveRequest) (domain.ArtifactID, error) {
	res, err := a.s.rt.ReserveArtifact(ctx, req)
	if err != nil {
		return "", err
	}
	if res.State != runtimev1.StateReserved || res.Upload == nil {
		return "", &ProtocolError{Op: "reserveArtifact", Err: errors.New("file artifact was not reserved with upload instructions")}
	}
	if err := a.s.up.Upload(ctx, *res.Upload, f, *req.SizeBytes); err != nil {
		return "", err
	}
	done, err := a.s.rt.CompleteArtifact(ctx, res.ArtifactID, runtimev1.ArtifactCompleteRequest{Digest: req.Digest, SizeBytes: *req.SizeBytes})
	if err != nil {
		return "", err
	}
	if done.State != runtimev1.StateUsable || done.ArtifactID != res.ArtifactID || done.Digest != req.Digest || done.SizeBytes != *req.SizeBytes {
		return "", &ProtocolError{Op: "completeArtifact", Err: errors.New("completion does not match the upload")}
	}
	a.artifacts[res.ArtifactID] = runtimev1.ArtifactRef{ID: res.ArtifactID, Scope: a.c.Scope(), Attempt: a.c.AttemptID, Kind: runtimev1.KindFile, Usable: true}
	return res.ArtifactID, nil
}

// collect validates every declared output locally, then transfers them. It
// returns an *outputError for an invalid or missing output (nothing uploaded),
// an *abortError, or a transfer error.
func (a *attempt) collect(m *manifestDoc) (map[string]runtimev1.Value, error) {
	c, cfg := a.c, a.s.cfg
	declared := map[string]runtimev1.OutputDecl{}
	for _, o := range c.Outputs {
		declared[o.Name] = o
	}
	if m != nil {
		for name := range m.Outputs {
			if d, ok := declared[name]; !ok || d.Type == runtimev1.OutputArtifact {
				return nil, outputErr(ReasonOutputInvalid, "manifest output %q is undeclared or a file output", name)
			}
		}
	}
	if int64(len(c.Outputs)) > 0 {
		var files int64
		for _, o := range c.Outputs {
			if o.Type == runtimev1.OutputArtifact {
				files++
			}
		}
		if files > c.Limits.MaxOutputFiles {
			return nil, outputErr(ReasonOutputInvalid, "%d file outputs exceed the limit of %d", files, c.Limits.MaxOutputFiles)
		}
	}

	scalars := map[string]runtimev1.Value{}
	ocis := map[string]ociValue{}
	var files []*fileOutput
	defer func() {
		for _, f := range files {
			f.file.Close()
		}
	}()
	var total int64
	taken := map[string]bool{}
	for _, o := range c.Outputs {
		switch o.Type {
		case runtimev1.OutputArtifact:
			f, fi, err := openOutputFile(cfg.OutputsDir, *o.Path)
			if err != nil {
				return nil, err
			}
			art, err := artifactName(o.Name, taken)
			if err != nil {
				f.Close()
				return nil, err
			}
			out := &fileOutput{name: o.Name, file: f, size: fi.Size(), mediaType: mediaTypeFor(*o.Path), artifact: art}
			files = append(files, out)
			if total += fi.Size(); total > c.Limits.MaxOutputBytes {
				return nil, outputErr(ReasonOutputInvalid, "declared files exceed %d bytes", c.Limits.MaxOutputBytes)
			}
			if out.digest, err = hashFile(f, fi.Size()); err != nil {
				return nil, outputErr(ReasonOutputInvalid, "declared output %q: %v", *o.Path, err)
			}
		case runtimev1.OutputOCIArtifact, runtimev1.OutputString, runtimev1.OutputBoolean, runtimev1.OutputInteger:
			if m == nil {
				return nil, outputErr(ReasonOutputMissing, "declared output %q requires %s, which was not written", o.Name, cfg.ResultsFile)
			}
			raw, ok := m.Outputs[o.Name]
			if !ok {
				return nil, outputErr(ReasonOutputMissing, "declared output %q is not in the manifest", o.Name)
			}
			if o.Type == runtimev1.OutputOCIArtifact {
				oci, err := ociValueOf(o.Name, raw)
				if err != nil {
					return nil, err
				}
				ocis[o.Name] = oci
			} else {
				v, err := scalarValue(o.Name, o.Type, raw)
				if err != nil {
					return nil, err
				}
				scalars[o.Name] = v
			}
		}
	}

	ctx := a.finalCtx()
	outputs := map[string]runtimev1.Value{}
	for n, v := range scalars {
		outputs[n] = v
	}
	for _, f := range files {
		mt, sz := f.mediaType, f.size
		id, err := a.uploadFile(ctx, f.file, runtimev1.ArtifactReserveRequest{
			Name: f.artifact, Kind: runtimev1.KindFile, Role: runtimev1.RoleOutput, Digest: f.digest, MediaType: &mt, SizeBytes: &sz,
		})
		if err != nil {
			return nil, a.transferError(ctx, err)
		}
		outputs[f.name] = runtimev1.StringValue(string(id))
	}
	for _, o := range c.Outputs {
		oci, ok := ocis[o.Name]
		if !ok {
			continue
		}
		reg := oci.RegistryRef
		res, err := a.s.rt.ReserveArtifact(ctx, runtimev1.ArtifactReserveRequest{
			Name: o.Name, Kind: runtimev1.KindOCI, Role: runtimev1.RoleOutput, Digest: oci.Digest, Registry: &reg,
		})
		if err != nil {
			return nil, a.transferError(ctx, err)
		}
		if res.State != runtimev1.StateRegistered {
			return nil, &ProtocolError{Op: "reserveArtifact", Err: errors.New("oci artifact was not registered")}
		}
		a.artifacts[res.ArtifactID] = runtimev1.ArtifactRef{ID: res.ArtifactID, Scope: c.Scope(), Attempt: c.AttemptID, Kind: runtimev1.KindOCI, Usable: true}
		outputs[o.Name] = runtimev1.StringValue(string(res.ArtifactID))
	}
	return outputs, nil
}

func (a *attempt) transferError(ctx context.Context, err error) error {
	if ab := a.classify(ctx, err); ab != nil {
		return ab
	}
	return err
}

func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return s, nil
}

func newNonce() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// remember keeps a credential this process handled so that source containing
// it is never published. It is held in memory only.
func (s *Step) remember(credential string) {
	if credential == "" {
		return
	}
	s.mu.Lock()
	s.issued = append(s.issued, []byte(credential))
	s.gen++
	s.mu.Unlock()
}

// secretNeedles lists every value that must never appear in published source:
// injected secrets, the checkout credential and the admission/runtime tokens.
func (a *attempt) secretNeedles() [][]byte {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	return append(append([][]byte{}, a.needles...), a.s.issued...)
}

// logNeedles is secretNeedles plus the checkout credential's representations:
// everything that must not reach a retained or mirrored log.
func (a *attempt) logNeedles() [][]byte {
	n, _ := a.logNeedlesGen()
	return n
}

// logNeedlesGen also returns the generation of the issued-token set; it
// changes when a token is issued or refreshed.
func (a *attempt) logNeedlesGen() ([][]byte, uint64) {
	a.s.mu.Lock()
	defer a.s.mu.Unlock()
	n := append(append([][]byte{}, a.needles...), a.credForms...)
	return append(n, a.s.issued...), a.s.gen
}

// writeToken publishes the current runtime token atomically (0600) so the
// wrapper's own tooling can read it. A failure is logged, never fatal.
func (s *Step) writeToken(token string) {
	s.remember(token)
	if s.cfg.TokenFile == "" {
		return
	}
	dir := filepath.Dir(s.cfg.TokenFile)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.log.Warn("cannot write token file", "error", err.Error())
		return
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		s.log.Warn("cannot write token file", "error", err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.WriteString(token)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), s.cfg.TokenFile)
	}
	if werr != nil {
		s.log.Warn("cannot write token file", "error", werr.Error())
	}
}

// renewLoop renews the token at half its remaining life (never earlier than
// RenewMinInterval). A denied renewal (revoked, forbidden, invalid) or a token
// that cannot be renewed past its maximum cancels the run with
// errAuthorityLost: the child is stopped and the loss is surfaced.
func (s *Step) renewLoop(ctx context.Context, cancel context.CancelCauseFunc, tok runtimev1.TokenInfo) {
	lose := func(why error) { cancel(fmt.Errorf("%w: %v", errAuthorityLost, why)) }
	for {
		exp, _ := tok.ExpiresAt.Time()
		maxAt, _ := tok.MaxExpiresAt.Time()
		if !exp.Before(maxAt) {
			// Final token: nothing left to renew. Stop before it expires so
			// the result can still be submitted.
			if sleep(ctx, time.Until(maxAt.Add(-s.cfg.RenewReserve))) == nil {
				lose(errors.New("maximum token lifetime reached"))
			}
			return
		}
		wait := max(time.Until(exp)/2, s.cfg.RenewMinInterval)
		if sleep(ctx, wait) != nil {
			return
		}
		resp, err := s.rt.RenewToken(ctx)
		switch {
		case err == nil:
			tok = resp.Token
			s.writeToken(tok.Token)
			s.log.Debug("token renewed", "expiresAt", tok.ExpiresAt)
		case ctx.Err() != nil:
			return
		case isTransport(err):
			if time.Now().After(exp) {
				lose(errors.New("token expired while renewal was unreachable"))
				return
			}
			s.log.Warn("token renewal unreachable; will retry", "error", err.Error())
			if sleep(ctx, s.cfg.RenewMinInterval) != nil {
				return
			}
		default:
			s.log.Error("token renewal denied", "error", err.Error())
			lose(err)
			return
		}
	}
}

func redactErr(err error) string {
	var te *TransportError
	if errors.As(err, &te) {
		return te.Error()
	}
	return err.Error()
}
