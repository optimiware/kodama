// Package dispatch turns an applied push into indexed pipelines and runs. It
// consumes git.refs_updated from the event bus (at least once) and, for each
// event:
//
//   - when the push moved the repository's default branch, reads .kodama/*.yaml
//     at the exact pushed commit through the tenant's worker, parses and validates
//     each file strictly, and indexes it as the pipeline named by the file (a
//     refused file leaves the previous definition in place and is recorded; a
//     file that is gone disables its pipeline);
//   - matches each applied, non-delete update against the triggers.git of every
//     pipeline of the repository and admits at most one run per (pipeline, push
//     event, ref), pinning the update's new commit in runs.sources;
//   - admits nothing for a push made by a run (originRunId), which never
//     retriggers.
//
// Indexing and admission for one event commit in one transaction
// (store.DispatchBackend), and the event's identity makes a redelivery a no-op.
package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path"
	"strings"

	eventsv1 "github.com/optimiware/kodama/contracts/events/v1"
	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
	"github.com/optimiware/kodama/internal/shell/bus"
	"github.com/optimiware/kodama/internal/shell/store"
)

// Consumer is the durable consumer name and the subject filter of the dispatcher.
const (
	Consumer = "kernel-git-dispatch"
	Filter   = bus.SubjectRoot + ".*.*." + eventsv1.TypeGitRefsUpdated
)

// Refusal codes of a file that cannot be indexed, besides the stable diagnostic
// codes of the pipeline package.
const (
	CodeNameMismatch  = "name_mismatch"
	CodeDuplicateName = "duplicate_name"
	CodeFilesRefused  = "files_refused"
)

// Store is the persistence the dispatcher needs.
type Store interface {
	GetRepository(ctx context.Context, tenant domain.TenantID, id string) (store.Repository, error)
	store.DispatchBackend
}

// Files reads the definitions of an exact commit from a tenant's worker.
type Files interface {
	ReadFiles(ctx context.Context, tenant domain.TenantID, req gitv1.ReadFilesRequest) (gitv1.ReadFilesResponse, error)
}

// Config configures a Dispatcher.
type Config struct {
	Store Store
	Files Files
	// Wake is called after a dispatch admitted a run, so the kernel scheduler
	// picks it up without waiting for its poll.
	Wake   func()
	Logger *slog.Logger
}

// Dispatcher indexes pipelines and admits runs for applied pushes.
type Dispatcher struct {
	cfg Config
}

// New validates the configuration.
func New(cfg Config) (*Dispatcher, error) {
	if cfg.Store == nil || cfg.Files == nil {
		return nil, errors.New("dispatch: a store and a file reader are required")
	}
	if cfg.Wake == nil {
		cfg.Wake = func() {}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	return &Dispatcher{cfg: cfg}, nil
}

// Subscribe attaches the dispatcher to the bus.
func (d *Dispatcher) Subscribe(ctx context.Context, b bus.Subscriber) (func(), error) {
	return b.Subscribe(ctx, Consumer, Filter, d.Handle)
}

// TriggerKey is the runs.trigger_key of the run a push event starts for a ref: the
// pipeline is the other half of the unique key.
func TriggerKey(eventID, ref string) string {
	return eventsv1.TypeGitRefsUpdated + ":" + eventID + ":" + ref
}

// Handle processes one delivery. A nil error settles it; an error leaves it to be
// delivered again (the worker could not be reached, the database failed). An
// event that redelivery cannot fix (a malformed payload, an unknown repository)
// is logged and settled.
func (d *Dispatcher) Handle(ctx context.Context, m bus.Message) error {
	if m.Type != eventsv1.TypeGitRefsUpdated {
		return nil
	}
	log := d.cfg.Logger.With("event", m.ID, "tenant", m.TenantID)
	ev, err := eventsv1.DecodeGitRefsUpdated(m.Data)
	if err != nil {
		log.Error("git.refs_updated is malformed and was dropped", "error", err)
		return nil
	}
	// A completed dispatch is a replay: settle it before anything is read from
	// the worker, so a replay does not depend on the worker being up. The
	// transactional check in DispatchPush stays the authority.
	if done, err := d.cfg.Store.Dispatched(ctx, m.TenantID, m.ID); err != nil {
		return err
	} else if done {
		log.Debug("git.refs_updated was already dispatched")
		return nil
	}
	repo, err := d.cfg.Store.GetRepository(ctx, m.TenantID, ev.RepositoryID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		log.Error("git.refs_updated names an unknown repository and was dropped", "repository", ev.RepositoryID)
		return nil
	case err != nil:
		return err
	case repo.ProjectID == "" || repo.ProjectID != m.ProjectID:
		log.Info("git.refs_updated is not for a project repository; nothing to dispatch", "repository", repo.ID)
		return nil
	case repo.Status == store.RepoArchived:
		log.Info("the repository is archived; nothing to dispatch", "repository", repo.ID)
		return nil
	}

	in := store.PushDispatch{TenantID: m.TenantID, RepositoryID: repo.ID, EventID: m.ID, ReceiveID: ev.ReceiveID}
	if ev.JournalPosition != nil {
		in.JournalPosition = *ev.JournalPosition
	}
	if commit := defaultBranchCommit(repo, ev); commit != "" {
		if in.Index, err = d.readIndex(ctx, m.TenantID, repo, commit); err != nil {
			return err
		}
	}
	if ev.OriginRunID == nil {
		in.Plan = Plan(m.ID, repo.ID, ev.Updates, log)
	} else {
		log.Info("the push was made by a run; it starts nothing", "run", *ev.OriginRunID)
	}
	res, err := d.cfg.Store.DispatchPush(ctx, in)
	if err != nil {
		return err
	}
	switch {
	case res.Replayed:
		log.Debug("git.refs_updated was already dispatched")
		return nil
	case res.Archived:
		log.Info("the repository was archived while its files were read; nothing was indexed or admitted", "repository", repo.ID)
		return nil
	}
	log.Info("git.refs_updated dispatched", "repository", repo.ID, "indexed", res.Indexed, "disabled", res.Disabled,
		"refused", res.Refused, "stale", res.Stale, "runs", res.Runs)
	if len(res.Runs) > 0 {
		d.cfg.Wake()
	}
	return nil
}

// defaultBranchCommit is the commit the push left on the default branch, or ""
// when it did not move it (or deleted it).
func defaultBranchCommit(repo store.Repository, ev eventsv1.GitRefsUpdated) string {
	ref := "refs/heads/" + repo.DefaultBranch
	for _, u := range ev.Updates {
		if u.Ref == ref && u.Kind != eventsv1.GitKindDelete {
			return u.New
		}
	}
	return ""
}

// readIndex reads and checks the definitions at the pushed commit. A failure to
// reach the worker is returned (the delivery repeats); a commit whose files the
// worker refuses (missing, too many or too large) is one recorded refusal and
// disables nothing.
func (d *Dispatcher) readIndex(ctx context.Context, tenant domain.TenantID, repo store.Repository, commit string) (*store.PushIndex, error) {
	ix := &store.PushIndex{Ref: "refs/heads/" + repo.DefaultBranch, Commit: commit}
	resp, err := d.cfg.Files.ReadFiles(ctx, tenant, gitv1.ReadFilesRequest{RepositoryID: repo.ID, Commit: commit, Paths: []string{gitv1.DefinitionDir}})
	if err != nil {
		var ce *gitv1.ContractError
		if errors.As(err, &ce) && (ce.Code == gitv1.CodeCommitNotFound || ce.Code == domain.CodeInvalidRequest) {
			ix.Unreadable = true
			ix.Refusals = []store.IndexRefusal{{Path: gitv1.DefinitionDir, Code: CodeFilesRefused}}
			return ix, nil
		}
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range resp.Files {
		name := strings.TrimSuffix(strings.TrimSuffix(path.Base(f.Path), ".yaml"), ".yml")
		refuse := func(code, docPath string) {
			ix.Refusals = append(ix.Refusals, store.IndexRefusal{Path: f.Path, Code: code, DocumentPath: docPath})
		}
		if seen[name] {
			refuse(CodeDuplicateName, "")
			continue
		}
		seen[name] = true
		ix.Present = append(ix.Present, name)
		doc, err := pipeline.Parse([]byte(f.Content))
		if err == nil {
			err = doc.Validate()
		}
		if err != nil {
			var diag *pipeline.Diagnostic
			if errors.As(err, &diag) {
				refuse(diag.Code, diag.Path)
			} else {
				refuse(pipeline.CodeInvalidValue, "")
			}
			continue
		}
		if doc.Name != name {
			refuse(CodeNameMismatch, "name")
			continue
		}
		canonical, err := doc.Canonical()
		if err != nil {
			refuse(pipeline.CodeInvalidValue, "")
			continue
		}
		ix.Definitions = append(ix.Definitions, store.IndexedDefinition{Name: name, Definition: canonical})
	}
	return ix, nil
}

// Plan returns the planner of one event: for each pipeline in force and each
// applied non-delete update that its triggers.git selects, one run under the
// trigger key (event, ref), pinned to the update's new commit.
func Plan(eventID, repositoryID string, updates []eventsv1.GitRefUpdate, log *slog.Logger) store.Planner {
	return func(pipelines []store.Pipeline) ([]store.RunAdmission, error) {
		var out []store.RunAdmission
		for _, p := range pipelines {
			var doc pipeline.Document
			if err := json.Unmarshal(p.Definition, &doc); err != nil {
				log.Error("an indexed pipeline cannot be read; it starts nothing", "pipeline", p.Name, "error", err)
				continue
			}
			order, err := doc.Order()
			if err != nil {
				log.Error("an indexed pipeline has no execution order; it starts nothing", "pipeline", p.Name, "error", err)
				continue
			}
			for _, u := range updates {
				if u.Kind == eventsv1.GitKindDelete || !Matches(doc.Triggers.Git, u.Ref) {
					continue
				}
				a, err := admission(p, doc, order, TriggerKey(eventID, u.Ref), repositoryID, u.New)
				if err != nil {
					return nil, err
				}
				out = append(out, a)
			}
		}
		return out, nil
	}
}

func admission(p store.Pipeline, doc pipeline.Document, order []string, triggerKey, repositoryID, commit string) (store.RunAdmission, error) {
	runID, err := store.NewID("run")
	if err != nil {
		return store.RunAdmission{}, err
	}
	byName := make(map[string]pipeline.Step, len(doc.Steps))
	for _, st := range doc.Steps {
		byName[st.Name] = st
	}
	steps := make([]store.Step, 0, len(order))
	for i, name := range order {
		steps = append(steps, store.Step{ID: name, StepKey: name, Ordinal: i, Image: byName[name].Image})
	}
	set := gitv1.SourceSet{Sources: []gitv1.Source{{RepositoryID: repositoryID, Commit: commit, Path: gitv1.RootPath, Access: gitv1.AccessRead}}}
	if err := set.Validate(); err != nil {
		return store.RunAdmission{}, err
	}
	sources, err := json.Marshal(set.Sources)
	if err != nil {
		return store.RunAdmission{}, err
	}
	return store.RunAdmission{
		PipelineID: p.ID, TriggerKey: triggerKey, Sources: sources, Steps: steps,
		Run: store.Run{ID: runID, PipelineID: p.ID, PipelineName: p.Name, Definition: p.Definition, Inputs: json.RawMessage(`{}`), ExecutionOrder: order},
	}, nil
}

// Push event kinds of a ref, as triggers.git.events names them.
const (
	EventPush = "push"
	EventTag  = "tag"
)

// EventKind is the event an update of ref is: a branch update is a push and a tag
// update a tag. Any other ref (a server ref) is no event.
func EventKind(ref string) string {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		return EventPush
	case strings.HasPrefix(ref, "refs/tags/"):
		return EventTag
	}
	return ""
}

// Matches reports whether a pipeline's trigger selects an update of ref: the
// update's event kind is among the trigger's events (default [push]) and the ref
// matches one of the patterns for that kind (branches for a push, tags for a tag).
// A pattern is a full ref or a short name relative to refs/heads/ or refs/tags/,
// and "*" matches any run of characters. A trigger without patterns selects
// nothing, so a pipeline without triggers.git is started by hand only.
func Matches(t pipeline.GitTrigger, ref string) bool {
	kind := EventKind(ref)
	if kind == "" {
		return false
	}
	events := t.Events
	if len(events) == 0 {
		events = []string{EventPush}
	}
	listed := false
	for _, e := range events {
		if e == kind {
			listed = true
		}
	}
	if !listed {
		return false
	}
	prefix, patterns := "refs/heads/", t.Branches
	if kind == EventTag {
		prefix, patterns = "refs/tags/", t.Tags
	}
	for _, p := range patterns {
		if !strings.HasPrefix(p, "refs/") {
			p = prefix + p
		}
		if glob(p, ref) {
			return true
		}
	}
	return false
}

// glob matches s against a pattern in which "*" is any run of characters
// (including none and "/") and everything else is literal.
func glob(pattern, s string) bool {
	p, i := 0, 0
	star, mark := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, mark = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			mark++
			i, p = mark, star+1
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
