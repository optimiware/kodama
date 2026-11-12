// Authorized execution.logs.read operation service.
//
// The retained sink is a scoped storage seam: it can serve one bounded page for
// one explicit ownership scope, execution, action and stream, and it is the
// retention authority that expires stored chunks. This service adds the
// operation boundary the declared registry row requires: the registry grant is
// evaluated first, the current principal is re-read, an execution principal is
// bound to the log of its own task's current execution, and only then does the
// sink see a read. A refused call never touches storage.
//
// The service returns the sink's already-redacted retained bytes. It never
// reads raw workload output, never re-redacts (a second pass could mask a
// legitimate marker), and never invents a chunk. A cursor is an opaque,
// versioned token carrying the (offset, sequence) pair of the last served
// chunk. An offset alone is not unique: the sink accepts empty chunks and
// several chunks may share one offset, so only the pair names a chunk exactly.
// A continuation resumes strictly after that pair; it is bound to the
// command's execution/action/stream by the caller passing the same target.

package log

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// operationRead is the declared registry row this service implements.
const operationRead authorize.OperationName = "execution.logs.read"

// Read bounds. They match the public page bound so the operation cannot be used
// to ask for a larger page than the public edge accepts.
const (
	// DefaultReadLimit is the page size used when a caller omits a limit.
	DefaultReadLimit = api.DefaultPageLimit
	// MaxReadLimit is the largest accepted retained-log page size.
	MaxReadLimit = api.MaxPageLimit
)

// ReaderConfig supplies the operation boundaries. Every collaborator is
// required: the service refuses an incomplete boundary instead of accepting a
// nil dependency that could bypass a check.
type ReaderConfig struct {
	Authorizer authorize.Authorizer
	Authority  authorize.AuthorityReader
	Principals identity.PrincipalReader
	Ownership  ExecutionOwnershipReader
	Sink       RetainedSink
	Clock      Clock
}

// ReadCommand is one authorized retained-log read. ActionID and Stream name the
// declared sink stream to page; the retained sink has no execution-wide read.
// Cursor is empty for the first page.
type ReadCommand struct {
	Actor       authorize.Principal
	Scope       tenancy.Scope
	ExecutionID id.ExecutionID
	ActionID    id.ActionID
	Stream      Stream
	Cursor      string
	Limit       int
}

// ReadPage is one bounded retained-log page. Chunks are the sink's redacted
// retained content; NextCursor continues the same stream.
type ReadPage struct {
	Chunks     []Chunk
	NextCursor string
	Truncated  bool
}

// Reader is the authorized execution.logs.read operation service.
type Reader struct {
	authorizer authorize.Authorizer
	authority  authorize.AuthorityReader
	principals identity.PrincipalReader
	ownership  ExecutionOwnershipReader
	sink       RetainedSink
}

// NewReader validates the retained-log operation boundary.
func NewReader(config ReaderConfig) (*Reader, error) {
	switch {
	case config.Authorizer == nil:
		return nil, fault.New(fault.InternalFailure, "log reader requires an authorizer")
	case config.Authority == nil:
		return nil, fault.New(fault.InternalFailure, "log reader requires an authority reader")
	case config.Principals == nil:
		return nil, fault.New(fault.InternalFailure, "log reader requires a principal reader")
	case config.Ownership == nil:
		return nil, fault.New(fault.InternalFailure, "log reader requires an execution ownership reader")
	case config.Sink == nil:
		return nil, fault.New(fault.InternalFailure, "log reader requires a retained sink")
	case config.Clock == nil:
		return nil, fault.New(fault.InternalFailure, "log reader requires a clock")
	}
	return &Reader{
		authorizer: config.Authorizer,
		authority:  config.Authority,
		principals: config.Principals,
		ownership:  config.Ownership,
		sink:       config.Sink,
	}, nil
}

// Read authorizes and serves one bounded retained-log page. The order is the
// operation boundary: registry grant, current principal, execution binding and
// current membership authority, then the scoped sink read.
func (reader *Reader) Read(ctx context.Context, command ReadCommand) (ReadPage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := requireProjectScope(command.Scope); err != nil {
		return ReadPage{}, err
	}
	if err := checkActor(command.Actor); err != nil {
		return ReadPage{}, err
	}
	registered, err := reader.authorize(ctx, command.Actor, command.Scope)
	if err != nil {
		return ReadPage{}, err
	}
	principal, err := reader.confirmPrincipal(ctx, command.Actor)
	if err != nil {
		return ReadPage{}, err
	}
	if !scopeContains(principal.Scope, command.Scope) {
		return ReadPage{}, fault.New(fault.Forbidden, "principal scope does not contain the command scope")
	}
	if err := confirmPrincipalGrant(principal, command.Actor, registered.Requires); err != nil {
		return ReadPage{}, err
	}
	if err := validateReadTarget(command); err != nil {
		return ReadPage{}, err
	}
	limit, after, err := readBounds(command)
	if err != nil {
		return ReadPage{}, err
	}
	execution := principal.Kind == identity.KindExecution
	if execution {
		if err := reader.confirmOwnTask(ctx, command.Scope, command.ExecutionID, principal); err != nil {
			return ReadPage{}, err
		}
	}
	if err := reader.confirmAuthority(ctx, registered, command.Scope, command.Actor.ID, command.ExecutionID, execution); err != nil {
		return ReadPage{}, err
	}
	return reader.read(ctx, command, limit, after)
}

// authorize evaluates the registry grant. A human or service principal is
// decided by the shared matrix. An execution principal carries the registry's
// execution permission instead of a membership role, so it is decided by the
// admitted-execution matrix and never by a human/service fallback.
func (reader *Reader) authorize(ctx context.Context, actor authorize.Principal, scope tenancy.Scope) (authorize.Operation, error) {
	registered, ok := authorize.LookupOperation(operationRead)
	if !ok {
		return authorize.Operation{}, fault.New(fault.InternalFailure, "execution.logs.read is not registered")
	}
	var (
		decision authorize.Decision
		err      error
	)
	if actor.Kind == identity.KindExecution {
		admitted := authorize.AdmittedExecution{
			Principal:   actor,
			ExecutionID: actor.ExecutionID,
			Permission:  registered.Requires.Execution,
		}
		decision, err = authorize.AuthorizeExecution(admitted, operationRead, scope)
	} else {
		decision, err = reader.authorizer.Authorize(ctx, actor, operationRead, scope)
	}
	if err != nil {
		if _, ok := fault.ClassOf(err); ok {
			return authorize.Operation{}, err
		}
		return authorize.Operation{}, fault.Wrap(fault.InternalFailure, "execution.logs.read authorization failed", err)
	}
	if decision != authorize.DecisionAllow {
		return authorize.Operation{}, fault.New(fault.Forbidden, "principal is not authorized for execution.logs.read")
	}
	return registered, nil
}

// confirmPrincipal re-reads the actor's current principal row so a previously
// calculated allow never substitutes for current enablement, scope and kind.
func (reader *Reader) confirmPrincipal(ctx context.Context, actor authorize.Principal) (identity.Principal, error) {
	principal, err := reader.principals.ReadPrincipal(ctx, identity.PrincipalRequest{
		Scope:       actor.Scope,
		PrincipalID: actor.ID,
	})
	if err != nil {
		return identity.Principal{}, err
	}
	if principal.Scope != actor.Scope {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal is not present in the actor scope")
	}
	if !principal.Enabled {
		return identity.Principal{}, fault.New(fault.Forbidden, "principal is disabled")
	}
	if principal.Kind != actor.Kind {
		return identity.Principal{}, fault.New(fault.Forbidden, "current principal kind does not match the authenticated actor")
	}
	return principal, nil
}

// confirmPrincipalGrant applies the registry row to the current principal. An
// execution principal must use an execution permission, never a membership
// role; a human or service principal must currently hold one of its declared
// roles.
func confirmPrincipalGrant(principal identity.Principal, actor authorize.Principal, requires authorize.Requires) error {
	if principal.Kind == identity.KindExecution {
		if requires.Execution == authorize.ExecNone {
			return fault.New(fault.Forbidden, "execution principal has no permission for execution.logs.read")
		}
		if principal.ExecutionID == "" || principal.ExecutionID != actor.ExecutionID {
			return fault.New(fault.Forbidden, "execution principal identity does not match the authenticated actor")
		}
		return nil
	}
	if !readerGrantHeld(principal, requires) {
		return fault.New(fault.Forbidden, "principal does not currently hold the grant for execution.logs.read")
	}
	return nil
}

// readerGrantHeld reports whether a human or service principal currently holds
// one of the registry row's roles for its kind. Execution authority is handled
// separately and is never a membership grant.
func readerGrantHeld(principal identity.Principal, requires authorize.Requires) bool {
	switch principal.Kind {
	case identity.KindHuman:
		if slices.ContainsFunc(requires.HumanRoles, principal.HasRole) {
			return true
		}
	case identity.KindService:
		if slices.ContainsFunc(requires.ServiceRoles, principal.HasRole) {
			return true
		}
	}
	return false
}

// confirmOwnTask binds an execution principal to its own task's current
// execution. A foreign execution, a stale previous execution, a task-free
// execution or an execution outside the scope is refused Forbidden before the
// sink is asked for any content.
func (reader *Reader) confirmOwnTask(ctx context.Context, scope tenancy.Scope, executionID id.ExecutionID, principal identity.Principal) error {
	// The target must be the principal's own execution; the ownership read
	// below then proves it is still that task's current execution.
	if principal.ExecutionID == "" || principal.ExecutionID != executionID {
		return fault.New(fault.Forbidden, "execution principal may read only its own task's current execution")
	}
	binding, err := reader.ownership.Ownership(ctx, scope, executionID)
	if err != nil {
		if class, ok := fault.ClassOf(err); ok {
			if class == fault.NotFound {
				return fault.New(fault.Forbidden, "execution principal may read only its own task's current execution")
			}
			return err
		}
		return fault.Wrap(fault.InternalFailure, "execution ownership read failed", err)
	}
	if binding.TaskID == "" || binding.CurrentExecution == "" {
		return fault.New(fault.Forbidden, "execution principal may read only its own task's current execution")
	}
	if binding.CurrentExecution != principal.ExecutionID {
		return fault.New(fault.Forbidden, "execution principal may read only its own task's current execution")
	}
	return nil
}

// confirmAuthority re-reads the registry row's current authority facts. The
// membership check is a human/service fact; an execution principal is bound by
// its execution permission and own-task association instead. Any other
// registry check still applies to both kinds.
func (reader *Reader) confirmAuthority(ctx context.Context, registered authorize.Operation, scope tenancy.Scope, principalID id.PrincipalID, executionID id.ExecutionID, execution bool) error {
	reads := make([]authorize.AuthorityRead, 0, len(registered.Authority))
	for _, check := range registered.Authority {
		if execution && check == authorize.CheckMembership {
			continue
		}
		read := authorize.AuthorityRead{
			Operation:   registered.Name,
			Required:    registered.Requires,
			Check:       check,
			Scope:       scope,
			PrincipalID: principalID,
		}
		if execution {
			switch check {
			case authorize.CheckExecutionRunning, authorize.CheckExecutionVersion:
				read.TargetID = string(executionID)
			}
		}
		reads = append(reads, read)
	}
	if len(reads) == 0 {
		return nil
	}
	facts, err := reader.authority.Read(ctx, reads)
	if err != nil {
		return err
	}
	return authorize.Confirm(reads, facts)
}

// chunkPosition is the unique order key of one retained chunk. Append accepts
// empty chunks and several chunks may share one offset, so only the
// (offset, sequence) pair names a chunk exactly.
type chunkPosition struct {
	offset   uint64
	sequence uint64
}

// after reports whether this position follows other in the sink's order.
func (position chunkPosition) after(other chunkPosition) bool {
	if position.offset != other.offset {
		return position.offset > other.offset
	}
	return position.sequence > other.sequence
}

// pairReader is an optional retained-sink capability. The declared RetainedSink
// contract carries only an offset lower bound, which cannot address a later
// chunk that shares an offset. FileSink implements this so a shared-offset
// group of any size pages without repeating or skipping a chunk. A sink that
// does not implement it is served by re-reading from the start of the group
// with a growing window.
type pairReader interface {
	readAfterPair(context.Context, ReadRequest, uint64) (ReadResult, error)
}

// maxWindowProbe bounds the fallback re-read growth. A served prefix larger
// than this cannot be addressed through the declared offset-only read contract;
// such a continuation is reported as a retryable dependency failure rather
// than a permanent internal failure or a silently skipped chunk.
const maxWindowProbe = 1 << 16

// read serves the bounded sink page and maps adapter failures to a public
// fault. A private adapter cause is never rendered, and a chunk outside the
// requested execution/action/stream is refused instead of returned.
//
// Paging resumes strictly after the last served (offset, sequence) pair. The
// service asks the sink for one lookahead chunk so a full window proves that
// at least one chunk follows; a sink that caps its own window is asked for a
// smaller page, and a reduced page is confirmed with one extra lookahead read.
// The cursor always names the last served pair, so a page may end inside a
// shared-offset group without losing the chunks that follow. An empty cursor
// is emitted only when no chunk remains.
func (reader *Reader) read(ctx context.Context, command ReadCommand, limit int, after *chunkPosition) (ReadPage, error) {
	chunks, more, truncated, err := reader.fetchPage(ctx, command, after, limit)
	if err != nil {
		return ReadPage{}, err
	}
	nextCursor := ""
	if more {
		if len(chunks) == 0 {
			return ReadPage{}, fault.New(fault.InternalFailure, "retained log read made no progress")
		}
		cursor, err := encodeChunkPosition(chunks[len(chunks)-1])
		if err != nil {
			return ReadPage{}, fault.Wrap(fault.InternalFailure, "retained log cursor could not be encoded", err)
		}
		nextCursor = cursor
	}
	return ReadPage{
		Chunks:     chunks,
		NextCursor: nextCursor,
		Truncated:  truncated,
	}, nil
}

// fetchPage returns at most limit chunks strictly after the cursor pair and
// reports whether at least one further chunk remains. It never derives
// continuation from the raw number of chunks the sink returned before the
// already-served prefix was filtered out.
func (reader *Reader) fetchPage(ctx context.Context, command ReadCommand, after *chunkPosition, limit int) ([]Chunk, bool, bool, error) {
	want := limit + 1
	window := want
	for {
		result, err := reader.fetchWindow(ctx, command, after, window)
		if err != nil {
			if window > 1 && isInvalidInput(err) {
				// A sink may cap the page it will serve. A smaller request
				// still advances the pair, so reduce the window instead of
				// making the stream unreadable.
				window /= 2
				continue
			}
			return nil, false, false, err
		}
		chunks := result.Chunks
		if len(chunks) > limit {
			chunks = chunks[:limit]
		}
		switch {
		case len(result.Chunks) == window && window >= want:
			// The lookahead chunk proves at least one chunk follows the page.
			return chunks, true, result.Truncated, nil
		case len(result.Chunks) == window:
			// The sink filled a reduced window, so its end is not proven;
			// confirm continuation with one chunk after the last fetched one.
			more, err := reader.hasMore(ctx, command, result.Chunks[len(result.Chunks)-1])
			if err != nil {
				return nil, false, false, err
			}
			return chunks, more, result.Truncated, nil
		default:
			// A partial window means the sink has no more chunks.
			return chunks, false, result.Truncated, nil
		}
	}
}

// hasMore reads one chunk strictly after the supplied chunk. It is used only
// when a reduced sink window cannot itself carry the lookahead chunk.
func (reader *Reader) hasMore(ctx context.Context, command ReadCommand, last Chunk) (bool, error) {
	after := chunkPosition{offset: last.Offset, sequence: last.Sequence}
	result, err := reader.fetchWindow(ctx, command, &after, 1)
	if err != nil {
		return false, err
	}
	return len(result.Chunks) > 0, nil
}

// fetchWindow reads at most window chunks strictly after the cursor pair.
func (reader *Reader) fetchWindow(ctx context.Context, command ReadCommand, after *chunkPosition, window int) (ReadResult, error) {
	request := ReadRequest{
		Scope:       command.Scope,
		ExecutionID: command.ExecutionID,
		ActionID:    command.ActionID,
		Stream:      command.Stream,
		Limit:       window,
	}
	if after == nil {
		result, err := reader.sink.Read(ctx, request)
		if err != nil {
			return ReadResult{}, mapSinkError(err)
		}
		return validateWindow(command, result, window, nil)
	}
	if pairSink, ok := reader.sink.(pairReader); ok {
		request.AfterOffset = after.offset
		result, err := pairSink.readAfterPair(ctx, request, after.sequence)
		if err != nil {
			return ReadResult{}, mapSinkError(err)
		}
		return validateWindow(command, result, window, after)
	}
	return reader.fetchOffsetWindow(ctx, command, request, after, window)
}

// fetchOffsetWindow serves a continuation through a sink that only accepts an
// offset lower bound. It re-reads from the start of the cursor's offset group
// and grows the window until the already-served prefix is smaller than the
// window, so a full window always contains window chunks strictly after the
// pair. It never treats the pre-filter chunk count as a page count.
func (reader *Reader) fetchOffsetWindow(ctx context.Context, command ReadCommand, request ReadRequest, after *chunkPosition, window int) (ReadResult, error) {
	lower := after.offset
	if lower > 0 {
		lower--
	}
	attempt := window
	largestAccepted := 0
	smallestRejected := 0
	for {
		request.AfterOffset = lower
		request.Limit = attempt
		result, err := reader.sink.Read(ctx, request)
		if err != nil {
			if attempt > window && isInvalidInput(err) {
				// The sink capped the window below this attempt. Search the
				// accepted range instead of treating the cap as unreadable.
				smallestRejected = attempt
				next, ok := nextWindow(largestAccepted, smallestRejected)
				if !ok {
					return ReadResult{}, fault.New(fault.DependencyUnavailable, "retained log sink window is too small to continue a shared-offset group")
				}
				attempt = next
				continue
			}
			return ReadResult{}, mapSinkError(err)
		}
		largestAccepted = attempt
		if len(result.Chunks) > attempt {
			return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned more chunks than requested")
		}
		if result.NextOffset < lower {
			return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned a regressing offset")
		}
		var previous chunkPosition
		for index, chunk := range result.Chunks {
			if chunk.ExecutionID != command.ExecutionID || chunk.ActionID != command.ActionID || chunk.Stream != command.Stream {
				return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned a chunk outside the requested stream")
			}
			if chunk.Offset < lower {
				return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned a chunk before the requested offset")
			}
			position := chunkPosition{offset: chunk.Offset, sequence: chunk.Sequence}
			if index > 0 && !position.after(previous) {
				return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned chunks out of order")
			}
			previous = position
		}
		filtered := make([]Chunk, 0, len(result.Chunks))
		for _, chunk := range result.Chunks {
			position := chunkPosition{offset: chunk.Offset, sequence: chunk.Sequence}
			if !position.after(*after) {
				continue
			}
			cloned := chunk
			cloned.Data = append([]byte(nil), chunk.Data...)
			filtered = append(filtered, cloned)
		}
		if len(filtered) > window {
			filtered = filtered[:window]
		}
		if len(filtered) >= window || len(result.Chunks) < attempt {
			result.Chunks = filtered
			return result, nil
		}
		if smallestRejected > 0 {
			next, ok := nextWindow(largestAccepted, smallestRejected)
			if !ok {
				return ReadResult{}, fault.New(fault.DependencyUnavailable, "retained log sink window is too small to continue a shared-offset group")
			}
			attempt = next
			continue
		}
		if attempt >= maxWindowProbe {
			return ReadResult{}, fault.New(fault.DependencyUnavailable, "retained log sink window is too small to continue a shared-offset group")
		}
		attempt *= 2
	}
}

// nextWindow picks the midpoint of the known-accepted and known-rejected
// windows. It reports false when no larger accepted window can exist.
func nextWindow(largestAccepted, smallestRejected int) (int, bool) {
	next := largestAccepted + (smallestRejected-largestAccepted)/2
	if next <= largestAccepted {
		return 0, false
	}
	return next, true
}

// validateWindow checks a sink page that is already known to be strictly after
// the cursor pair (or the first page) and clones its bytes.
func validateWindow(command ReadCommand, result ReadResult, window int, after *chunkPosition) (ReadResult, error) {
	if len(result.Chunks) > window {
		return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned more chunks than requested")
	}
	if after != nil && result.NextOffset < after.offset {
		return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned a regressing offset")
	}
	var previous chunkPosition
	cloned := make([]Chunk, 0, len(result.Chunks))
	for _, chunk := range result.Chunks {
		if chunk.ExecutionID != command.ExecutionID || chunk.ActionID != command.ActionID || chunk.Stream != command.Stream {
			return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned a chunk outside the requested stream")
		}
		position := chunkPosition{offset: chunk.Offset, sequence: chunk.Sequence}
		if after != nil && !position.after(*after) {
			return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned a chunk before the cursor")
		}
		if len(cloned) > 0 && !position.after(previous) {
			return ReadResult{}, fault.New(fault.InternalFailure, "retained log read returned chunks out of order")
		}
		previous = position
		copied := chunk
		copied.Data = append([]byte(nil), chunk.Data...)
		cloned = append(cloned, copied)
	}
	result.Chunks = cloned
	return result, nil
}

// mapSinkError keeps a public fault and wraps a private adapter cause without
// rendering it.
func mapSinkError(err error) error {
	if _, ok := fault.ClassOf(err); ok {
		return err
	}
	return fault.Wrap(fault.InternalFailure, "retained log read failed", err)
}

// isInvalidInput reports whether err is the stable invalid_input class.
func isInvalidInput(err error) bool {
	class, ok := fault.ClassOf(err)
	return ok && class == fault.InvalidInput
}

// encodeChunkPosition turns the last served pair into an opaque cursor.
func encodeChunkPosition(chunk Chunk) (string, error) {
	return api.EncodeCursor(strconv.FormatUint(chunk.Offset, 10) + ":" + strconv.FormatUint(chunk.Sequence, 10))
}

// scopeContains reports whether the owner scope covers the target scope. A
// tenant-scoped owner covers every project in its tenant; a project-scoped
// owner covers only that same project.
func scopeContains(owner, target tenancy.Scope) bool {
	if owner.TenantID == "" || target.TenantID == "" || owner.TenantID != target.TenantID {
		return false
	}
	if owner.ProjectID == "" {
		return true
	}
	return owner.ProjectID == target.ProjectID
}

// requireProjectScope validates a project-scoped read target.
func requireProjectScope(scope tenancy.Scope) error {
	if err := scope.Validate(); err != nil {
		return err
	}
	if !scope.ProjectScoped() {
		return fault.New(fault.InvalidInput, "execution.logs.read requires a project scope")
	}
	return nil
}

// checkActor verifies that the presented principal is structurally usable. A
// missing principal is unauthenticated; a malformed one is an invalid input.
func checkActor(actor authorize.Principal) error {
	if actor.ID == "" {
		return fault.New(fault.Unauthenticated, "log read requires a verified principal")
	}
	if err := actor.Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, "log read principal is invalid", err)
	}
	return nil
}

// validateReadTarget checks the retained stream identity before any sink read.
func validateReadTarget(command ReadCommand) error {
	if _, err := id.ParseExecutionID(string(command.ExecutionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "log read execution id is invalid", err)
	}
	if _, err := id.ParseActionID(string(command.ActionID)); err != nil {
		return fault.Wrap(fault.InvalidInput, "log read action id is invalid", err)
	}
	if !command.Stream.Valid() {
		return fault.New(fault.InvalidInput, fmt.Sprintf("log read stream %q is unknown", command.Stream))
	}
	return nil
}

// readBounds validates the page bound and decodes the opaque continuation
// cursor into the (offset, sequence) pair of the last served chunk. An empty
// cursor means the first page; a non-empty cursor resumes strictly after that
// pair, so a cursor naming an offset-zero chunk is a real continuation and not
// a second first page.
func readBounds(command ReadCommand) (int, *chunkPosition, error) {
	if command.Limit < 0 || command.Limit > MaxReadLimit {
		return 0, nil, fault.New(fault.InvalidInput, fmt.Sprintf("log read limit %d is outside 0..%d", command.Limit, MaxReadLimit))
	}
	limit := command.Limit
	if limit == 0 {
		limit = DefaultReadLimit
	}
	if command.Cursor == "" {
		return limit, nil, nil
	}
	sortKey, err := api.DecodeCursor(command.Cursor)
	if err != nil {
		return 0, nil, fault.Wrap(fault.InvalidInput, "log read cursor is invalid", err)
	}
	position, err := parseChunkPosition(sortKey)
	if err != nil {
		return 0, nil, fault.Wrap(fault.InvalidInput, "log read cursor is invalid", err)
	}
	return limit, &position, nil
}

// parseChunkPosition decodes the cursor's (offset, sequence) sort key.
func parseChunkPosition(sortKey string) (chunkPosition, error) {
	offsetText, sequenceText, ok := strings.Cut(sortKey, ":")
	if !ok {
		return chunkPosition{}, fmt.Errorf("cursor sort key has no offset/sequence separator")
	}
	offset, err := strconv.ParseUint(offsetText, 10, 64)
	if err != nil {
		return chunkPosition{}, fmt.Errorf("cursor offset is invalid: %w", err)
	}
	sequence, err := strconv.ParseUint(sequenceText, 10, 64)
	if err != nil {
		return chunkPosition{}, fmt.Errorf("cursor sequence is invalid: %w", err)
	}
	return chunkPosition{offset: offset, sequence: sequence}, nil
}
