package native

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/git"
)

// The trusted policy is the tenant's `admin` repository, not a mutable
// working-tree file. The accepted rules are the `conf/git.conf` blob of the
// commit the trusted ref resolves to, so only a commit that reached the
// repository through the authorized receive path can change them.
const (
	// AdminRepository is the tenant-relative repository that carries the
	// trusted access policy.
	AdminRepository = "admin"
	// CommittedPolicyRef is the trusted ref the policy is read from. Only an
	// accepted commit on this ref is policy; a work branch is never policy.
	CommittedPolicyRef = "refs/heads/main"
	// CommittedPolicyFile is the repository-relative policy path.
	CommittedPolicyFile = "conf/git.conf"
	// refStorageFiles is the only ref backend whose absent refs are visible to
	// a plain filesystem scan of `refs` and `packed-refs`.
	refStorageFiles = "files"
	// MaxCommittedPolicyBytes bounds one committed policy blob. The policy is
	// repository content, so one commit must never be able to make a policy
	// read consume unbounded memory.
	MaxCommittedPolicyBytes = 1 << 20
	// policyUnavailableMessage is the single public diagnostic for every
	// committed-policy dependency failure. It never carries a path, a ref
	// value or policy content.
	policyUnavailableMessage = "the tenant Git policy is unavailable"
)

// ErrCommittedPolicyTooLarge reports a trusted policy blob above
// MaxCommittedPolicyBytes. The blob is refused before it is parsed; the error
// carries no policy content.
var ErrCommittedPolicyTooLarge = errors.New("committed admin policy exceeds the size limit")

// CommittedPolicyConfig is the complete committed-policy source dependency set.
type CommittedPolicyConfig struct {
	// RepositoryPath is the resolved tenant-scoped `admin` repository path.
	RepositoryPath string
	// Executor resolves the ref and reads the committed blob through the same
	// command seam the rest of the service uses.
	Executor git.CommandExecutor
	// ResourceIDs are the service's static repository resource pins. A
	// proposed policy that contradicts one is refused.
	ResourceIDs map[string]string
}

// CommittedPolicy implements git.PolicySource by resolving the trusted admin
// ref to an immutable commit and parsing that commit's policy blob on every
// call. It never caches: an accepted admin push changes the next attempt, and
// a restart reconstructs the same rules from the repository alone.
type CommittedPolicy struct {
	repositoryPath string
	executor       git.CommandExecutor
	resourceIDs    map[string]string
}

var _ git.PolicySource = (*CommittedPolicy)(nil)

// NewCommittedPolicy validates the configuration and returns the source. It
// reads no ref: construction must not depend on the current policy state.
func NewCommittedPolicy(config CommittedPolicyConfig) (*CommittedPolicy, error) {
	repositoryPath := strings.TrimSpace(config.RepositoryPath)
	if repositoryPath == "" {
		return nil, fmt.Errorf("committed admin policy repository path is empty")
	}
	if config.Executor == nil {
		return nil, fmt.Errorf("committed admin policy command executor is missing")
	}
	return &CommittedPolicy{repositoryPath: repositoryPath, executor: config.Executor, resourceIDs: config.ResourceIDs}, nil
}

// Current resolves the accepted admin commit and parses its policy blob. There
// is no fallback: a missing admin repository, a missing trusted ref, an
// unreadable commit or blob, and a malformed blob are all dependency faults.
// A canceled context keeps its context error so cancellation is never reported
// as a policy dependency failure.
func (policy *CommittedPolicy) Current(ctx context.Context) (git.Config, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return git.Config{}, err
	}
	config, err := policy.current(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return git.Config{}, ctxErr
		}
		return git.Config{}, fault.Wrap(fault.DependencyUnavailable, policyUnavailableMessage, err)
	}
	return config, nil
}

// current resolves and parses the accepted policy, returning the private cause
// for Current to classify.
func (policy *CommittedPolicy) current(ctx context.Context) (git.Config, error) {
	// The trusted admin repository is not the repository being pushed: a
	// receive-pack quarantine environment on ctx belongs to that push and must
	// never redirect the admin object lookup.
	ctx = WithoutCommandEnvironment(ctx)
	commit, err := policy.acceptedCommit(ctx)
	if err != nil {
		return git.Config{}, err
	}
	return policy.committedConfig(ctx, commit)
}

// ValidateCommit reads and parses the policy a candidate commit would install.
// Pre-receive calls it with the receive-pack quarantine environment on the
// context, so the not-yet-accepted objects are readable and the check runs
// before receive-pack moves any ref. It never mutates the accepted policy.
func (policy *CommittedPolicy) ValidateCommit(ctx context.Context, commit string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validProposalCommit(commit) {
		return fmt.Errorf("proposed policy commit %q is not a full object id", commit)
	}
	data, err := policy.readCommittedPolicy(ctx, commit)
	if err != nil {
		return err
	}
	config, err := git.ParseConfig(data)
	if err != nil {
		return fmt.Errorf("proposed admin policy %s: %w", CommittedPolicyFile, err)
	}
	// The service refuses every request for a repository whose `option
	// resource` contradicts its static pin, so accepting such a policy for
	// the admin repository would also refuse the push that corrects it.
	for repository, pinned := range policy.resourceIDs {
		pinned = strings.TrimSpace(pinned)
		proposed, _ := config.Policy(strings.TrimSpace(repository))
		if pinned != "" && proposed.ResourceID != "" && proposed.ResourceID != pinned {
			return fmt.Errorf("proposed admin policy %s declares a resource for the %q repository that disagrees with its configured resource id", CommittedPolicyFile, repository)
		}
	}
	return requireAdminWriteOfPolicyRef(config)
}

// requireAdminWriteOfPolicyRef refuses a parsed policy that would leave the
// trusted policy ref unwritable by the admin role. The check is decided by the
// rule evaluator, not by a second grammar over the policy text: the
// proposed `admin` repository policy is asked whether the @admin human role may
// fast-forward the trusted ref. An ordered evaluator therefore sees a preceding
// deny, an @all wildcard, a principal list and a ref prefix exactly as a real
// push would, and a policy that omits the `admin` repository is refused too.
//
// A write rule alone is not enough. The real push path has a second gate:
// prepareLocked refuses a ref covered by `option approve.<ref>` with
// ErrApprovalRequired unless the identity is an execution carrying approval,
// and a human identity is never an execution, so a human can never push an
// approval-guarded ref. A proposed policy that guards the trusted policy ref
// would therefore lock every later human policy push out even though the
// evaluator allows the write. The guard is asked of the same evaluator call
// prepareLocked uses (RepositoryPolicy.ApprovalFor), so the two cannot drift:
// the exact ref form, the `refs/heads/` prefix form and any approver role cover
// the trusted ref, while a guard outside it does not.
//
// ValidateCommit has no pusher identity, so the identity is pinned to the role
// that governs the policy repository rather than taken from the request. The
// check never reads or writes the accepted policy and never echoes policy
// content in its refusal.
func requireAdminWriteOfPolicyRef(config git.Config) error {
	repository, ok := config.Policy(AdminRepository)
	if !ok {
		return fmt.Errorf("proposed admin policy %s does not declare the %q repository", CommittedPolicyFile, AdminRepository)
	}
	decision, err := repository.Authorize(git.AccessRequest{
		Identity: git.Identity{Roles: []string{"admin"}},
		Ref:      CommittedPolicyRef,
		Update:   git.UpdateFastForward,
	})
	if err != nil {
		return fmt.Errorf("proposed admin policy %s could not be evaluated for the admin role: %w", CommittedPolicyFile, err)
	}
	if decision != git.DecisionAllow {
		return fmt.Errorf("proposed admin policy %s does not allow the admin role to fast-forward %s of the %q repository", CommittedPolicyFile, CommittedPolicyRef, AdminRepository)
	}
	if _, guarded := repository.ApprovalFor(CommittedPolicyRef); guarded {
		return fmt.Errorf("proposed admin policy %s requires approval for %s of the %q repository, which no human push can satisfy", CommittedPolicyFile, CommittedPolicyRef, AdminRepository)
	}
	return nil
}

// policyGitArgs builds one policy command. The explicit --git-dir forbids Git
// discovery from walking up out of the trusted admin path (a non-repository
// admin directory must fail closed, never borrow an ancestor work tree), and
// --no-replace-objects keeps refs/replace from changing what a validated object
// id reads as.
func policyGitArgs(repositoryPath string, args ...string) []string {
	policyArgs := make([]string, 0, len(args)+2)
	policyArgs = append(policyArgs, "--git-dir="+repositoryPath, "--no-replace-objects")
	return append(policyArgs, args...)
}

// acceptedCommit resolves CommittedPolicyRef to the immutable commit object it
// names. Every state that cannot answer is a refusal: there is no policy-file
// or unprovisioned fallback.
func (policy *CommittedPolicy) acceptedCommit(ctx context.Context) (string, error) {
	info, err := os.Stat(policy.repositoryPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("tenant has no admin repository")
		}
		return "", fmt.Errorf("stat trusted admin policy repository %q: %w", policy.repositoryPath, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("trusted admin policy repository %q is not a directory", policy.repositoryPath)
	}
	// An existing admin path must itself be a repository. Without this check a
	// non-repository directory could be resolved through an ancestor work tree.
	result, err := policy.executor.Run(ctx, git.Command{
		Name: "git",
		Args: policyGitArgs(policy.repositoryPath, "rev-parse", "--git-dir"),
	})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 || strings.TrimSpace(string(result.Stderr)) != "" {
		return "", fmt.Errorf("trusted admin policy repository %q is not a readable repository", policy.repositoryPath)
	}
	result, err = policy.executor.Run(ctx, git.Command{
		Name: "git",
		Args: policyGitArgs(policy.repositoryPath,
			"for-each-ref", "--format=%(refname) %(objectname)", CommittedPolicyRef),
	})
	if err != nil {
		return "", err
	}
	// A broken ref makes for-each-ref warn on stderr and exit zero with no
	// stdout. Any diagnostic is a refusal: it is never evidence of absence.
	if strings.TrimSpace(string(result.Stderr)) != "" {
		return "", fmt.Errorf("resolve trusted admin policy ref %s: git for-each-ref reported a broken ref", CommittedPolicyRef)
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("resolve trusted admin policy ref %s: git for-each-ref exited %d", CommittedPolicyRef, result.ExitCode)
	}
	var commit string
	for _, line := range strings.Split(strings.TrimSpace(string(result.Stdout)), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return "", fmt.Errorf("trusted admin policy ref %s reported a malformed ref line", CommittedPolicyRef)
		}
		// for-each-ref matches by prefix, so refs/heads/main/x is not the
		// trusted ref. Requiring the exact refname closes that confusion.
		if fields[0] != CommittedPolicyRef {
			continue
		}
		if commit != "" {
			return "", fmt.Errorf("trusted admin policy ref %s resolved more than once", CommittedPolicyRef)
		}
		commit = fields[1]
	}
	if commit == "" {
		return "", fmt.Errorf("trusted admin policy ref %s is absent", CommittedPolicyRef)
	}
	if !validPolicyCommit(commit) {
		return "", fmt.Errorf("trusted admin policy ref %s does not resolve to a full object id", CommittedPolicyRef)
	}
	result, err = policy.executor.Run(ctx, git.Command{
		Name: "git",
		Args: policyGitArgs(policy.repositoryPath, "cat-file", "-t", commit),
	})
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", fmt.Errorf("trusted admin policy commit %s is unreadable", commit)
	}
	if kind := strings.TrimSpace(string(result.Stdout)); kind != "commit" {
		return "", fmt.Errorf("trusted admin policy ref %s does not point at a commit", CommittedPolicyRef)
	}
	return commit, nil
}

// committedConfig parses the accepted commit's policy blob.
func (policy *CommittedPolicy) committedConfig(ctx context.Context, commit string) (git.Config, error) {
	data, err := policy.readCommittedPolicy(ctx, commit)
	if err != nil {
		return git.Config{}, err
	}
	config, err := git.ParseConfig(data)
	if err != nil {
		return git.Config{}, fmt.Errorf("trusted admin policy %s in commit %s: %w", CommittedPolicyFile, commit, err)
	}
	return config, nil
}

// readCommittedPolicy reads exactly one policy blob from one immutable commit.
// The commit id is validated before it becomes a command argument, so a caller
// cannot smuggle another object spec into the read.
func (policy *CommittedPolicy) readCommittedPolicy(ctx context.Context, commit string) ([]byte, error) {
	if !validPolicyCommit(commit) {
		return nil, fmt.Errorf("policy commit %q is not a full object id", commit)
	}
	result, err := policy.executor.Run(ctx, git.Command{
		Name: "git",
		Args: policyGitArgs(policy.repositoryPath,
			"cat-file", "blob", commit+":"+CommittedPolicyFile),
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("trusted admin policy %s is missing or unreadable in commit %s", CommittedPolicyFile, commit)
	}
	if len(result.Stdout) > MaxCommittedPolicyBytes {
		return nil, fmt.Errorf("%w: %d bytes exceeds %d", ErrCommittedPolicyTooLarge, len(result.Stdout), MaxCommittedPolicyBytes)
	}
	return result.Stdout, nil
}

// validProposalCommit reports whether a proposed policy commit is one full
// SHA-1/SHA-256 object id. A proposal comes from the push, so it must be a
// concrete object, never a revision expression such as HEAD that could read the
// accepted policy instead of the candidate.
func validProposalCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'f':
		case character >= 'A' && character <= 'F':
		default:
			return false
		}
	}
	return true
}

// validPolicyCommit bounds the commit token before it becomes part of a
// `cat-file` object spec. It accepts a full object id and the opaque revision
// token a scripted command seam returns, but never a colon, whitespace or a
// control character that could shift the blob path.
func validPolicyCommit(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case character >= '0' && character <= '9':
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}
