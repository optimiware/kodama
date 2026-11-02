// Package pipelines is the shell's project pipeline registry (C1 of the
// pipeline-runtime simplification plan).
//
// A pipeline is a project-scoped, project-unique name bound to an immutable ID
// and one persisted document: the canonical form of the
// internal/kernel/pipeline definition. Trigger and source metadata are views
// derived from that document, never a second editable copy.
//
// The registry owns three guarantees:
//
//   - Names are unique inside a project. A second registration of a name is
//     refused with ErrDuplicateName by the registry itself; the database
//     constraint is only the last line of defence.
//   - One source owns a name. A definition from another repository, or from the
//     API when a repository owns the name (and the reverse), never replaces the
//     existing one: the attempt is recorded and refused with ErrSourceConflict.
//   - Indexing is idempotent on (event, ref, commit) for a name: replaying an
//     index creates nothing new, and an older replay never regresses a newer
//     definition.
//
// Every operation takes a tenant and project scope and treats a project or
// repository outside it exactly as one that does not exist (ErrNotFound).
// Validation is delegated to internal/kernel/pipeline; its stable diagnostic
// codes are surfaced as *pipeline.Diagnostic, whose messages never echo
// commands, environment values or secret values.
//
// Memory and PostgreSQL are adapters of the Store port and share the policy in
// decide, so one contract suite qualifies both.
package pipelines

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/kernel/pipeline"
)

// Stable errors. Callers classify with errors.Is; the messages are fixed and
// carry no document content.
var (
	// ErrNotFound reports a name, ID, project or repository that does not exist
	// in the caller's scope. A foreign one is indistinguishable.
	ErrNotFound error = &domain.Error{Code: domain.CodeNotFound, Message: "pipeline not found"}
	// ErrDuplicateName reports a registration of a name the project already has.
	ErrDuplicateName error = &domain.Error{Code: CodeDuplicateName, Message: "the project already has a pipeline with this name"}
	// ErrSourceConflict reports a definition from a source that does not own the
	// name. The attempt is recorded; the existing definition is untouched.
	ErrSourceConflict error = &domain.Error{Code: CodeSourceConflict, Message: "another source owns this pipeline name"}
)

// Stable registry error codes.
const (
	CodeDuplicateName  domain.ErrorCode = "pipeline_duplicate_name"
	CodeSourceConflict domain.ErrorCode = "pipeline_source_conflict"
)

// ConflictError is the detail of a refused source conflict. It matches
// ErrSourceConflict. It names the two sources (project data, not secrets) and
// never carries document content.
type ConflictError struct {
	Name      string
	Existing  Source
	Attempted Source
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s: %s (owned by %s, refused %s)", CodeSourceConflict, e.Name, e.Existing, e.Attempted)
}

func (e *ConflictError) Unwrap() error { return ErrSourceConflict }

// SourceKind is where a definition comes from.
type SourceKind string

const (
	SourceAPI SourceKind = "api"
	SourceGit SourceKind = "git"
)

// Source identifies one definition source. A Git source is one repository of
// the project; the API source is the project itself.
type Source struct {
	Kind         SourceKind `json:"kind"`
	RepositoryID string     `json:"repositoryId,omitempty"`
}

func (s Source) String() string {
	if s.Kind == SourceGit {
		return "git:" + s.RepositoryID
	}
	return string(s.Kind)
}

// Definition is one registered pipeline. ID never changes for the life of the
// name. Document is the canonical JSON of the validated definition (adapters
// may normalize key order; compare by decoding).
type Definition struct {
	Scope    domain.Scope
	ID       string
	Name     string
	Source   Source
	Commit   string // set for a Git source: the exact commit read
	Document json.RawMessage
	Version  domain.Version
	Disabled bool
	Created  time.Time
	Updated  time.Time
}

// Conflict is one recorded and refused definition. It never holds the refused
// document.
type Conflict struct {
	ID         string
	Scope      domain.Scope
	Name       string
	PipelineID string // the definition that kept the name
	Existing   Source
	Attempted  Source
	EventID    string
	Ref        string
	Commit     string
	Recorded   time.Time
}

// RegisterRequest registers an API definition.
type RegisterRequest struct {
	Scope domain.Scope
	// Document is the YAML (or JSON) pipeline document.
	Document []byte
}

// IndexRequest indexes the definition of one file at an exact commit of a
// project repository. EventID, Ref and Commit identify the attempt: repeating
// them for the same name changes nothing.
type IndexRequest struct {
	Scope        domain.Scope
	RepositoryID string
	EventID      string
	Ref          string
	Commit       string
	Document     []byte
}

// Result reports what an accepted operation did.
type Result struct {
	Definition Definition
	// Created is true when this call created the definition; Updated when it
	// replaced the same source's earlier definition; Replayed when the attempt
	// was recorded before and nothing was done. A definition identical to the
	// stored one is none of them.
	Created  bool
	Updated  bool
	Replayed bool
}

// Registry is the narrow port consumers (task creation, run admission, the
// transports) use. Both adapters implement it through Service.
type Registry interface {
	// Resolve returns the immutable ID of the project's pipeline of that name.
	Resolve(ctx context.Context, scope domain.Scope, name string) (string, error)
	// Register adds an API-managed definition. It is refused with
	// ErrDuplicateName when the name exists (never replacing it) and with
	// ErrSourceConflict, recording the attempt, when a repository owns the name.
	Register(ctx context.Context, req RegisterRequest) (Result, error)
	// Index records a repository's definition at a commit. It is idempotent per
	// (event, ref, commit, name) and refused with ErrSourceConflict, recording
	// the attempt, when another source owns the name.
	Index(ctx context.Context, req IndexRequest) (Result, error)
	// Get reads one definition by its immutable ID.
	Get(ctx context.Context, scope domain.Scope, id string) (Definition, error)
}

// ConflictLog reads the recorded refusals of a project, oldest first.
type ConflictLog interface {
	Conflicts(ctx context.Context, scope domain.Scope) ([]Conflict, error)
}

// Submission is a validated definition handed to the Store. It is built only by
// Service, so an adapter never sees an unvalidated document.
type Submission struct {
	Scope    domain.Scope
	Name     string
	Document json.RawMessage // canonical
	Source   Source
	// AttemptKey is the deterministic identity of the attempt: replaying it is
	// answered from the record. For an API registration it identifies the
	// (name, document) pair.
	AttemptKey string
	EventID    string
	Ref        string
	Commit     string
}

// Store is the persistence port of the registry. Each method is atomic and
// scoped: a project or repository outside the scope is ErrNotFound.
type Store interface {
	Resolve(ctx context.Context, scope domain.Scope, name string) (string, error)
	Get(ctx context.Context, scope domain.Scope, id string) (Definition, error)
	// Submit applies decide to the name's current definition and persists the
	// outcome (definition change or conflict record) in one transaction.
	Submit(ctx context.Context, sub Submission) (Result, error)
	Conflicts(ctx context.Context, scope domain.Scope) ([]Conflict, error)
}

// Service validates requests and delegates to a Store.
type Service struct{ store Store }

var (
	_ Registry    = (*Service)(nil)
	_ ConflictLog = (*Service)(nil)
)

// New returns the registry over a store adapter.
func New(store Store) *Service { return &Service{store: store} }

const maxDocumentBytes = 256 << 10

var (
	commitRE = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	nameRE   = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
)

func (s *Service) Resolve(ctx context.Context, scope domain.Scope, name string) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if !nameRE.MatchString(name) {
		return "", ErrNotFound
	}
	return s.store.Resolve(ctx, scope, name)
}

func (s *Service) Get(ctx context.Context, scope domain.Scope, id string) (Definition, error) {
	if err := scope.Validate(); err != nil {
		return Definition{}, err
	}
	if domain.PipelineID(id).Validate() != nil {
		return Definition{}, ErrNotFound
	}
	return s.store.Get(ctx, scope, id)
}

func (s *Service) Conflicts(ctx context.Context, scope domain.Scope) ([]Conflict, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	return s.store.Conflicts(ctx, scope)
}

func (s *Service) Register(ctx context.Context, req RegisterRequest) (Result, error) {
	if err := req.Scope.Validate(); err != nil {
		return Result{}, err
	}
	name, doc, err := validateDocument(req.Document)
	if err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(doc)
	return s.store.Submit(ctx, Submission{
		Scope: req.Scope, Name: name, Document: doc, Source: Source{Kind: SourceAPI},
		AttemptKey: attemptKey("api", string(req.Scope.TenantID), string(req.Scope.ProjectID), name, hex.EncodeToString(sum[:])),
	})
}

func (s *Service) Index(ctx context.Context, req IndexRequest) (Result, error) {
	if err := req.Scope.Validate(); err != nil {
		return Result{}, err
	}
	if domain.RepositoryID(req.RepositoryID).Validate() != nil {
		return Result{}, ErrNotFound
	}
	if err := validateBounded("event id", req.EventID, 256); err != nil {
		return Result{}, err
	}
	if err := validateBounded("ref", req.Ref, 512); err != nil {
		return Result{}, err
	}
	if !commitRE.MatchString(req.Commit) {
		return Result{}, fmt.Errorf("%w: commit must be a full lowercase hexadecimal object ID", domain.ErrInvalidValue)
	}
	name, doc, err := validateDocument(req.Document)
	if err != nil {
		return Result{}, err
	}
	return s.store.Submit(ctx, Submission{
		Scope: req.Scope, Name: name, Document: doc,
		Source:     Source{Kind: SourceGit, RepositoryID: req.RepositoryID},
		AttemptKey: attemptKey("git", string(req.Scope.TenantID), string(req.Scope.ProjectID), req.RepositoryID, req.EventID, req.Ref, req.Commit, name),
		EventID:    req.EventID, Ref: req.Ref, Commit: req.Commit,
	})
}

func validateBounded(what, v string, max int) error {
	if v == "" || len(v) > max || strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%w: %s must be 1 to %d printable bytes", domain.ErrInvalidValue, what, max)
	}
	return nil
}

// validateDocument parses, validates and canonicalizes a document through the
// kernel. A refusal is the kernel's *pipeline.Diagnostic; nothing about the
// document is echoed.
func validateDocument(data []byte) (string, json.RawMessage, error) {
	if len(data) > maxDocumentBytes {
		return "", nil, &pipeline.Diagnostic{Code: pipeline.CodeInvalidValue, Path: "$", Message: "the document exceeds the size limit"}
	}
	doc, err := pipeline.Parse(data)
	if err != nil {
		return "", nil, err
	}
	if err := doc.Validate(); err != nil {
		return "", nil, err
	}
	canon, err := doc.Canonical()
	if err != nil {
		return "", nil, fmt.Errorf("pipelines: canonical form: %w", err)
	}
	return doc.Name, canon, nil
}

func attemptKey(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s;", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil)[:20])
}

// Action is what decide chose for a submission.
type Action int

const (
	ActionCreate    Action = iota // no definition has the name
	ActionUnchanged               // the same source, commit and document: nothing to do
	ActionUpdate                  // the owning source replaced its definition
	ActionDuplicate               // an API registration of an existing name
	ActionConflict                // another source owns the name
)

// decide is the registry policy, shared by every adapter so they cannot drift.
// existing is the definition currently holding the name (nil if none);
// sameDocument reports whether it holds the submission's document.
func decide(existing *Definition, sub Submission, sameDocument bool) Action {
	switch {
	case existing == nil:
		return ActionCreate
	case existing.Source != sub.Source:
		return ActionConflict
	case sub.Source.Kind == SourceAPI:
		// The API never replaces: replacing or renaming is not a registration.
		return ActionDuplicate
	case existing.Commit == sub.Commit && sameDocument && !existing.Disabled:
		return ActionUnchanged
	default:
		return ActionUpdate
	}
}
