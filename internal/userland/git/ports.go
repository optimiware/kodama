package git

import (
	"context"
	"time"

	"github.com/optimiware/kodama/api"
)

// Command is one native Git command. It never carries a credential.
type Command struct {
	Name string   `json:"name"`
	Args []string `json:"args,omitempty"`
}

// CommandResult is one native command outcome.
type CommandResult struct {
	ExitCode int    `json:"exit_code"`
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
}

// RefState is one observed repository ref.
type RefState struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// CommitRefs is one page of commits referenced by retained executions.
type CommitRefs struct {
	Commits []string     `json:"commits"`
	Next    api.PageInfo `json:"next"`
}

// CommandExecutor runs a native Git command without a shell.
type CommandExecutor interface {
	Run(ctx context.Context, command Command) (CommandResult, error)
}

// ReadAuthorizer decides local Git read access for one actor and repository.
// The smart-HTTP read path first verifies the actor with the public
// repository-level `git.read` operation, then asks this seam whether the actor
// may read the whole repository. Users with scoped RBAC roles and service
// accounts (including short-lived pipeline service accounts) are evaluated
// against the repository's local ordered rules. A read carries no ref argument
// because the decision is repository-level; there is no `git.ref.read`
// resource operation. Updates keep the `git.ref.update` verification/approval
// path.
type ReadAuthorizer interface {
	AuthorizeRead(ctx context.Context, repository string, identity Identity) (Decision, error)
}

// RefObserver observes the current commit of one repository ref.
type RefObserver interface {
	Observe(ctx context.Context, repository, ref string) (RefState, error)
}

// JournalStore persists the durable receive journal. Load is keyed by the stable
// source-event ID.
type JournalStore interface {
	Load(ctx context.Context, key string) (JournalEntry, bool, error)
	Record(ctx context.Context, entry JournalEntry) error
}

// VerificationClient is the public service verification/report boundary.
type VerificationClient interface {
	Verify(ctx context.Context, request api.VerifyRequest) (api.VerifyResponse, error)
	Report(ctx context.Context, request api.ReportRequest) (api.ReportResponse, error)
}

// RetentionQuery discovers commits referenced by retained executions through an
// authorized public query.
type RetentionQuery interface {
	ReferencedCommits(ctx context.Context, page api.Page) (CommitRefs, error)
}

// Clock is the injected time boundary.
type Clock interface {
	Now() time.Time
}
