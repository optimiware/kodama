package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
)

// defaultRescanInterval is the periodic current-request rescan period when the
// configuration does not supply one.
const defaultRescanInterval = 30 * time.Second

// requestInboxKind is the only served inbox kind a service adapter may answer.
// Waiting-task and failed-task items are scoped to human operators.
const requestInboxKind = "request"

// publicCodecs is the served operation codec table. Every adapter request
// and result crosses the public wire through the real codec, so the adapter can
// never invent a private vocabulary or accept an unknown field.
var publicCodecs = func() map[string]api.OperationCodec {
	codecs := make(map[string]api.OperationCodec, len(api.OperationCodecs()))
	for _, codec := range api.OperationCodecs() {
		codecs[codec.Operation] = codec
	}
	return codecs
}()

// NewAdapter validates a complete adapter configuration. It starts nothing.
func NewAdapter(config AdapterConfig) (*Adapter, error) {
	if !config.Kind.Valid() {
		return nil, fmt.Errorf("service kind %q is unknown", config.Kind)
	}
	if config.Kind == KindMergeCoordinator && config.Merges == nil {
		return nil, fmt.Errorf("merge coordinator dependency is required")
	}
	if _, err := id.ParsePrincipalID(string(config.ServiceID)); err != nil {
		return nil, fmt.Errorf("adapter service identity %q is invalid: %w", config.ServiceID, err)
	}
	if _, err := id.ParseTenantID(string(config.TenantID)); err != nil {
		return nil, fmt.Errorf("adapter tenant %q is invalid: %w", config.TenantID, err)
	}
	if _, err := id.ParseProjectID(string(config.ProjectID)); err != nil {
		return nil, fmt.Errorf("adapter project %q is invalid: %w", config.ProjectID, err)
	}
	if config.Handler == nil {
		return nil, fmt.Errorf("adapter handler is required")
	}
	if config.Events == nil {
		return nil, fmt.Errorf("adapter event source is required")
	}
	if config.Clock == nil {
		return nil, fmt.Errorf("adapter clock is required")
	}
	if config.RescanInterval < 0 {
		return nil, fmt.Errorf("adapter rescan interval is negative")
	}
	rescan := config.RescanInterval
	if rescan == 0 {
		rescan = defaultRescanInterval
	}
	if config.Kind == KindTimer && config.Schedule <= 0 {
		return nil, fmt.Errorf("timer adapter needs a positive schedule")
	}
	return &Adapter{
		kind:      config.Kind,
		serviceID: config.ServiceID,
		tenant:    config.TenantID,
		project:   config.ProjectID,
		handler:   config.Handler,
		events:    config.Events,
		clock:     config.Clock,
		schedule:  config.Schedule,
		rescan:    rescan,
		merges:    config.Merges,
	}, nil
}

// Kind reports the adapter kind.
func (adapter *Adapter) Kind() ServiceKind { return adapter.kind }

// Run performs a startup scan and then rescans periodically until ctx is done.
// Cancellation is a clean stop and returns nil. A startup scan failure is
// returned; a notification-drain failure is non-fatal because notifications are
// only hints, so Run keeps scanning while the event source is unavailable.
func (adapter *Adapter) Run(ctx context.Context) error {
	if err := adapter.RunOnce(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		var notificationFailure *eventSourceError
		if !errors.As(err, &notificationFailure) {
			return err
		}
	}
	ticker := time.NewTicker(adapter.rescan)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			// A periodic failure is retried by the next pass with the same
			// stable key; notifications are only hints.
			_ = adapter.RunOnce(ctx)
		}
	}
}

// RunOnce performs one notification drain and one paginated current-inbox scan.
// A drain failure never skips the scan: notifications are hints, and a retained
// request must still be resolved while the event source is unavailable. It
// processes every candidate even when an earlier one fails and returns the
// first scan error after the pass, otherwise the drain error.
func (adapter *Adapter) RunOnce(ctx context.Context) error {
	notificationErr := adapter.drainNotifications(ctx)
	var firstErr error
	// A merge assignment whose request resolution committed but whose response
	// was lost is no longer offered by the inbox. Converge it from the durable
	// assignment before scanning for current requests.
	if adapter.kind == KindMergeCoordinator {
		if err := adapter.reconcilePending(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	cursor := ""
	seen := make(map[string]bool)
	for {
		page, err := adapter.scanInbox(ctx, cursor)
		if err != nil {
			return err
		}
		for _, item := range page.Items {
			if !adapter.inScope(item) {
				continue
			}
			if err := adapter.consider(ctx, item); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		next := page.NextCursor
		if next == "" || seen[next] {
			break
		}
		seen[next] = true
		cursor = next
	}
	if firstErr != nil {
		return firstErr
	}
	if notificationErr != nil {
		return &eventSourceError{err: notificationErr}
	}
	return nil
}

// eventSourceError marks a notification-drain failure. Notifications never own
// an exclusive grant, so Run treats this class as retryable and keeps scanning.
type eventSourceError struct{ err error }

func (failure *eventSourceError) Error() string {
	return "event source: " + failure.err.Error()
}

func (failure *eventSourceError) Unwrap() error { return failure.err }

// drainNotifications polls every retained page. Notifications are hints only:
// they never cause a resolve and never grant assignment authority.
func (adapter *Adapter) drainNotifications(ctx context.Context) error {
	cursor := ""
	seen := make(map[string]bool)
	for {
		page, err := adapter.events.Poll(ctx, cursor)
		if err != nil {
			return err
		}
		next := page.Next.NextCursor
		if next == "" || next == cursor || seen[next] {
			return nil
		}
		seen[next] = true
		cursor = next
	}
}

// scanInbox requests one positive page of current request items through the
// served task.inbox operation.
func (adapter *Adapter) scanInbox(ctx context.Context, cursor string) (api.TaskInboxResult, error) {
	request := api.TaskInboxRequest{
		ProjectID: string(adapter.project),
		Kinds:     []string{requestInboxKind},
		Page:      &api.Page{Limit: api.DefaultPageLimit, Cursor: cursor},
	}
	decoded, err := adapter.invoke(ctx, OperationInbox, request, nil, "")
	if err != nil {
		return api.TaskInboxResult{}, err
	}
	page, ok := decoded.(api.TaskInboxResult)
	if !ok {
		return api.TaskInboxResult{}, fmt.Errorf("task.inbox returned an unexpected %T", decoded)
	}
	return page, nil
}

// invoke crosses the served public boundary with the public operation codec: the
// request is strictly encoded, the result operation identity is validated and
// the result is strictly decoded.
func (adapter *Adapter) invoke(ctx context.Context, operation string, request any, expectedVersion *uint64, idempotencyKey string) (any, error) {
	codec, ok := publicCodecs[operation]
	if !ok {
		return nil, fmt.Errorf("operation %q has no public codec", operation)
	}
	input, err := codec.EncodeRequest(request)
	if err != nil {
		return nil, fmt.Errorf("encode %s request: %w", operation, err)
	}
	result, err := adapter.handler.Invoke(ctx, api.Invocation{
		Operation: operation,
		Request: api.Request{
			Input:           input,
			ExpectedVersion: expectedVersion,
			IdempotencyKey:  idempotencyKey,
		},
	})
	if err != nil {
		return nil, err
	}
	if result.Operation != operation {
		return nil, fmt.Errorf("operation %q returned a result for %q", operation, result.Operation)
	}
	decoded, err := codec.DecodeResult(result.Body)
	if err != nil {
		return nil, fmt.Errorf("decode %s result: %w", operation, err)
	}
	return decoded, nil
}

// inScope reports whether the adapter may answer the inbox item at all: only an
// exact request assignment to its own service identity. A human role, a foreign
// service and a near-miss service id are filtered before any inspection.
func (adapter *Adapter) inScope(item api.InboxItem) bool {
	return item.Kind == requestInboxKind &&
		item.Assignment == "service/"+string(adapter.serviceID)
}

// consider revalidates one scanned request against current execution state and,
// when its action is still current, answers it with the current execution and
// action versions. The merge kind first maps and validates the typed assignment,
// so every malformed or out-of-scope request is refused before any effect.
func (adapter *Adapter) consider(ctx context.Context, scanned api.InboxItem) error {
	if adapter.kind == KindMergeCoordinator {
		// The inbox item's At is the action deadline. An elapsed deadline is
		// never answerable and must cause no effect.
		if !adapter.clock.Now().Before(scanned.At) {
			return nil
		}
	}
	current, err := adapter.inspect(ctx, scanned)
	if err != nil {
		var publicErr *api.Error
		if errors.As(err, &publicErr) && publicErr.Code == api.CodeNotFound {
			return nil
		}
		return err
	}
	if current.ExecutionID != scanned.ExecutionID || current.State != "running" {
		return nil
	}
	action, ok := currentAction(current, scanned.ActionID)
	if !ok {
		// The scanned action is no longer the current request.
		return nil
	}
	if action.Decision != "" || action.State != "running" {
		// The request is already answered or no longer answerable.
		return nil
	}
	if action.Version < scanned.Version {
		return fmt.Errorf("%w: current action version %d is behind scanned version %d", ErrStaleVersion, action.Version, scanned.Version)
	}
	if adapter.kind == KindMergeCoordinator {
		return adapter.considerMerge(ctx, scanned, current, action)
	}
	return adapter.resolve(ctx, scanned, current, action, nil)
}

// currentAction returns the inspected action with the exact scanned identity.
func currentAction(inspected api.ExecutionInspectResult, actionID string) (api.ExecutionAction, bool) {
	for _, action := range inspected.Actions {
		if action.ActionID == actionID {
			return action, true
		}
	}
	return api.ExecutionAction{}, false
}

// inspect revalidates one scanned request through the served execution.inspect
// operation.
func (adapter *Adapter) inspect(ctx context.Context, scanned api.InboxItem) (api.ExecutionInspectResult, error) {
	decoded, err := adapter.invoke(ctx, OperationInspect, api.ExecutionInspectRequest{
		ExecutionID: scanned.ExecutionID,
	}, nil, "")
	if err != nil {
		return api.ExecutionInspectResult{}, err
	}
	inspected, ok := decoded.(api.ExecutionInspectResult)
	if !ok {
		return api.ExecutionInspectResult{}, fmt.Errorf("execution.inspect returned an unexpected %T", decoded)
	}
	return inspected, nil
}

// resolve settles the current request with the current action version in the
// action-only body and the current execution version in the shared envelope.
// The responder and email adapters submit no response outputs: choosing their
// values is an explicit service-supplied response policy that is a separately
// specified decision boundary. The merge adapter supplies exactly the observed
// values its assignment declared; both paths never infer an output value.
func (adapter *Adapter) resolve(ctx context.Context, scanned api.InboxItem, current api.ExecutionInspectResult, action api.ExecutionAction, outputs map[string]string) error {
	request := api.ExecutionRequestResolveRequest{
		ExecutionID:           scanned.ExecutionID,
		ActionID:              scanned.ActionID,
		ExpectedActionVersion: action.Version,
		Decision:              DecisionPass,
		Outputs:               outputs,
	}
	expected := current.Version
	_, err := adapter.invoke(ctx, OperationResolve, request, &expected, adapter.idempotencyKey(scanned, current, action))
	if err == nil {
		return nil
	}
	var publicErr *api.Error
	if errors.As(err, &publicErr) {
		switch publicErr.Code {
		case api.CodeVersionConflict:
			return fmt.Errorf("%w: %v", ErrStaleVersion, err)
		case api.CodeChangedIdempotencyContent:
			return fmt.Errorf("%w: %v", ErrChangedContent, err)
		}
	}
	return err
}

// idempotencyKey derives the stable key for one scanned request. A timer keys by
// its truncated schedule window so two timers in the same window never collide;
// every kind keys by the exact execution identity and both observed versions, so
// redelivery reuses the key while a newer current version changes it.
func (adapter *Adapter) idempotencyKey(scanned api.InboxItem, current api.ExecutionInspectResult, action api.ExecutionAction) string {
	scope := strings.Join([]string{
		string(adapter.kind),
		scanned.ExecutionID,
		scanned.ActionID,
		strconv.FormatUint(current.Version, 10),
		strconv.FormatUint(action.Version, 10),
	}, ":")
	if adapter.kind == KindTimer {
		window := adapter.clock.Now().Truncate(adapter.schedule)
		scope = strconv.FormatInt(window.Unix(), 10) + ":" + scope
	}
	sum := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("%s:v%d:%s", adapter.kind, action.Version, hex.EncodeToString(sum[:])[:32])
}
