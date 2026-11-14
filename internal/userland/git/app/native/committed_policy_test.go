package native

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/userland/git"
)

// policyRepositoryPath returns an existing directory that the scripted
// executor presents as a repository. Production refuses an existing
// non-repository path, so the unit seam starts from a present path with the
// readable files-backend layout a real bare repository has.
func policyRepositoryPath(t *testing.T) string {
	t.Helper()
	return policyLayoutPath(t, filepath.Join(t.TempDir(), "admin.git"))
}

// newTestCommittedPolicy builds the committed policy source over one repository
// path and one command executor.
func newTestCommittedPolicy(t *testing.T, repositoryPath string, executor git.CommandExecutor) *CommittedPolicy {
	t.Helper()
	policy, err := NewCommittedPolicy(CommittedPolicyConfig{
		RepositoryPath: repositoryPath,
		Executor:       executor,
	})
	if err != nil {
		t.Fatalf("NewCommittedPolicy: %v", err)
	}
	return policy
}

// policyLayoutPath creates the minimal readable files-backend layout of a bare
// repository with no trusted ref: `refs/heads`, `refs/tags`, `objects` and a
// valid symbolic HEAD. Tests add refs, packed-refs or faults on top of it.
func policyLayoutPath(t *testing.T, path string) string {
	t.Helper()
	for _, directory := range []string{"refs/heads", "refs/tags", "objects/info", "objects/pack"} {
		if err := os.MkdirAll(filepath.Join(path, filepath.FromSlash(directory)), 0o700); err != nil {
			t.Fatalf("create policy repository layout %q: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("write policy repository HEAD: %v", err)
	}
	return path
}

// committedPolicyExecutor is the existing command seam for the committed admin
// policy source. It scripts a trusted ref and the content of one commit without
// touching a working tree, so a source that reads an ordinary file cannot pass.
// It also records the per-command context environment so a policy read can be
// proved to drop a pushed repository's quarantine variables.
type committedPolicyExecutor struct {
	mu           sync.Mutex
	commands     []git.Command
	environments [][]string
	commit       string
	refLines     []string
	refStderr    string
	refExit      int
	contents     map[string]string
	readExit     int
	typeExit     int
	kind         string
	// repositoryExit and repositoryStderr script the repository check.
	repositoryExit   int
	repositoryStderr string
}

func (executor *committedPolicyExecutor) Run(ctx context.Context, command git.Command) (git.CommandResult, error) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commands = append(executor.commands, command)
	executor.environments = append(executor.environments, append([]string(nil), CommandEnvironment(ctx)...))
	joined := strings.Join(command.Args, " ")
	switch {
	case strings.Contains(joined, "for-each-ref"):
		lines := executor.refLines
		if lines == nil && executor.commit != "" {
			lines = []string{CommittedPolicyRef + " " + executor.commit}
		}
		stdout := ""
		if len(lines) > 0 {
			stdout = strings.Join(lines, "\n") + "\n"
		}
		return git.CommandResult{Stdout: []byte(stdout), Stderr: []byte(executor.refStderr), ExitCode: executor.refExit}, nil
	case strings.Contains(joined, "rev-parse"):
		return git.CommandResult{Stdout: []byte("/git\n"), Stderr: []byte(executor.repositoryStderr), ExitCode: executor.repositoryExit}, nil
	case strings.Contains(joined, "cat-file"):
		for _, argument := range command.Args {
			if argument == "-t" {
				kind := executor.kind
				if kind == "" {
					kind = "commit"
				}
				return git.CommandResult{Stdout: []byte(kind + "\n"), ExitCode: executor.typeExit}, nil
			}
			if strings.Contains(argument, ":") {
				return executor.read(argument)
			}
		}
	}
	return git.CommandResult{}, nil
}

func (executor *committedPolicyExecutor) read(spec string) (git.CommandResult, error) {
	if executor.readExit != 0 {
		return git.CommandResult{ExitCode: executor.readExit}, nil
	}
	content, ok := executor.contents[spec]
	if !ok {
		return git.CommandResult{ExitCode: 128}, nil
	}
	return git.CommandResult{Stdout: []byte(content)}, nil
}

func (executor *committedPolicyExecutor) set(commit string, contents map[string]string) {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.commit = commit
	executor.contents = contents
}

func (executor *committedPolicyExecutor) readSpecs() []string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	var specs []string
	for _, command := range executor.commands {
		for _, argument := range command.Args {
			if strings.Contains(argument, ":conf/git.conf") {
				specs = append(specs, argument)
			}
		}
	}
	return specs
}

func (executor *committedPolicyExecutor) recordedCommands() []git.Command {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	return append([]git.Command(nil), executor.commands...)
}

func (executor *committedPolicyExecutor) recordedEnvironments() [][]string {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	environments := make([][]string, len(executor.environments))
	copy(environments, executor.environments)
	return environments
}

func TestCommittedPolicyReadsTheAcceptedCommitOnly(t *testing.T) {
	executor := &committedPolicyExecutor{}
	executor.set("commit-one", map[string]string{
		"commit-one:conf/git.conf": "repo repo\nRW = @all\n",
	})
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	config, err := policy.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	repository, ok := config.Policy("repo")
	if !ok || len(repository.Rules) != 1 || repository.Rules[0].Permission != git.PermissionWrite {
		t.Fatalf("policy = %+v; want the committed write rule", repository)
	}
	if specs := executor.readSpecs(); len(specs) == 0 || specs[0] != "commit-one:conf/git.conf" {
		t.Fatalf("read specs = %v; want commit-one:conf/git.conf", specs)
	}

	// A new accepted commit is used by the next attempt; nothing is cached.
	executor.set("commit-two", map[string]string{
		"commit-two:conf/git.conf": "repo repo\nR = @all\n",
	})
	config, err = policy.Current(context.Background())
	if err != nil {
		t.Fatalf("Current after accepted commit: %v", err)
	}
	repository, _ = config.Policy("repo")
	if len(repository.Rules) != 1 || repository.Rules[0].Permission != git.PermissionRead {
		t.Fatalf("rotated policy = %+v; want the new read rule", repository)
	}
	specs := executor.readSpecs()
	if len(specs) < 2 || specs[len(specs)-1] != "commit-two:conf/git.conf" {
		t.Fatalf("read specs = %v; want the second commit read", specs)
	}
}

// requirePolicyUnavailableFault proves the committed-policy dependency class
// and that the public fault message is generic: it must never name the
// repository path or echo policy content.
func requirePolicyUnavailableFault(t *testing.T, err error, repositoryPath string) {
	t.Helper()
	if err == nil {
		t.Fatal("Current accepted an unavailable committed policy")
	}
	code, ok := fault.CodeOf(err)
	if !ok || code != fault.CodeDependencyUnavailable {
		t.Fatalf("fault code = %q (%v); want %q", code, ok, fault.CodeDependencyUnavailable)
	}
	message, ok := fault.MessageOf(err)
	if !ok {
		t.Fatalf("error %v is not a public fault", err)
	}
	if repositoryPath != "" && strings.Contains(message, repositoryPath) {
		t.Fatalf("fault message %q leaks the repository path", message)
	}
	if strings.Contains(message, "RW") || strings.Contains(message, "repo ") {
		t.Fatalf("fault message %q leaks policy content", message)
	}
}

func TestCommittedPolicyFailsClosedWithoutAnAcceptedRef(t *testing.T) {
	repositoryPath := policyRepositoryPath(t)
	executor := &committedPolicyExecutor{}
	executor.set("", nil)
	policy := newTestCommittedPolicy(t, repositoryPath, executor)
	_, err := policy.Current(context.Background())
	requirePolicyUnavailableFault(t, err, repositoryPath)
	if specs := executor.readSpecs(); len(specs) != 0 {
		t.Fatalf("read specs = %v; want no policy read", specs)
	}
}

// TestCommittedPolicyMissingAdminRepositoryFailsClosed pins that an absent
// admin repository is a dependency fault and never a policy-file fallback. The
// real command would fail on a missing path, so a source that dropped the
// absence check would also fail; it must fail with the dependency class before
// running git.
func TestCommittedPolicyMissingAdminRepositoryFailsClosed(t *testing.T) {
	repositoryPath := filepath.Join(t.TempDir(), "absent-admin")
	executor := &committedPolicyExecutor{
		repositoryExit:   128,
		repositoryStderr: "fatal: not a git repository\n",
	}
	policy := newTestCommittedPolicy(t, repositoryPath, executor)
	_, err := policy.Current(context.Background())
	requirePolicyUnavailableFault(t, err, repositoryPath)
	if commands := executor.recordedCommands(); len(commands) != 0 {
		t.Fatalf("commands = %v; want none for an absent repository", commands)
	}
}

func TestCommittedPolicyRefusesMalformedOrUnreadableCommit(t *testing.T) {
	executor := &committedPolicyExecutor{}
	executor.set("commit-one", map[string]string{
		"commit-one:conf/git.conf": "repo repo\nRW = @wizard\n",
	})
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	if _, err := policy.Current(context.Background()); err == nil {
		t.Fatal("Current accepted a malformed committed policy")
	}

	executor.set("commit-one", nil)
	executor.readExit = 128
	_, err := policy.Current(context.Background())
	requirePolicyUnavailableFault(t, err, "")
}

func TestCommittedPolicyPropagatesCancellation(t *testing.T) {
	executor := &committedPolicyExecutor{}
	executor.set("commit-one", map[string]string{
		"commit-one:conf/git.conf": "repo repo\nRW = @all\n",
	})
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := policy.Current(ctx); err == nil {
		t.Fatal("Current accepted a canceled context")
	}
}

// TestCommittedPolicyRefusesBrokenTrustedRefs pins the fail-closed handling of
// every ref state in which git can exit zero with no usable commit line. With
// no bootstrap fallback, a broken ref is a dependency fault.
func TestCommittedPolicyRefusesBrokenTrustedRefs(t *testing.T) {
	for _, broken := range []struct {
		name    string
		lines   []string
		stderr  string
		refExit int
	}{
		{name: "empty ref file", stderr: "warning: ignoring broken ref refs/heads/main\n"},
		{name: "garbage ref file", stderr: "warning: ignoring broken ref refs/heads/main\n"},
		{name: "stderr warning with a commit", lines: []string{CommittedPolicyRef + " commit-one"}, stderr: "warning: unrelated diagnostic\n"},
		{name: "ref command failure", refExit: 128},
	} {
		t.Run(broken.name, func(t *testing.T) {
			repositoryPath := policyRepositoryPath(t)
			executor := &committedPolicyExecutor{
				refLines:  broken.lines,
				refStderr: broken.stderr,
				refExit:   broken.refExit,
				contents:  map[string]string{"commit-one:conf/git.conf": "repo repo\nRW = @all\n"},
			}
			policy := newTestCommittedPolicy(t, repositoryPath, executor)
			_, err := policy.Current(context.Background())
			requirePolicyUnavailableFault(t, err, repositoryPath)
		})
	}
}

func TestCommittedPolicyPrefixRefIsNotTheTrustedRef(t *testing.T) {
	repositoryPath := policyRepositoryPath(t)
	executor := &committedPolicyExecutor{
		refLines: []string{"refs/heads/main/x commit-prefix"},
		contents: map[string]string{
			"commit-prefix:conf/git.conf": "repo repo\nR = @all\n",
		},
	}
	policy := newTestCommittedPolicy(t, repositoryPath, executor)
	_, err := policy.Current(context.Background())
	requirePolicyUnavailableFault(t, err, repositoryPath)
	if specs := executor.readSpecs(); len(specs) != 0 {
		t.Fatalf("read specs = %v; want no read of the prefix ref's commit", specs)
	}
}

// TestCommittedPolicyRefusesNonRepositoryAdminDirectory pins that an existing
// admin path must itself be a repository; it is never resolved through git
// discovery.
func TestCommittedPolicyRefusesNonRepositoryAdminDirectory(t *testing.T) {
	repositoryPath := policyRepositoryPath(t)
	executor := &committedPolicyExecutor{
		repositoryExit:   128,
		repositoryStderr: "fatal: not a git repository\n",
	}
	policy := newTestCommittedPolicy(t, repositoryPath, executor)
	_, err := policy.Current(context.Background())
	requirePolicyUnavailableFault(t, err, repositoryPath)
}

// TestCommittedPolicyRefusesFailingRepository kills a source that treats any
// non-zero ref exit as absence: an existing admin repository that cannot
// answer must fail closed.
func TestCommittedPolicyRefusesFailingRepository(t *testing.T) {
	for _, exit := range []int{1, 128} {
		repositoryPath := policyRepositoryPath(t)
		executor := &committedPolicyExecutor{refExit: exit}
		policy := newTestCommittedPolicy(t, repositoryPath, executor)
		_, err := policy.Current(context.Background())
		requirePolicyUnavailableFault(t, err, repositoryPath)
	}
}

func TestCommittedPolicyRefusesUnreadableCommitType(t *testing.T) {
	executor := &committedPolicyExecutor{
		commit:   "commit-one",
		typeExit: 128,
		contents: map[string]string{"commit-one:conf/git.conf": "repo repo\nRW = @all\n"},
	}
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	if _, err := policy.Current(context.Background()); err == nil {
		t.Fatal("Current accepted a trusted commit whose object type could not be read")
	}
}

// TestCommittedPolicyReadDropsQuarantineEnvironment proves the accepted policy
// read of the admin repository never inherits the pushed repository's
// receive-pack quarantine object environment.
func TestCommittedPolicyReadDropsQuarantineEnvironment(t *testing.T) {
	executor := &committedPolicyExecutor{}
	executor.set("commit-one", map[string]string{
		"commit-one:conf/git.conf": "repo repo\nRW = @all\n",
	})
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	quarantine := []string{
		EnvObjectDirectory + "=/repos/shop/web/objects/incoming",
		EnvAlternateObjectDirectories + "=/repos/shop/web/objects",
		EnvQuarantinePath + "=/repos/shop/web/objects/incoming",
	}
	ctx := WithCommandEnvironment(context.Background(), quarantine)
	if _, err := policy.Current(ctx); err != nil {
		t.Fatalf("Current: %v", err)
	}
	for index, environment := range executor.recordedEnvironments() {
		if len(environment) != 0 {
			t.Fatalf("policy command %d saw quarantine environment %v; want none", index, environment)
		}
	}
}

// TestCommittedPolicyCommandsUseExplicitGitDirAndNoReplaceObjects pins the two
// hardening flags on every policy read.
func TestCommittedPolicyCommandsUseExplicitGitDirAndNoReplaceObjects(t *testing.T) {
	executor := &committedPolicyExecutor{}
	executor.set("commit-one", map[string]string{
		"commit-one:conf/git.conf": "repo repo\nRW = @all\n",
	})
	repositoryPath := policyRepositoryPath(t)
	policy := newTestCommittedPolicy(t, repositoryPath, executor)
	if _, err := policy.Current(context.Background()); err != nil {
		t.Fatalf("Current: %v", err)
	}
	commands := executor.recordedCommands()
	if len(commands) == 0 {
		t.Fatal("policy source ran no commands")
	}
	explicitGitDir := "--git-dir=" + repositoryPath
	for _, command := range commands {
		if !slices.Contains(command.Args, explicitGitDir) {
			t.Fatalf("policy command %v lacks %q", command.Args, explicitGitDir)
		}
		if !slices.Contains(command.Args, "--no-replace-objects") {
			t.Fatalf("policy command %v lacks --no-replace-objects", command.Args)
		}
		if slices.Contains(command.Args, "-C") {
			t.Fatalf("policy command %v uses -C discovery", command.Args)
		}
	}
}

// TestCommittedPolicyRefusesAStatErrorOtherThanNotExist kills the mutant that
// treats any stat failure as an absent repository: a path whose parent is a
// file fails with ENOTDIR, which is not "not found".
func TestCommittedPolicyRefusesAStatErrorOtherThanNotExist(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("write parent file: %v", err)
	}
	repositoryPath := filepath.Join(parent, "admin")
	policy := newTestCommittedPolicy(t, repositoryPath, &committedPolicyExecutor{})
	_, err := policy.Current(context.Background())
	requirePolicyUnavailableFault(t, err, repositoryPath)
}

// TestCommittedPolicyRefusesMalformedOrDuplicateRefLines pins that a
// for-each-ref answer with a line that is not exactly one ref, or that repeats
// the trusted ref, is a refusal even when git reports no diagnostic.
func TestCommittedPolicyRefusesMalformedOrDuplicateRefLines(t *testing.T) {
	t.Run("duplicate trusted ref", func(t *testing.T) {
		executor := &committedPolicyExecutor{
			refLines: []string{CommittedPolicyRef + " commit-one", CommittedPolicyRef + " commit-two"},
			contents: map[string]string{
				"commit-one:conf/git.conf": "repo repo\nR = @all\n",
				"commit-two:conf/git.conf": "repo repo\nRW = @all\n",
			},
		}
		policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
		if _, err := policy.Current(context.Background()); err == nil {
			t.Fatal("Current accepted a trusted ref reported more than once")
		}
	})
	t.Run("malformed ref line", func(t *testing.T) {
		executor := &committedPolicyExecutor{refLines: []string{CommittedPolicyRef}}
		policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
		if _, err := policy.Current(context.Background()); err == nil {
			t.Fatal("Current accepted a malformed ref line")
		}
	})
}

// TestCommittedPolicyRefusesAFailedCommitTypeCheck pins that the object-type
// check is decided by git's exit status, not by whatever it printed: a failed
// check that still prints "commit" must refuse.
func TestCommittedPolicyRefusesAFailedCommitTypeCheck(t *testing.T) {
	executor := &committedPolicyExecutor{
		commit:   "commit-one",
		typeExit: 128,
		kind:     "commit",
		contents: map[string]string{"commit-one:conf/git.conf": "repo repo\nRW = @all\n"},
	}
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	if _, err := policy.Current(context.Background()); err == nil {
		t.Fatal("Current accepted a trusted ref whose object type check failed")
	}
}

// TestCommittedPolicyValidateCommitRefusesWithoutAnAdminWriteRule pins decision
// 31's self-governance invariant: the policy a proposal would install must let
// the admin role write the trusted policy ref of the `admin` repository. The
// check is decided by the rule evaluator (ordered rules, first match,
// @all wildcard, role principals), never by a second grammar.
func TestCommittedPolicyValidateCommitRefusesWithoutAnAdminWriteRule(t *testing.T) {
	candidate := strings.Repeat("2", 40)
	tests := []struct {
		name   string
		policy string
	}{
		{
			name:   "no admin repository at all",
			policy: "repo shop/web\nRW = @all\n",
		},
		{
			name:   "admin rule is read-only",
			policy: "repo admin\nR = @admin\n",
		},
		{
			name:   "admin write granted to another human role",
			policy: "repo admin\nRW = @engineer\n",
		},
		{
			name:   "admin write granted only to execution",
			policy: "repo admin\nRW = @execution\n",
		},
		{
			name:   "admin write granted only to an explicit service",
			policy: "repo admin\nRW = service-i03c\n",
		},
		{
			name:   "admin write limited to another ref",
			policy: "repo admin\nRW refs/heads/work/ = @admin\n",
		},
		{
			name:   "an earlier deny matches the admin identity",
			policy: "repo admin\n- = @admin\nRW = @all\n",
		},
		{
			name:   "admin resource contradicts its static pin",
			policy: "repo admin\noption resource = res-other\nRW = @admin\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &committedPolicyExecutor{}
			executor.set(candidate, map[string]string{
				candidate + ":conf/git.conf": test.policy,
			})
			policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
			policy.resourceIDs = map[string]string{AdminRepository: "res-admin"}
			if err := policy.ValidateCommit(context.Background(), candidate); err == nil {
				t.Fatalf("ValidateCommit accepted a proposed policy that leaves the admin policy ref unwritable by @admin: %q", test.policy)
			}
		})
	}
}

// TestCommittedPolicyValidateCommitAcceptsEveryEvaluatorPathToAdminWrite covers
// the positive side of the same invariant: the first-commit shape, the @all
// wildcard, a principal list that includes @admin and a ref prefix covering the
// trusted policy ref are all accepted because the evaluator allows an @admin
// write of refs/heads/main.
func TestCommittedPolicyValidateCommitAcceptsEveryEvaluatorPathToAdminWrite(t *testing.T) {
	candidate := strings.Repeat("2", 40)
	tests := []struct {
		name   string
		policy string
	}{
		{
			name:   "first-commit shape",
			policy: "repo admin\nRW = @admin\n",
		},
		{
			name:   "wildcard principal",
			policy: "repo admin\nRW = @all\n",
		},
		{
			name:   "principal list includes admin",
			policy: "repo admin\nRW = @engineer @admin @reviewer\n",
		},
		{
			name:   "ref prefix covers the trusted ref",
			policy: "repo admin\nRW refs/heads/ = @admin\n",
		},
		{
			name:   "exact trusted ref",
			policy: "repo admin\nRW refs/heads/main = @admin\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &committedPolicyExecutor{}
			executor.set(candidate, map[string]string{
				candidate + ":conf/git.conf": test.policy,
			})
			policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
			if err := policy.ValidateCommit(context.Background(), candidate); err != nil {
				t.Fatalf("ValidateCommit refused a proposed policy that grants @admin write on the policy ref: %v (policy %q)", err, test.policy)
			}
		})
	}
}

// TestCommittedPolicyValidateCommitRefusesAnApprovalGuardOnThePolicyRef pins
// the second gate of the real push path: a ref covered by
// `option approve.<ref>` is refused with ErrApprovalRequired unless the
// identity is an execution carrying approval, and a human identity is never an
// execution. A proposed policy that guards the trusted policy ref therefore
// locks every later human policy push out even when it grants @admin write, so
// it must be refused. The guard is decided by the same evaluator call
// prepareLocked uses (RepositoryPolicy.ApprovalFor), not a second grammar: the
// exact ref form, the `refs/heads/` prefix form and any approver role all
// cover refs/heads/main.
func TestCommittedPolicyValidateCommitRefusesAnApprovalGuardOnThePolicyRef(t *testing.T) {
	candidate := strings.Repeat("2", 40)
	tests := []struct {
		name string
		// approval is the approver value; the refusal must not echo it.
		approval string
		policy   string
	}{
		{
			name:     "exact policy ref guard",
			approval: "reviewer",
			policy:   "repo admin\n    RW = @admin\n    option approve.refs/heads/main = reviewer\n",
		},
		{
			name:     "policy ref prefix guard",
			approval: "reviewer",
			policy:   "repo admin\n    RW = @admin\n    option approve.refs/heads/ = reviewer\n",
		},
		{
			name:     "another approver value",
			approval: "operator",
			policy:   "repo admin\n    RW = @admin\n    option approve.refs/heads/main = operator\n",
		},
		{
			name:     "guard combined with an allowed write rule",
			approval: "viewer",
			policy:   "repo admin\n    RW refs/heads/ = @admin\n    option approve.refs/heads/ = viewer\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &committedPolicyExecutor{}
			executor.set(candidate, map[string]string{
				candidate + ":conf/git.conf": test.policy,
			})
			policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
			err := policy.ValidateCommit(context.Background(), candidate)
			if err == nil {
				t.Fatalf("ValidateCommit accepted a proposed policy that approval-guards %s of the %q repository: %q", CommittedPolicyRef, AdminRepository, test.policy)
			}
			// The refusal is sanitized: it may name the ref and the
			// repository, never the approver value or the policy text.
			if strings.Contains(err.Error(), test.approval) {
				t.Fatalf("refusal echoes the approver value %q: %v", test.approval, err)
			}
			if strings.Contains(err.Error(), "option approve") {
				t.Fatalf("refusal echoes the policy option text: %v", err)
			}
		})
	}
}

// TestCommittedPolicyValidateCommitAcceptsAnApprovalGuardOnAnotherRef is the
// other half of the approval invariant: a guard outside the trusted policy ref
// does not lock the policy ref, so it stays accepted. A prefix guard covers
// only its own segment; a ref that merely starts with the same characters is
// not that ref.
func TestCommittedPolicyValidateCommitAcceptsAnApprovalGuardOnAnotherRef(t *testing.T) {
	candidate := strings.Repeat("2", 40)
	tests := []struct {
		name   string
		policy string
	}{
		{
			name:   "guard on another ref prefix",
			policy: "repo admin\n    RW = @admin\n    option approve.refs/heads/work/ = reviewer\n",
		},
		{
			name:   "guard on one other exact ref",
			policy: "repo admin\n    RW = @admin\n    option approve.refs/heads/release/2026 = reviewer\n",
		},
		{
			name:   "guard on a ref sharing the policy ref prefix",
			policy: "repo admin\n    RW = @admin\n    option approve.refs/heads/mainline = reviewer\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor := &committedPolicyExecutor{}
			executor.set(candidate, map[string]string{
				candidate + ":conf/git.conf": test.policy,
			})
			policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
			if err := policy.ValidateCommit(context.Background(), candidate); err != nil {
				t.Fatalf("ValidateCommit refused a guard that does not cover %s: %v (policy %q)", CommittedPolicyRef, err, test.policy)
			}
		})
	}
}

// TestCommittedPolicyValidateCommitNeverChangesTheAcceptedPolicy updates the
// trusted ref's accepted policy and then validates an invalid candidate: the
// refusal must not disturb the accepted commit, so the next authorization
// attempt still uses the old rules.
func TestCommittedPolicyValidateCommitNeverChangesTheAcceptedPolicy(t *testing.T) {
	executor := &committedPolicyExecutor{}
	executor.set("commit-accepted", map[string]string{
		"commit-accepted:conf/git.conf": "repo admin\nRW = @admin\n",
	})
	policy := newTestCommittedPolicy(t, policyRepositoryPath(t), executor)
	config, err := policy.Current(context.Background())
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if config.PolicyNames() == nil || len(config.PolicyNames()) != 1 || config.PolicyNames()[0] != AdminRepository {
		t.Fatalf("accepted policy names = %v; want [%s]", config.PolicyNames(), AdminRepository)
	}

	candidate := strings.Repeat("3", 40)
	executor.mu.Lock()
	executor.contents[candidate+":conf/git.conf"] = "repo admin\nR = @admin\n"
	executor.mu.Unlock()
	if err := policy.ValidateCommit(context.Background(), candidate); err == nil {
		t.Fatal("ValidateCommit accepted a proposed policy with no admin write rule")
	}

	config, err = policy.Current(context.Background())
	if err != nil {
		t.Fatalf("Current after refused candidate: %v", err)
	}
	repository, ok := config.Policy(AdminRepository)
	if !ok || len(repository.Rules) != 1 || repository.Rules[0].Permission != git.PermissionWrite {
		t.Fatalf("accepted policy after refused candidate = %+v; want the original admin write rule", repository)
	}
}

// TestCommittedPolicyRefusesOversizedPolicyBlob pins the policy blob bound: a
// trusted blob larger than the bound is refused with a named error before it
// is parsed, on both the accepted-policy and the pre-receive validation paths.
// The policy is repository content, so an unbounded read would let one commit
// consume arbitrary memory.
func TestCommittedPolicyRefusesOversizedPolicyBlob(t *testing.T) {
	oversized := strings.Repeat("a", MaxCommittedPolicyBytes+1)
	commit := strings.Repeat("a", 40)
	executor := &committedPolicyExecutor{}
	executor.set(commit, map[string]string{commit + ":conf/git.conf": oversized})
	repositoryPath := policyRepositoryPath(t)
	policy := newTestCommittedPolicy(t, repositoryPath, executor)

	_, err := policy.Current(context.Background())
	if !errors.Is(err, ErrCommittedPolicyTooLarge) {
		t.Fatalf("Current error = %v; want ErrCommittedPolicyTooLarge", err)
	}
	code, ok := fault.CodeOf(err)
	if !ok || code != fault.CodeDependencyUnavailable {
		t.Fatalf("fault code = %q (%v); want %q", code, ok, fault.CodeDependencyUnavailable)
	}

	err = policy.ValidateCommit(context.Background(), commit)
	if !errors.Is(err, ErrCommittedPolicyTooLarge) {
		t.Fatalf("ValidateCommit error = %v; want ErrCommittedPolicyTooLarge", err)
	}
}
