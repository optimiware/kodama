package git

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/optimiware/kodama/api"
)

// The native hook environment contract. The HTTP front handler places only an
// opaque actor handle in the backend environment; the hook resolves that handle
// through the injected ActorSource. The raw credential never appears in the
// environment, the journal, a message or a log.
const (
	// EnvActorRef names the opaque actor handle set by the smart-HTTP handler.
	EnvActorRef = "KODAMA_ACTOR_REF"
	// EnvRepository names the tenant-relative repository being pushed.
	EnvRepository = "KODAMA_REPOSITORY"
	// EnvResourceID names the registered resource metadata identity.
	EnvResourceID = "KODAMA_RESOURCE_ID"
	// EnvSourceEvent names the unique per-push identity minted by the
	// smart-HTTP handler. It is required: without a fresh identity an identical
	// later push could settle a stale journal entry instead of verifying again.
	EnvSourceEvent = "KODAMA_SOURCE_EVENT"
)

// ActorSource resolves the opaque backend handle to the forwarded actor
// credential used only for the immediate update verification.
type ActorSource interface {
	ResolveActor(ctx context.Context, handle string) (string, error)
}

// HookConfig is the complete native-hook dependency set.
type HookConfig struct {
	Service      *Service
	Actors       ActorSource
	Environment  func(string) (string, bool)
	MaxLineBytes int
	MaxBatch     int
}

// Hook is the native `update`/`pre-receive` authority adapter. It verifies the
// exact ref update immediately before acceptance and returns a git-client exit
// code. Uncertain or dependency-failed updates always reject.
type Hook struct {
	service     *Service
	actors      ActorSource
	environment func(string) (string, bool)
	maxLine     int
	maxBatch    int
}

// HookResult is one hook decision. ExitCode is 0 for accept and 1 for reject;
// Message is a sanitized one-line client diagnostic.
type HookResult struct {
	Decision Decision `json:"decision"`
	ExitCode int      `json:"exit_code"`
	Message  string   `json:"message,omitempty"`
}

// Accepted reports whether the hook accepted the update or batch.
func (result HookResult) Accepted() bool {
	return result.Decision == DecisionAllow && result.ExitCode == 0
}

var (
	errMalformedHook = errors.New("malformed native Git hook input")
	// The journal candidate is non-empty: a delete has no durable
	// representation, so the hook refuses it before any effect. `RW+` delete
	// permission semantics stay in the rule evaluator; accepting a delete needs
	// a journal format change, not a silent zero-object-id candidate.
	errDeleteUnsupported = errors.New("delete is not representable in the receive journal")
)

const (
	defaultHookMaxLineBytes = 4096
	defaultHookMaxBatch     = 256
	hookRejectionMessage    = "kodama git: update rejected"
)

// NewHook validates the boundaries and returns the hook adapter.
func NewHook(config HookConfig) (*Hook, error) {
	switch {
	case config.Service == nil:
		return nil, fmt.Errorf("git hook service is missing")
	case config.Actors == nil:
		return nil, fmt.Errorf("git hook actor source is missing")
	}
	environment := config.Environment
	if environment == nil {
		environment = os.LookupEnv
	}
	maxLine := config.MaxLineBytes
	if maxLine <= 0 {
		maxLine = defaultHookMaxLineBytes
	}
	maxBatch := config.MaxBatch
	if maxBatch <= 0 {
		maxBatch = defaultHookMaxBatch
	}
	return &Hook{
		service:     config.Service,
		actors:      config.Actors,
		environment: environment,
		maxLine:     maxLine,
		maxBatch:    maxBatch,
	}, nil
}

// RunUpdate is the `update` hook entry: `<ref> <old> <new>`.
func (hook *Hook) RunUpdate(ctx context.Context, ref, old, new string) (HookResult, error) {
	update, err := parseHookUpdate(ref, old, new)
	if err != nil {
		return rejectHook(err)
	}
	// Ref deletes are refused: the journal cannot represent them.
	if update.delete {
		return rejectHook(errDeleteUnsupported)
	}
	return hook.runOne(ctx, update)
}

// RunPreReceive is the `pre-receive` hook entry: one `<old> <new> <ref>` line
// per update. Every ref is verified and journaled as an intent before the
// decision; the adapter performs no ref effect and sends no report. A refusal
// of any ref rejects the whole batch, and the native receive-pack ref
// transaction remains the atomic all-or-nothing boundary. Acceptance and
// `git.ref.updated` reporting follow from RunPostReceive.
func (hook *Hook) RunPreReceive(ctx context.Context, stdin io.Reader) (HookResult, error) {
	updates, err := hook.parsePreReceive(stdin)
	if err != nil {
		return rejectHook(err)
	}
	for _, update := range updates {
		// Ref deletes are refused: the journal cannot represent them.
		if update.delete {
			return rejectHook(errDeleteUnsupported)
		}
	}
	return hook.runBatch(ctx, updates)
}

// RunPostReceive is the `post-receive` recovery entry: one `<old> <new> <ref>`
// line per applied update. It performs no ref effect and no verification. It
// reconciles each journaled intent from observed ref state through
// Service.Recover, which records acceptance and reports the verified effect.
// A failure leaves the durable entry for a later recovery retry; the native
// receive-pack transaction has already decided the ref outcome.
func (hook *Hook) RunPostReceive(ctx context.Context, stdin io.Reader) (HookResult, error) {
	updates, err := hook.parsePreReceive(stdin)
	if err != nil {
		return rejectHook(err)
	}
	for _, update := range updates {
		// Ref deletes are refused: the journal cannot represent them.
		if update.delete {
			return rejectHook(errDeleteUnsupported)
		}
	}
	sourceEventIDs, err := hook.batchEventIDs(updates)
	if err != nil {
		return rejectHook(err)
	}
	var recoverErr error
	for _, sourceEventID := range sourceEventIDs {
		if _, err := hook.service.Recover(ctx, sourceEventID); err != nil {
			recoverErr = errors.Join(recoverErr, err)
		}
	}
	if recoverErr != nil {
		return rejectHook(recoverErr)
	}
	return acceptHook()
}

// runOne performs the exact single-ref update through Service.Update: verify,
// evaluate current rules/approval, journal intent, compare-and-swap, record
// acceptance and report.
func (hook *Hook) runOne(ctx context.Context, update hookUpdate) (HookResult, error) {
	requests, err := hook.buildRequests(ctx, []hookUpdate{update})
	if err != nil {
		return rejectHook(err)
	}
	settled, err := hook.service.Update(ctx, requests[0])
	if err != nil {
		return rejectHook(err)
	}
	switch settled.Entry.State {
	case JournalReported, JournalAccepted:
		return acceptHook()
	case JournalUncertain:
		return rejectHook(ErrUncertain)
	default:
		return rejectHook(ErrPolicyDenied)
	}
}

// runBatch is the atomic pre-receive form. It verifies every ref and journals
// one intent per ref, but performs no ref effect and sends no report. The
// native receive-pack ref transaction is the atomic boundary; a refusal of any
// ref rejects the whole batch while nothing has been written. Acceptance and
// reports are produced later from observation by RunPostReceive.
func (hook *Hook) runBatch(ctx context.Context, updates []hookUpdate) (HookResult, error) {
	requests, err := hook.buildRequests(ctx, updates)
	if err != nil {
		return rejectHook(err)
	}
	entries, fresh, err := hook.service.prepareBatch(ctx, requests)
	if err != nil {
		return rejectHook(err)
	}
	// Refuse a batch whose verified base is no longer observable before the
	// native transaction. This is a read-only pre-check; it never applies.
	for _, entry := range entries {
		observed, err := hook.service.observer.Observe(ctx, entry.Repository, entry.Ref)
		if err != nil {
			hook.service.refuseBatchEntries(ctx, fresh)
			return rejectHook(err)
		}
		if observed.Ref != entry.Ref {
			hook.service.refuseBatchEntries(ctx, fresh)
			return rejectHook(ErrStaleBase)
		}
		if observed.Commit != entry.ExpectedBase && observed.Commit != entry.Candidate {
			hook.service.refuseBatchEntries(ctx, fresh)
			return rejectHook(ErrStaleBase)
		}
	}
	return acceptHook()
}

// hookUpdate is one validated native ref update. old is empty for a create;
// delete marks a zero new object ID.
type hookUpdate struct {
	ref    string
	old    string
	new    string
	delete bool
}

// parseHookUpdate validates the ref and both object IDs.
func parseHookUpdate(ref, old, new string) (hookUpdate, error) {
	if !validHookRef(ref) {
		return hookUpdate{}, errMalformedHook
	}
	if !validObjectID(old) || !validObjectID(new) {
		return hookUpdate{}, errMalformedHook
	}
	oldZero := isZeroObjectID(old)
	newZero := isZeroObjectID(new)
	if oldZero && newZero {
		return hookUpdate{}, errMalformedHook
	}
	update := hookUpdate{ref: ref, new: new}
	if oldZero {
		update.old = ""
	} else {
		update.old = old
	}
	if newZero {
		update.delete = true
		update.new = ""
	}
	return update, nil
}

// validHookRef restricts native hook updates to branch and tag refs under the
// declared anchored-ref grammar.
func validHookRef(ref string) bool {
	if !validRef(ref) || strings.HasSuffix(ref, "/") {
		return false
	}
	for index := 0; index < len(ref); index++ {
		if ref[index] < 0x20 || ref[index] == 0x7f {
			return false
		}
	}
	return strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, "refs/tags/")
}

// validObjectID accepts the SHA-1 (40) and SHA-256 (64) hex object IDs.
func validObjectID(value string) bool {
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

func isZeroObjectID(value string) bool {
	return strings.Trim(value, "0") == ""
}

// parsePreReceive reads one bounded `<old> <new> <ref>` line per update.
func (hook *Hook) parsePreReceive(stdin io.Reader) ([]hookUpdate, error) {
	if stdin == nil {
		return nil, errMalformedHook
	}
	scanner := bufio.NewScanner(stdin)
	scanner.Buffer(make([]byte, 0, hook.maxLine), hook.maxLine)
	var updates []hookUpdate
	seen := make(map[string]bool)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			return nil, errMalformedHook
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, errMalformedHook
		}
		update, err := parseHookUpdate(fields[2], fields[0], fields[1])
		if err != nil {
			return nil, err
		}
		if seen[update.ref] {
			return nil, errMalformedHook
		}
		seen[update.ref] = true
		updates = append(updates, update)
		if len(updates) > hook.maxBatch {
			return nil, errMalformedHook
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(updates) == 0 {
		return nil, errMalformedHook
	}
	return updates, nil
}

// buildRequests resolves the actor handle and stable environment contract into
// one UpdateRequest per ref. The raw credential is used only for the immediate
// service call and is never returned. The required per-push identity makes an
// identical later push a different source event; only a redelivery of the same
// push reuses an entry.
func (hook *Hook) buildRequests(ctx context.Context, updates []hookUpdate) ([]UpdateRequest, error) {
	actorRef, ok := hook.lookup(EnvActorRef)
	if !ok || !validActorHandle(actorRef) {
		return nil, errMalformedHook
	}
	repository, pushID, err := hook.pushIdentity()
	if err != nil {
		return nil, err
	}
	resourceID, ok := hook.lookup(EnvResourceID)
	if !ok || !validJournalID(resourceID) {
		return nil, errMalformedHook
	}
	credential, err := hook.actors.ResolveActor(ctx, actorRef)
	if err != nil || credential == "" || len(credential) > api.MaxCredentialBytes {
		return nil, errMalformedHook
	}
	requests := make([]UpdateRequest, 0, len(updates))
	for _, update := range updates {
		requests = append(requests, UpdateRequest{
			ResourceID:      resourceID,
			Repository:      repository,
			Ref:             update.ref,
			ExpectedBase:    update.old,
			Candidate:       update.new,
			ActorCredential: credential,
			SourceEventID:   hook.sourceEventID(pushID, repository, update),
		})
	}
	return requests, nil
}

func (hook *Hook) lookup(key string) (string, bool) {
	return hook.environment(key)
}

// pushIdentity reads the required repository and unique per-push identity from
// the native hook environment. A missing or malformed KODAMA_SOURCE_EVENT is a
// refusal: without a fresh push identity an identical repeat could settle a
// stale journal entry instead of verifying again.
func (hook *Hook) pushIdentity() (string, string, error) {
	repository, ok := hook.lookup(EnvRepository)
	if !ok || validateRepositoryName(repository) != nil {
		return "", "", errMalformedHook
	}
	pushID, ok := hook.lookup(EnvSourceEvent)
	if !ok || !validJournalID(pushID) {
		return "", "", errMalformedHook
	}
	return repository, pushID, nil
}

// batchEventIDs derives the per-ref source-event identities for a post-receive
// batch. It needs only the repository and the unique push identity; recovery
// never re-verifies the actor.
func (hook *Hook) batchEventIDs(updates []hookUpdate) ([]string, error) {
	repository, pushID, err := hook.pushIdentity()
	if err != nil {
		return nil, err
	}
	sourceEventIDs := make([]string, 0, len(updates))
	for _, update := range updates {
		sourceEventIDs = append(sourceEventIDs, hook.sourceEventID(pushID, repository, update))
	}
	return sourceEventIDs, nil
}

// sourceEventID derives a stable receive-event identity from the unique push
// identity and the exact ref update. The smart-HTTP handler mints a fresh push
// identity for every request, so an identical later push cannot reuse an
// earlier reported entry; the same push delivered twice keeps the same identity
// and settles the existing entry.
func (hook *Hook) sourceEventID(pushID, repository string, update hookUpdate) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		"kodama-git-hook-v2",
		pushID,
		repository,
		update.ref,
		update.old,
		update.new,
	}, "\x00")))
	return hex.EncodeToString(sum[:])
}

func acceptHook() (HookResult, error) {
	return HookResult{Decision: DecisionAllow, ExitCode: 0}, nil
}

// rejectHook returns a sanitized one-line rejection. The cause is returned for
// internal diagnostics; callers must send Message, never cause, to the Git
// client.
func rejectHook(cause error) (HookResult, error) {
	if cause == nil {
		cause = ErrPolicyDenied
	}
	return HookResult{Decision: DecisionDeny, ExitCode: 1, Message: hookRejectionMessage}, cause
}
