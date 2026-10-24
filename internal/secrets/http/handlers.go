// Package secretshttp is the management HTTP adapter of the variables and
// secrets module: routes on net/http through httpapi.Router, strict bounded
// JSON, ETag/If-Match, scoped cursors and metadata-only responses. Secret values
// are accepted only in the write-only `value` member of the create and
// new-version requests and appear in no response, error or log.
//
// The package depends on the secrets use cases and the shared httpapi
// foundation; it holds no pgx types. The keyed (idempotent) transaction is an
// injected port implemented by internal/secrets/postgres. Authentication,
// authorization actions, rate limits and the router are injected by the
// composition root (T-027); nothing here grants access.
package secretshttp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/secrets/envelope"
)

// Module-defined error codes (documented in api/configuration.yaml).
const (
	// CodeNameConflict: the name is, or was, used in the project.
	CodeNameConflict domain.ErrorCode = "name_conflict"
	// CodeResourceDeleted: the resource is logically deleted.
	CodeResourceDeleted domain.ErrorCode = "resource_deleted"
)

// Authorization resource kinds passed to the Authorizer.
const (
	ResourceVariable = "variable"
	ResourceSecret   = "secret"
)

// Actions are the authorization action identifiers of the module's routes. The
// composition root maps them to its role policy; administrative authority over
// secrets never implies plaintext read, which no route here offers.
type Actions struct {
	VariableList, VariableRead, VariableCreate, VariableUpdate, VariableDelete httpapi.Action
	SecretList, SecretRead, SecretCreate, SecretDelete                         httpapi.Action
	VersionList, VersionCreate, VersionRevoke                                  httpapi.Action
}

// DefaultActions returns the documented action identifiers.
func DefaultActions() Actions {
	return Actions{
		VariableList: "variable.list", VariableRead: "variable.read", VariableCreate: "variable.create",
		VariableUpdate: "variable.update", VariableDelete: "variable.delete",
		SecretList: "secret.list", SecretRead: "secret.read", SecretCreate: "secret.create", SecretDelete: "secret.delete",
		VersionList: "secret.version.list", VersionCreate: "secret.version.create", VersionRevoke: "secret.version.revoke",
	}
}

func (a Actions) all() []httpapi.Action {
	return []httpapi.Action{
		a.VariableList, a.VariableRead, a.VariableCreate, a.VariableUpdate, a.VariableDelete,
		a.SecretList, a.SecretRead, a.SecretCreate, a.SecretDelete, a.VersionList, a.VersionCreate, a.VersionRevoke,
	}
}

// Keyed runs a management write exactly once for its idempotency identity,
// atomically with the keyed response (implemented by postgres.Keyed). A replay
// is authorized again through authorizeReplay and never runs do.
type Keyed interface {
	Execute(ctx context.Context, id httpapi.RequestIdentity, authorizeReplay func(ctx context.Context, ref httpapi.Reference) error,
		do func(ctx context.Context, w secrets.Writer) (httpapi.Outcome, error)) (response httpapi.StoredResponse, replayed bool, err error)
}

// Config wires a Handler.
type Config struct {
	Service *secrets.Service
	Keyed   Keyed
	// Cursors signs list continuation tokens.
	Cursors *httpapi.CursorCodec
	// HashKey (at least 32 bytes, kept outside the database) keys the
	// idempotency fingerprint of requests that carry a secret value. Without it
	// the stored request hash would be a plain SHA-256 of a body containing the
	// value, an offline-guessable fingerprint of the secret.
	HashKey []byte
	// Actions defaults to DefaultActions when every member is empty.
	Actions Actions
	// Clock defaults to the UTC wall clock.
	Clock func() time.Time
}

// Handler serves the management routes.
type Handler struct {
	svc     *secrets.Service
	keyed   Keyed
	cursors *httpapi.CursorCodec
	hashKey []byte
	actions Actions
	clock   func() time.Time
}

// NewHandler validates cfg.
func NewHandler(cfg Config) (*Handler, error) {
	if cfg.Service == nil || cfg.Keyed == nil || cfg.Cursors == nil {
		return nil, errors.New("secretshttp: Service, Keyed and Cursors are required")
	}
	if len(cfg.HashKey) < 32 {
		return nil, errors.New("secretshttp: HashKey must be at least 32 bytes")
	}
	h := &Handler{svc: cfg.Service, keyed: cfg.Keyed, cursors: cfg.Cursors, hashKey: append([]byte(nil), cfg.HashKey...), actions: cfg.Actions, clock: cfg.Clock}
	if h.actions == (Actions{}) {
		h.actions = DefaultActions()
	}
	for _, a := range h.actions.all() {
		if a == "" {
			return nil, errors.New("secretshttp: every authorization action must be set")
		}
	}
	if h.clock == nil {
		h.clock = func() time.Time { return time.Now().UTC() }
	}
	return h, nil
}

const base = "/tenants/{tenantId}/projects/{projectId}"

// Route is one registered operation, exposed so tests and the integration
// owner can compare the table with api/configuration.yaml.
type Route struct {
	Method, Path string
	Operation    string
}

// Routes returns the route table in registration order.
func (h *Handler) Routes() []Route {
	var out []Route
	for _, r := range h.table() {
		out = append(out, Route{Method: r.route.Method, Path: r.route.Path, Operation: r.operation})
	}
	return out
}

type tableEntry struct {
	operation string
	route     httpapi.Route
}

func (h *Handler) table() []tableEntry {
	a := h.actions
	return []tableEntry{
		{"listVariables", httpapi.Route{Method: "GET", Path: base + "/variables", Action: a.VariableList, Handler: h.listVariables}},
		{"createVariable", httpapi.Route{Method: "POST", Path: base + "/variables", Action: a.VariableCreate, Handler: h.createVariable}},
		{"getVariable", httpapi.Route{Method: "GET", Path: base + "/variables/{id}", Action: a.VariableRead, ResourceKind: ResourceVariable, ResourceParam: "id", Handler: h.getVariable}},
		{"updateVariable", httpapi.Route{Method: "PATCH", Path: base + "/variables/{id}", Action: a.VariableUpdate, ResourceKind: ResourceVariable, ResourceParam: "id", Handler: h.updateVariable}},
		{"deleteVariable", httpapi.Route{Method: "DELETE", Path: base + "/variables/{id}", Action: a.VariableDelete, ResourceKind: ResourceVariable, ResourceParam: "id", Handler: h.deleteVariable}},
		{"listSecrets", httpapi.Route{Method: "GET", Path: base + "/secrets", Action: a.SecretList, Handler: h.listSecrets}},
		{"createSecret", httpapi.Route{Method: "POST", Path: base + "/secrets", Action: a.SecretCreate, Handler: h.createSecret}},
		{"getSecret", httpapi.Route{Method: "GET", Path: base + "/secrets/{id}", Action: a.SecretRead, ResourceKind: ResourceSecret, ResourceParam: "id", Handler: h.getSecret}},
		{"deleteSecret", httpapi.Route{Method: "DELETE", Path: base + "/secrets/{id}", Action: a.SecretDelete, ResourceKind: ResourceSecret, ResourceParam: "id", Handler: h.deleteSecret}},
		{"listSecretVersions", httpapi.Route{Method: "GET", Path: base + "/secrets/{id}/versions", Action: a.VersionList, ResourceKind: ResourceSecret, ResourceParam: "id", Handler: h.listVersions}},
		{"createSecretVersion", httpapi.Route{Method: "POST", Path: base + "/secrets/{id}/versions", Action: a.VersionCreate, ResourceKind: ResourceSecret, ResourceParam: "id", Handler: h.createVersion}},
		{"revokeSecretVersion", httpapi.Route{Method: "POST", Path: base + "/secrets/{id}/versions/{version}/revoke", Action: a.VersionRevoke, ResourceKind: ResourceSecret, ResourceParam: "id", Handler: h.revokeVersion}},
	}
}

// RegisterRoutes registers every route on rt. The router's prefix (for example
// /api/v1) is applied by rt. Registration errors are returned, never panicked.
func (h *Handler) RegisterRoutes(rt *httpapi.Router) error {
	for _, e := range h.table() {
		if err := rt.Handle(e.route); err != nil {
			return fmt.Errorf("secretshttp: register %s: %w", e.operation, err)
		}
	}
	return nil
}

// ---- helpers ------------------------------------------------------------------

func (h *Handler) mutation(c *httpapi.Call, reason string) secrets.Mutation {
	return secrets.Mutation{
		Actor:     secrets.Actor{Type: secrets.ActorUser, ID: actorID(c.Principal.ID)},
		RequestID: c.RequestID, Reason: reason, Now: h.clock(),
	}
}

// actorID bounds a principal identifier to the audit column: a longer one is
// truncated and suffixed with a digest of the full value, so it stays unique.
func actorID(id string) string {
	if len(id) <= 256 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return id[:200] + "#" + hex.EncodeToString(sum[:8])
}

// protectHash replaces the idempotency fingerprint of a value-bearing request
// with a keyed one (HMAC-SHA-256), so the stored hash cannot be used to guess the
// value offline. Equal requests still hash equally, which is all idempotency
// needs.
func (h *Handler) protectHash(id *httpapi.RequestIdentity) {
	mac := hmac.New(sha256.New, h.hashKey)
	mac.Write([]byte("kodama-secrets-request-v1"))
	mac.Write(id.Hash[:])
	copy(id.Hash[:], mac.Sum(nil))
}

// childPath is the path of a created resource under the request's collection.
// It follows the router's prefix instead of assuming one.
func childPath(r *http.Request, id string) string {
	return strings.TrimSuffix(r.URL.Path, "/") + "/" + id
}

func etag(v domain.Version) map[string]string {
	t, _ := httpapi.FormatETag(v)
	return map[string]string{"ETag": t}
}

func writeVersioned(w http.ResponseWriter, r *http.Request, status int, v domain.Version, body any) {
	_ = httpapi.SetETag(w, v)
	httpapi.WriteJSON(w, r, status, body)
}

// fail renders err through the module's mapping.
func fail(w http.ResponseWriter, r *http.Request, err error) {
	httpapi.WriteError(w, r, mapError(err))
}

// mapError converts module errors to their transport form. It never echoes
// request content: messages come from fixed texts or the module's own
// validation messages, which name rules and never values.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	var api *httpapi.APIError
	if errors.As(err, &api) {
		return err
	}
	var conflict *secrets.ConflictError
	if errors.As(err, &conflict) {
		return httpapi.VersionConflictError()
	}
	var ke *secrets.KeyError
	if errors.As(err, &ke) {
		e := httpapi.UnavailableError(time.Second, err)
		e.Message = "the key provider could not complete the operation"
		e.Details = map[string]any{"keyProvider": string(ke.Class), "retryable": ke.Retryable()}
		if !ke.Retryable() {
			e.Header = nil
		}
		return e
	}
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return httpapi.NotFoundError()
	case errors.Is(err, secrets.ErrNameTaken):
		return httpapi.NewError(http.StatusConflict, CodeNameConflict, "the name is, or was, used in this project")
	case errors.Is(err, secrets.ErrDeleted):
		return httpapi.NewError(http.StatusConflict, CodeResourceDeleted, "the resource is deleted")
	case errors.Is(err, secrets.ErrBackpressure), errors.Is(err, secrets.ErrUnavailable):
		return httpapi.UnavailableError(time.Second, err)
	case errors.Is(err, secrets.ErrInvalid), errors.Is(err, domain.ErrInvalidValue):
		return httpapi.BadRequestError(validationMessage(err))
	}
	return err
}

func validationMessage(err error) string {
	msg := err.Error()
	for _, p := range []string{"secrets: invalid: ", "invalid value: "} {
		if i := strings.Index(msg, p); i >= 0 {
			msg = msg[i+len(p):]
		}
	}
	if len(msg) > 256 {
		msg = msg[:256]
	}
	return msg
}

// ---- views -----------------------------------------------------------------------

func timePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func variableView(v secrets.Variable) Variable {
	return Variable{
		Id: v.ID(), Name: v.Name(), Type: VariableType(v.Type()), Value: v.Value(), ValueVersion: int64(v.ValueVersion()),
		Description: v.Description(), Version: int64(v.Version()),
		CreatedAt: v.CreatedAt().UTC(), UpdatedAt: v.UpdatedAt().UTC(), DeletedAt: timePtr(v.DeletedAt()),
	}
}

func retentionView(r secrets.RetentionReport) RetentionReport {
	return RetentionReport{
		PinnedVersions: r.PinnedVersions, ActivePins: r.ActivePins, PendingConsumerHandling: r.PendingHandling,
		BackupRetention: RetentionReportBackupRetention(r.BackupRetention),
	}
}

func secretView(s secrets.Secret) Secret {
	return Secret{
		Id: s.ID(), Name: s.Name(), Kind: SecretKind(s.Kind()), Ref: s.Ref(), CurrentVersion: s.CurrentVersion(),
		Version: int64(s.Version()), CreatedAt: s.CreatedAt().UTC(), UpdatedAt: s.UpdatedAt().UTC(), DeletedAt: timePtr(s.DeletedAt()),
	}
}

func versionView(v secrets.SecretVersion) SecretVersion {
	rec := v.Record()
	return SecretVersion{
		SecretId: rec.SecretID, Version: rec.Version, Algorithm: rec.Envelope.Algorithm, KeyGeneration: int(rec.KeyGeneration),
		CreatedAt: rec.CreatedAt.UTC(), CreatedBy: rec.CreatedBy, RevokedAt: timePtr(v.RevokedAt()),
	}
}

// ---- variables -------------------------------------------------------------------

func (h *Handler) listVariables(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	cs := c.CursorScope(nil, "name")
	req, err := h.cursors.ParsePage(r, cs)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	rows, err := h.svc.Reader().ListVariables(r.Context(), scope, string(req.Position), req.FetchLimit())
	if err != nil {
		fail(w, r, err)
		return
	}
	views := make([]Variable, 0, len(rows))
	for _, v := range rows {
		views = append(views, variableView(v))
	}
	page, err := httpapi.BuildPage(h.cursors, cs, req, views, func(v Variable) ([]byte, error) { return []byte(v.Name), nil })
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, page)
}

func (h *Handler) createVariable(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	id, err := c.Identity(r, body)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var in CreateVariableRequest
	if err := httpapi.DecodeJSONBytes(body, &in); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, "")
	desc := ""
	if in.Description != nil {
		desc = *in.Description
	}
	resp, replayed, err := h.keyed.Execute(r.Context(), id, c.AuthorizeReference, func(ctx context.Context, wr secrets.Writer) (httpapi.Outcome, error) {
		v, err := h.svc.CreateVariable(ctx, wr, m, secrets.CreateVariableInput{Scope: scope, Name: in.Name, Value: in.Value, Description: desc})
		if err != nil {
			return httpapi.Outcome{}, err
		}
		hdr := etag(v.Version())
		hdr["Location"] = childPath(r, v.ID())
		sr, err := httpapi.NewStoredResponse(r, http.StatusCreated, variableView(v), hdr)
		return httpapi.Outcome{Response: sr, Target: httpapi.Reference{Kind: ResourceVariable, ID: v.ID()}}, err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
}

func (h *Handler) getVariable(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	v, err := h.svc.Reader().GetVariable(r.Context(), scope, c.Target.Resource.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	writeVersioned(w, r, http.StatusOK, v.Version(), variableView(v))
}

func (h *Handler) updateVariable(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var in UpdateVariableRequest
	if err := httpapi.DecodeJSON(w, r, &in); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, "")
	var out secrets.Variable
	err = h.svc.Atomically(r.Context(), scope.TenantID, func(ctx context.Context, wr secrets.Writer) error {
		var err error
		out, err = h.svc.EditVariable(ctx, wr, m, scope, c.Target.Resource.ID, expected, secrets.VariableEdit{Value: in.Value, Description: in.Description})
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeVersioned(w, r, http.StatusOK, out.Version(), variableView(out))
}

func (h *Handler) deleteVariable(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, "")
	err = h.svc.Atomically(r.Context(), scope.TenantID, func(ctx context.Context, wr secrets.Writer) error {
		_, err := h.svc.DeleteVariable(ctx, wr, m, scope, c.Target.Resource.ID, expected)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// ---- secrets ---------------------------------------------------------------------

func (h *Handler) listSecrets(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	cs := c.CursorScope(nil, "name")
	req, err := h.cursors.ParsePage(r, cs)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	rows, err := h.svc.Reader().ListSecrets(r.Context(), scope, string(req.Position), req.FetchLimit())
	if err != nil {
		fail(w, r, err)
		return
	}
	views := make([]Secret, 0, len(rows))
	for _, s := range rows {
		views = append(views, secretView(s))
	}
	page, err := httpapi.BuildPage(h.cursors, cs, req, views, func(s Secret) ([]byte, error) { return []byte(s.Name), nil })
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, page)
}

// decodeValue extracts the write-only value. The caller clears the returned
// bytes after use.
func decodeValue(v *[]byte) ([]byte, error) {
	if v == nil || len(*v) == 0 {
		return nil, httpapi.BadRequestError("a non-empty value is required")
	}
	if len(*v) > envelope.MaxPlaintextBytes {
		return nil, httpapi.BadRequestError(fmt.Sprintf("the value must be at most %d bytes", envelope.MaxPlaintextBytes))
	}
	return *v, nil
}

func (h *Handler) createSecret(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	defer clear(body)
	id, err := c.Identity(r, body)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	h.protectHash(&id)
	var in CreateSecretRequest
	if err := httpapi.DecodeJSONBytes(body, &in); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	value, err := decodeValue(in.Value)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	defer clear(value)
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, "")
	// Encryption and every key-provider call happen before the transaction. A
	// preparation failure is reported from inside the keyed unit, so a replay of
	// an earlier success is still served (a replay never depends on the current
	// state or on the key provider).
	prepared, prepErr := h.svc.PrepareSecret(r.Context(), m, scope, in.Name, secrets.SecretKind(in.Kind), value)
	resp, replayed, err := h.keyed.Execute(r.Context(), id, c.AuthorizeReference, func(ctx context.Context, wr secrets.Writer) (httpapi.Outcome, error) {
		if prepErr != nil {
			return httpapi.Outcome{}, prepErr
		}
		s, err := h.svc.CreateSecret(ctx, wr, m, prepared)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		hdr := etag(s.Version())
		hdr["Location"] = childPath(r, s.ID())
		sr, err := httpapi.NewStoredResponse(r, http.StatusCreated, secretView(s), hdr)
		return httpapi.Outcome{Response: sr, Target: httpapi.Reference{Kind: ResourceSecret, ID: s.ID()}}, err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
}

func (h *Handler) getSecret(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	s, err := h.svc.Reader().GetSecret(r.Context(), scope, c.Target.Resource.ID)
	if err != nil {
		fail(w, r, err)
		return
	}
	view := secretView(s)
	if s.Deleted() {
		rep, err := h.svc.Reader().RetentionReport(r.Context(), scope, s.ID())
		if err != nil {
			fail(w, r, err)
			return
		}
		ret := retentionView(rep)
		view.Retention = &ret
	}
	writeVersioned(w, r, http.StatusOK, s.Version(), view)
}

func (h *Handler) deleteSecret(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, "")
	var (
		s   secrets.Secret
		rep secrets.RetentionReport
	)
	err = h.svc.Atomically(r.Context(), scope.TenantID, func(ctx context.Context, wr secrets.Writer) error {
		var err error
		s, rep, err = h.svc.DeleteSecret(ctx, wr, m, scope, c.Target.Resource.ID, expected)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	ret := retentionView(rep)
	view := secretView(s)
	view.Retention = &ret
	writeVersioned(w, r, http.StatusOK, s.Version(), DeleteSecretResult{Secret: view, Retention: ret})
}

// ---- versions --------------------------------------------------------------------

func (h *Handler) listVersions(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	if err := httpapi.CheckQuery(r); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	cs := c.CursorScope(nil, "version")
	req, err := h.cursors.ParsePage(r, cs)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	var after int64
	if req.Position != nil {
		after, err = strconv.ParseInt(string(req.Position), 10, 64)
		if err != nil || after < 0 {
			httpapi.WriteError(w, r, httpapi.NewError(http.StatusBadRequest, httpapi.CodeInvalidCursor, "the cursor is not valid"))
			return
		}
	}
	secretID := c.Target.Resource.ID
	if _, err := h.svc.Reader().GetSecret(r.Context(), scope, secretID); err != nil {
		fail(w, r, err)
		return
	}
	rows, err := h.svc.Reader().ListSecretVersions(r.Context(), scope, secretID, after, req.FetchLimit())
	if err != nil {
		fail(w, r, err)
		return
	}
	views := make([]SecretVersion, 0, len(rows))
	for _, v := range rows {
		views = append(views, versionView(v))
	}
	page, err := httpapi.BuildPage(h.cursors, cs, req, views, func(v SecretVersion) ([]byte, error) {
		return []byte(strconv.FormatInt(v.Version, 10)), nil
	})
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	httpapi.WriteJSON(w, r, http.StatusOK, page)
}

func (h *Handler) createVersion(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	expected, err := httpapi.RequireIfMatch(r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	body, err := httpapi.ReadBody(w, r)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	defer clear(body)
	id, err := c.Identity(r, body)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	h.protectHash(&id)
	var in CreateSecretVersionRequest
	if err := httpapi.DecodeJSONBytes(body, &in); err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	value, err := decodeValue(in.Value)
	if err != nil {
		httpapi.WriteError(w, r, err)
		return
	}
	defer clear(value)
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, "")
	secretID := c.Target.Resource.ID
	prepared, prepErr := h.svc.PrepareVersion(r.Context(), m, scope, secretID, expected, value)
	resp, replayed, err := h.keyed.Execute(r.Context(), id, c.AuthorizeReference, func(ctx context.Context, wr secrets.Writer) (httpapi.Outcome, error) {
		if prepErr != nil {
			return httpapi.Outcome{}, prepErr
		}
		s, err := h.svc.AddVersion(ctx, wr, m, prepared)
		if err != nil {
			return httpapi.Outcome{}, err
		}
		hdr := etag(s.Version())
		hdr["Location"] = r.URL.Path
		sr, err := httpapi.NewStoredResponse(r, http.StatusCreated, SecretVersionResult{Secret: secretView(s), Version: versionView(prepared.Version)}, hdr)
		return httpapi.Outcome{Response: sr, Target: httpapi.Reference{Kind: ResourceSecret, ID: s.ID()}}, err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	httpapi.WriteStored(w, r, resp, replayed)
}

func (h *Handler) revokeVersion(w http.ResponseWriter, r *http.Request, c *httpapi.Call) {
	version, err := strconv.ParseInt(r.PathValue("version"), 10, 64)
	if err != nil || version < 1 || version > domain.MaxSafeInteger || strconv.FormatInt(version, 10) != r.PathValue("version") {
		httpapi.WriteError(w, r, httpapi.BadRequestError("the version must be a positive integer"))
		return
	}
	var in RevokeSecretVersionRequest
	if r.ContentLength != 0 { // the body is optional
		if err := httpapi.DecodeJSON(w, r, &in); err != nil {
			httpapi.WriteError(w, r, err)
			return
		}
	}
	reason := ""
	if in.Reason != nil {
		reason = *in.Reason
	}
	scope, err := c.Scope()
	if err != nil {
		fail(w, r, err)
		return
	}
	m := h.mutation(c, reason)
	var res secrets.RevocationResult
	err = h.svc.Atomically(r.Context(), scope.TenantID, func(ctx context.Context, wr secrets.Writer) error {
		var err error
		res, err = h.svc.RevokeVersion(ctx, wr, m, scope, c.Target.Resource.ID, version)
		return err
	})
	if err != nil {
		fail(w, r, err)
		return
	}
	writeVersioned(w, r, http.StatusOK, res.Secret.Version(), RevokeSecretVersionResult{
		Secret: secretView(res.Secret), Version: versionView(res.Version),
		ConsumerHandling: ConsumerHandlingSummary{Total: len(res.Handling), Pending: res.PendingHandlingCount()},
	})
}
