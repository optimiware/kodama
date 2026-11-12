package services

// The concrete Git merge/rebase boundary.
//
// Execute fetches the exact base and candidate from the configured remote, then
// fast-forwards when the candidate already descends from the base and rebases
// otherwise. It records the durable effect identity locally before pushing, so
// Observe can classify a lost push response from the remote ref instead of
// repeating the merge. The native Git process is injected: this package owns
// the merge policy, the composition entry owns the operating-system executor.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/optimiware/kodama/internal/id"
)

// ErrMergeBaseMoved reports that the remote target ref no longer points at the
// request base. Nothing was recorded and nothing was pushed.
var ErrMergeBaseMoved = errors.New("merge base moved on the remote")

// GitCommand is one native Git invocation. Directory is the working tree and
// Args are passed verbatim; no shell ever sees them. Env carries explicitly
// composed child variables such as a scoped HTTP credential header.
type GitCommand struct {
	Directory string
	Args      []string
	Env       []string
}

// GitCommandResult is one native Git outcome. A non-zero ExitCode is a command
// result, not a transport failure.
type GitCommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// GitCommandExecutor runs one native Git command without a shell.
type GitCommandExecutor interface {
	Run(context.Context, GitCommand) (GitCommandResult, error)
}

// GitMergeEffectsConfig is the complete concrete Git effects configuration.
type GitMergeEffectsConfig struct {
	// RemoteBaseURL is the Git server/base URL. The repository of each
	// assignment is appended as one path segment.
	RemoteBaseURL string
	// StateDirectory keeps the durable per-operation effect record used by
	// Observe. It is required.
	StateDirectory string
	// WorkDirectory is where scratch clones live. A zero value uses
	// <StateDirectory>/work.
	WorkDirectory string
	// CredentialFile is the mounted service credential. It is read only for an
	// HTTP(S) remote, sent as an Authorization header to the configured Git
	// remote, and never placed in command arguments.
	CredentialFile string
	// Executor is the injected native Git boundary. It is required.
	Executor GitCommandExecutor
}

// GitMergeEffects is the concrete merge/rebase effect boundary.
type GitMergeEffects struct {
	remoteBaseURL string
	stateDir      string
	workDir       string
	credential    string
	executor      GitCommandExecutor
}

var _ MergeEffects = (*GitMergeEffects)(nil)

// gitEffectRecord is the durable evidence that this operation computed one
// result and was about to push it. Absence of a record proves the operation
// never reached the remote push.
type gitEffectRecord struct {
	Base      string `json:"base"`
	Candidate string `json:"candidate"`
	Result    string `json:"result"`
}

// NewGitMergeEffects validates the configuration and creates its directories.
// It starts no process and opens no remote connection.
func NewGitMergeEffects(config GitMergeEffectsConfig) (*GitMergeEffects, error) {
	base := strings.TrimSpace(config.RemoteBaseURL)
	if base == "" {
		return nil, fmt.Errorf("git merge effects remote base URL is empty")
	}
	if strings.ContainsAny(base, "\x00\n\r") {
		return nil, fmt.Errorf("git merge effects remote base URL contains a control character")
	}
	// The mounted credential file is the only credential source. A URL that
	// embeds userinfo would leak into diagnostics, so it is refused without
	// ever echoing the URL.
	if parsed, err := url.Parse(base); err == nil && parsed.User != nil {
		return nil, fmt.Errorf("git merge effects remote base URL must not embed credentials")
	}
	if config.Executor == nil {
		return nil, fmt.Errorf("git merge effects executor is required")
	}
	stateDir := strings.TrimSpace(config.StateDirectory)
	if stateDir == "" {
		return nil, fmt.Errorf("git merge effects state directory is empty")
	}
	stateDir = filepath.Clean(stateDir)
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create git merge effects state directory: %w", err)
	}
	workDir := strings.TrimSpace(config.WorkDirectory)
	if workDir == "" {
		workDir = filepath.Join(stateDir, "work")
	}
	workDir = filepath.Clean(workDir)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create git merge effects work directory: %w", err)
	}
	return &GitMergeEffects{
		remoteBaseURL: strings.TrimRight(base, "/"),
		stateDir:      stateDir,
		workDir:       workDir,
		credential:    strings.TrimSpace(config.CredentialFile),
		executor:      config.Executor,
	}, nil
}

// Execute performs at most one merge/rebase and push. A failure before the push
// is not_started because no remote mutation happened; a failed or lost push is
// classified by observing the remote ref, never by repeating the operation.
func (effects *GitMergeEffects) Execute(ctx context.Context, operation MergeOperation) (MergeObservation, error) {
	if err := effects.validateOperation(operation); err != nil {
		return MergeObservation{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	remoteURL := effects.repositoryURL(operation.Key.Repository)
	env, err := effects.remoteEnvironment(remoteURL)
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, err
	}
	workDir, err := os.MkdirTemp(effects.workDir, "merge-")
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, fmt.Errorf("create merge work tree: %w", err)
	}
	defer os.RemoveAll(workDir)

	if _, err := effects.require(ctx, workDir, nil, "init", "--quiet"); err != nil {
		return MergeObservation{State: EffectNotStarted}, err
	}
	if _, err := effects.require(ctx, workDir, nil, "remote", "add", "origin", remoteURL); err != nil {
		return MergeObservation{State: EffectNotStarted}, err
	}
	if _, err := effects.require(ctx, workDir, env, "fetch", "--no-tags", "--prune", "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		return MergeObservation{State: EffectNotStarted}, fmt.Errorf("fetch merge revisions: %w", err)
	}
	baseCommit, err := effects.resolveCommit(ctx, workDir, operation.Base)
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, fmt.Errorf("resolve merge base: %w", err)
	}
	// Refuse a stale request before computing or recording an effect. A
	// remote that already moved past the request base would only be rejected
	// by the push lease after the durable record was written, and that record
	// would then observe as uncertain forever. Nothing has started here.
	remoteCommit, err := effects.remoteRef(ctx, workDir, env, operation.Key.Ref)
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, fmt.Errorf("observe remote target: %w", err)
	}
	if remoteCommit != "" && remoteCommit != baseCommit {
		return MergeObservation{State: EffectNotStarted}, fmt.Errorf(
			"%w: %s is %s but the request base is %s",
			ErrMergeBaseMoved, operation.Key.Ref, remoteCommit, baseCommit)
	}
	candidateCommit, err := effects.resolveCommit(ctx, workDir, operation.Candidate)
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, fmt.Errorf("resolve merge candidate: %w", err)
	}
	candidateInBase, err := effects.isAncestor(ctx, workDir, candidateCommit, baseCommit)
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, err
	}
	baseInCandidate, err := effects.isAncestor(ctx, workDir, baseCommit, candidateCommit)
	if err != nil {
		return MergeObservation{State: EffectNotStarted}, err
	}
	resultCommit := ""
	switch {
	case candidateInBase:
		// The candidate is already contained in the base: there is nothing to
		// merge and the base is the settled result.
		resultCommit = baseCommit
	case baseInCandidate:
		// The base is an ancestor of the candidate: fast-forward.
		resultCommit = candidateCommit
	default:
		if _, err := effects.require(ctx, workDir, nil, "checkout", "--quiet", "--detach", candidateCommit); err != nil {
			return MergeObservation{State: EffectNotStarted}, err
		}
		if _, err := effects.require(ctx, workDir, nil, "rebase", baseCommit); err != nil {
			_, _ = effects.require(ctx, workDir, nil, "rebase", "--abort")
			return MergeObservation{State: EffectNotStarted}, fmt.Errorf("rebase merge candidate: %w", err)
		}
		rebased, err := effects.require(ctx, workDir, nil, "rev-parse", "HEAD")
		if err != nil {
			return MergeObservation{State: EffectNotStarted}, err
		}
		resultCommit = strings.TrimSpace(string(rebased.Stdout))
		if resultCommit == "" {
			return MergeObservation{State: EffectNotStarted}, fmt.Errorf("rebase merge produced no commit")
		}
	}

	record := gitEffectRecord{Base: baseCommit, Candidate: candidateCommit, Result: resultCommit}
	if err := effects.writeRecord(operation.OperationID, record); err != nil {
		return MergeObservation{State: EffectNotStarted}, err
	}
	expected := baseCommit
	if remoteCommit == "" {
		expected = ""
	}
	pushResult, pushErr := effects.command(ctx, workDir, env,
		"push", "--porcelain", "--force-with-lease="+operation.Key.Ref+":"+expected,
		"origin", resultCommit+":"+operation.Key.Ref)
	pushFailure := pushErr
	if pushFailure == nil && pushResult.ExitCode != 0 {
		pushFailure = fmt.Errorf("git push exited %d: %s", pushResult.ExitCode, commandDetail(pushResult))
	}
	if pushFailure == nil {
		observation, observeErr := effects.observeRecord(ctx, workDir, env, operation.Key.Ref, record)
		if observeErr != nil {
			return MergeObservation{State: EffectUncertain}, observeErr
		}
		if observation.State == EffectApplied {
			return observation, nil
		}
		return MergeObservation{State: EffectUncertain}, fmt.Errorf("push did not settle the remote ref")
	}
	observation, observeErr := effects.observeRecord(ctx, workDir, env, operation.Key.Ref, record)
	if observation.State == EffectApplied {
		return observation, nil
	}
	if observation.State == EffectNotStarted {
		return observation, pushFailure
	}
	if observeErr != nil {
		return MergeObservation{State: EffectUncertain}, fmt.Errorf("%w: %v", pushFailure, observeErr)
	}
	return MergeObservation{State: EffectUncertain}, pushFailure
}

// Observe reports what the remote ref settled as without executing. It never
// fabricates: a missing operation record proves no push was attempted; with a
// record only the exact result or the exact base is conclusive.
func (effects *GitMergeEffects) Observe(ctx context.Context, operation MergeOperation) (MergeObservation, error) {
	if err := effects.validateOperation(operation); err != nil {
		return MergeObservation{}, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	record, ok, err := effects.readRecord(operation.OperationID)
	if err != nil {
		return MergeObservation{State: EffectUncertain}, err
	}
	if !ok {
		return MergeObservation{State: EffectNotStarted}, nil
	}
	remoteURL := effects.repositoryURL(operation.Key.Repository)
	env, err := effects.remoteEnvironment(remoteURL)
	if err != nil {
		return MergeObservation{State: EffectUncertain}, err
	}
	workDir, err := os.MkdirTemp(effects.workDir, "observe-")
	if err != nil {
		return MergeObservation{State: EffectUncertain}, fmt.Errorf("create observation work tree: %w", err)
	}
	defer os.RemoveAll(workDir)
	if _, err := effects.require(ctx, workDir, nil, "init", "--quiet"); err != nil {
		return MergeObservation{State: EffectUncertain}, err
	}
	if _, err := effects.require(ctx, workDir, nil, "remote", "add", "origin", remoteURL); err != nil {
		return MergeObservation{State: EffectUncertain}, err
	}
	return effects.observeRecord(ctx, workDir, env, operation.Key.Ref, record)
}

// observeRecord classifies the remote ref against one durable operation record.
func (effects *GitMergeEffects) observeRecord(ctx context.Context, workDir string, env []string, ref string, record gitEffectRecord) (MergeObservation, error) {
	remoteCommit, err := effects.remoteRef(ctx, workDir, env, ref)
	if err != nil {
		return MergeObservation{State: EffectUncertain}, err
	}
	switch remoteCommit {
	case record.Result:
		return MergeObservation{
			State:     EffectApplied,
			Base:      record.Base,
			Candidate: record.Candidate,
			Output:    record.Result,
		}, nil
	case "", record.Base:
		return MergeObservation{State: EffectNotStarted}, nil
	default:
		return MergeObservation{State: EffectUncertain}, nil
	}
}

// validateOperation checks the durable effect identity and both revisions.
func (effects *GitMergeEffects) validateOperation(operation MergeOperation) error {
	if effects == nil || effects.executor == nil {
		return fmt.Errorf("git merge effects are not configured")
	}
	if err := operation.Key.Validate(); err != nil {
		return err
	}
	if _, err := id.ParseOperationID(string(operation.OperationID)); err != nil {
		return fmt.Errorf("merge operation identity %q is invalid: %w", operation.OperationID, err)
	}
	if strings.TrimSpace(operation.Base) == "" || strings.TrimSpace(operation.Candidate) == "" {
		return fmt.Errorf("merge operation needs a base and candidate")
	}
	return nil
}

// repositoryURL appends the assignment repository to the configured base URL.
func (effects *GitMergeEffects) repositoryURL(repository string) string {
	return effects.remoteBaseURL + "/" + repository
}

// remoteEnvironment composes the child variables for an HTTP(S) remote. The
// mounted credential becomes a scoped Authorization header in the environment,
// never in argv. A file or SSH remote needs no HTTP credential.
func (effects *GitMergeEffects) remoteEnvironment(remoteURL string) ([]string, error) {
	parsed, err := url.Parse(remoteURL)
	if err != nil {
		return nil, fmt.Errorf("parse merge remote URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, nil
	}
	if effects.credential == "" {
		return nil, fmt.Errorf("an HTTP merge remote requires a credential file")
	}
	data, err := os.ReadFile(effects.credential)
	if err != nil {
		return nil, fmt.Errorf("read merge credential: %w", err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, fmt.Errorf("merge credential file is empty")
	}
	basic := base64.StdEncoding.EncodeToString([]byte("kodama:" + token))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http." + remoteURL + ".extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}, nil
}

// resolveCommit maps an exact revision to its commit identity.
func (effects *GitMergeEffects) resolveCommit(ctx context.Context, workDir, revision string) (string, error) {
	result, err := effects.require(ctx, workDir, nil, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(string(result.Stdout))
	if commit == "" {
		return "", fmt.Errorf("revision %q resolved to no commit", revision)
	}
	return commit, nil
}

// isAncestor reports whether ancestor is reachable from descendant.
func (effects *GitMergeEffects) isAncestor(ctx context.Context, workDir, ancestor, descendant string) (bool, error) {
	result, err := effects.command(ctx, workDir, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base exited %d: %s", result.ExitCode, commandDetail(result))
	}
}

// remoteRef reads one exact remote ref. A missing ref is an empty commit.
func (effects *GitMergeEffects) remoteRef(ctx context.Context, workDir string, env []string, ref string) (string, error) {
	result, err := effects.require(ctx, workDir, env, "ls-remote", "--refs", "origin", ref)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(result.Stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref {
			return fields[0], nil
		}
	}
	return "", nil
}

// command runs one native command and returns its result. A transport failure
// is the only error; a non-zero Git exit is represented by ExitCode.
func (effects *GitMergeEffects) command(ctx context.Context, workDir string, env []string, args ...string) (GitCommandResult, error) {
	if len(args) == 0 {
		return GitCommandResult{}, fmt.Errorf("native git command has no arguments")
	}
	if err := ctx.Err(); err != nil {
		return GitCommandResult{}, err
	}
	result, err := effects.executor.Run(ctx, GitCommand{Directory: workDir, Args: args, Env: env})
	if err != nil {
		return result, fmt.Errorf("git %s: %w", args[0], err)
	}
	return result, nil
}

// require runs one command and refuses a non-zero exit.
func (effects *GitMergeEffects) require(ctx context.Context, workDir string, env []string, args ...string) (GitCommandResult, error) {
	result, err := effects.command(ctx, workDir, env, args...)
	if err != nil {
		return result, err
	}
	if result.ExitCode != 0 {
		return result, fmt.Errorf("git %s exited %d: %s", args[0], result.ExitCode, commandDetail(result))
	}
	return result, nil
}

// commandDetail bounds the command diagnostic so a remote failure cannot echo
// an unbounded or secret-bearing stream.
func commandDetail(result GitCommandResult) string {
	detail := strings.TrimSpace(string(result.Stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(result.Stdout))
	}
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 512 {
		detail = detail[:512]
	}
	return detail
}

// recordPath is the durable effect record of one operation identity.
func (effects *GitMergeEffects) recordPath(operationID id.OperationID) string {
	return filepath.Join(effects.stateDir, string(operationID)+".json")
}

// writeRecord publishes the computed result before the push, so a crash during
// the push leaves decisive observation evidence.
func (effects *GitMergeEffects) writeRecord(operationID id.OperationID, record gitEffectRecord) error {
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(effects.stateDir, ".merge-effect-*")
	if err != nil {
		return fmt.Errorf("create merge effect record: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, effects.recordPath(operationID)); err != nil {
		return fmt.Errorf("publish merge effect record: %w", err)
	}
	if err := syncDirectory(effects.stateDir); err != nil {
		return fmt.Errorf("make merge effect record durable: %w", err)
	}
	return nil
}

// readRecord reads one durable effect record. A missing record is (zero, false).
func (effects *GitMergeEffects) readRecord(operationID id.OperationID) (gitEffectRecord, bool, error) {
	data, err := os.ReadFile(effects.recordPath(operationID))
	if errors.Is(err, os.ErrNotExist) {
		return gitEffectRecord{}, false, nil
	}
	if err != nil {
		return gitEffectRecord{}, false, fmt.Errorf("read merge effect record: %w", err)
	}
	var record gitEffectRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return gitEffectRecord{}, false, fmt.Errorf("decode merge effect record: %w", err)
	}
	if record.Base == "" || record.Candidate == "" || record.Result == "" {
		return gitEffectRecord{}, false, fmt.Errorf("merge effect record is incomplete")
	}
	return record, true, nil
}
