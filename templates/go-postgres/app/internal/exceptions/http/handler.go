// Package exhttp is the application's versioned HTTP interface for inventory
// exceptions (see api/openapi.yaml). It authenticates a bearer token, resolves
// the caller's current membership for the requested tenant, and delegates to the
// exceptions use cases. It trusts no tenant or role header.
package exhttp

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"example.invalid/inventory/internal/exceptions"
	"example.invalid/inventory/internal/identity"
	"example.invalid/inventory/internal/tenancy"
)

// Authenticator verifies a bearer token (consumer-owned port).
type Authenticator interface {
	Verify(ctx context.Context, bearer string) (identity.Principal, error)
}

// Tenants resolves a verified subject and requested tenant into Access.
type Tenants interface {
	Resolve(ctx context.Context, subject, requestedTenant string) (tenancy.Access, error)
}

// Service is the use-case surface the transport needs.
type Service interface {
	Create(ctx context.Context, a tenancy.Access, in exceptions.CreateInput) (exceptions.Exception, error)
	Get(ctx context.Context, a tenancy.Access, id exceptions.ID) (exceptions.Exception, error)
	List(ctx context.Context, a tenancy.Access, q exceptions.ListQuery) (exceptions.Page, error)
	History(ctx context.Context, a tenancy.Access, id exceptions.ID) ([]exceptions.AuditEntry, error)
	Resolve(ctx context.Context, a tenancy.Access, id exceptions.ID, adjusted int64, reason, key string, expectedVersion int64) (exceptions.Receipt, error)
}

// Config wires the handler. Every collaborator is required.
type Config struct {
	Auth      Authenticator
	Tenants   Tenants
	Service   Service
	CursorKey []byte
	// Ready reports whether required dependencies are usable. Liveness never calls it.
	Ready  func(ctx context.Context) error
	Logger *slog.Logger
	// Build identifies what is running (deployment, configuration digest, image
	// digest, source commit). It is nonsecret and lets delivery tests prove they
	// inspect the intended deployment.
	Build map[string]string
}

// Handler serves the application routes.
type Handler struct {
	cfg Config
	log *slog.Logger
}

// New fails closed on a missing collaborator or a weak cursor key.
func New(cfg Config) (*Handler, error) {
	switch {
	case cfg.Auth == nil, cfg.Tenants == nil, cfg.Service == nil, cfg.Ready == nil:
		return nil, errors.New("exhttp: authenticator, tenants, service and readiness are required")
	case len(cfg.CursorKey) < 32:
		return nil, errors.New("exhttp: the cursor key must be at least 32 bytes")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cfg: cfg, log: log}, nil
}

// Routes returns the mux.
func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /readyz", h.readyz)
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, h.cfg.Build) })
	const base = "/api/v1/tenants/{tenantId}/exceptions"
	mux.HandleFunc("POST "+base, h.wrap(h.create))
	mux.HandleFunc("GET "+base, h.wrap(h.list))
	mux.HandleFunc("GET "+base+"/{id}", h.wrap(h.get))
	mux.HandleFunc("GET "+base+"/{id}/history", h.wrap(h.history))
	mux.HandleFunc("POST "+base+"/{id}/resolution", h.wrap(h.resolve))
	return h.correlate(mux)
}

type ctxKey struct{}

var reqIDRE = regexp.MustCompile(`^[A-Za-z0-9._-]{8,64}$`)

func (h *Handler) correlate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if !reqIDRE.MatchString(id) {
			var b [12]byte
			_, _ = rand.Read(b[:])
			id = "req-" + hex.EncodeToString(b[:])
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, id)))
	})
}

func requestID(r *http.Request) string { id, _ := r.Context().Value(ctxKey{}).(string); return id }

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	if err := h.cfg.Ready(ctx); err != nil {
		h.log.Warn("not ready", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type handlerFunc func(w http.ResponseWriter, r *http.Request, a tenancy.Access)

// wrap authenticates, resolves the requested tenant against current
// membership, and only then calls the operation.
func (h *Handler) wrap(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		scheme, token, ok := strings.Cut(auth, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			h.fail(w, r, errUnauthenticated)
			return
		}
		p, err := h.cfg.Auth.Verify(r.Context(), strings.TrimSpace(token))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		a, err := h.cfg.Tenants.Resolve(r.Context(), p.Subject, r.PathValue("tenantId"))
		if err != nil {
			h.fail(w, r, err)
			return
		}
		fn(w, r, a)
	}
}

var errUnauthenticated = identity.ErrUnauthenticated

// ---- representations ----

type resolutionJSON struct {
	OperationKey     string `json:"operationKey"`
	AdjustedQuantity int64  `json:"adjustedQuantity"`
	Reason           string `json:"reason"`
	RequestedBy      string `json:"requestedBy"`
	ERPReference     string `json:"erpReference,omitempty"`
	Detail           string `json:"detail,omitempty"`
}

type exceptionJSON struct {
	ID              string          `json:"id"`
	TenantID        string          `json:"tenantId"`
	ERPRef          string          `json:"erpRef"`
	Description     string          `json:"description"`
	ERPQuantity     int64           `json:"erpQuantity"`
	CountedQuantity int64           `json:"countedQuantity"`
	State           string          `json:"state"`
	Version         int64           `json:"version"`
	CreatedBy       string          `json:"createdBy"`
	CreatedAt       time.Time       `json:"createdAt"`
	UpdatedAt       time.Time       `json:"updatedAt"`
	Resolution      *resolutionJSON `json:"resolution"`
}

func toJSON(e exceptions.Exception) exceptionJSON {
	out := exceptionJSON{ID: string(e.ID), TenantID: string(e.Tenant), ERPRef: e.ERPRef, Description: e.Description, ERPQuantity: e.ERPQuantity,
		CountedQuantity: e.CountedQuantity, State: string(e.State), Version: e.Version, CreatedBy: e.CreatedBy, CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt}
	if r := e.Resolution; r != nil {
		out.Resolution = &resolutionJSON{OperationKey: r.OperationKey, AdjustedQuantity: r.AdjustedQuantity, Reason: r.Reason, RequestedBy: r.RequestedBy, ERPReference: r.ERPReference, Detail: r.Detail}
	}
	return out
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

// ---- operations ----

const maxBody = 64 << 10

func decode(r *http.Request, dst any) error {
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		return fmt.Errorf("%w: Content-Type must be application/json", exceptions.ErrInvalid)
	}
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("%w: malformed JSON body: %v", exceptions.ErrInvalid, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unexpected data after the JSON body", exceptions.ErrInvalid)
	}
	return nil
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request, a tenancy.Access) {
	var in struct {
		ERPRef          string `json:"erpRef"`
		Description     string `json:"description"`
		ERPQuantity     *int64 `json:"erpQuantity"`
		CountedQuantity *int64 `json:"countedQuantity"`
	}
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if in.ERPQuantity == nil || in.CountedQuantity == nil {
		h.fail(w, r, fmt.Errorf("%w: erpQuantity and countedQuantity are required", exceptions.ErrInvalid))
		return
	}
	e, err := h.cfg.Service.Create(r.Context(), a, exceptions.CreateInput{ERPRef: in.ERPRef, Description: in.Description, ERPQuantity: *in.ERPQuantity, CountedQuantity: *in.CountedQuantity})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(e.Version))
	w.Header().Set("Location", fmt.Sprintf("/api/v1/tenants/%s/exceptions/%s", e.Tenant, e.ID))
	writeJSON(w, http.StatusCreated, toJSON(e))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, a tenancy.Access) {
	id, err := exceptions.ParseID(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, exceptions.ErrNotFound)
		return
	}
	e, err := h.cfg.Service.Get(r.Context(), a, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(e.Version))
	writeJSON(w, http.StatusOK, toJSON(e))
}

func (h *Handler) history(w http.ResponseWriter, r *http.Request, a tenancy.Access) {
	id, err := exceptions.ParseID(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, exceptions.ErrNotFound)
		return
	}
	entries, err := h.cfg.Service.History(r.Context(), a, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	type entry struct {
		Seq    int64     `json:"seq"`
		Actor  string    `json:"actor"`
		Action string    `json:"action"`
		Detail string    `json:"detail"`
		At     time.Time `json:"at"`
	}
	out := make([]entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, entry{Seq: e.Seq, Actor: e.Actor, Action: e.Action, Detail: e.Detail, At: e.At})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request, a tenancy.Access) {
	q := r.URL.Query()
	for k := range q {
		if k != "state" && k != "limit" && k != "cursor" {
			h.fail(w, r, fmt.Errorf("%w: unknown query parameter %q", exceptions.ErrInvalid, k))
			return
		}
	}
	lq := exceptions.ListQuery{State: exceptions.State(q.Get("state")), Limit: 50}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			h.fail(w, r, fmt.Errorf("%w: limit must be 1..100", exceptions.ErrInvalid))
			return
		}
		lq.Limit = n
	}
	if c := q.Get("cursor"); c != "" {
		pos, err := h.decodeCursor(c, a.Tenant(), lq.State)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		lq.After = pos
	}
	page, err := h.cfg.Service.List(r.Context(), a, lq)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	items := make([]exceptionJSON, 0, len(page.Items))
	for _, e := range page.Items {
		items = append(items, toJSON(e))
	}
	var next *string
	if page.Next != nil {
		c := h.encodeCursor(a.Tenant(), lq.State, *page.Next)
		next = &c
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
}

func (h *Handler) resolve(w http.ResponseWriter, r *http.Request, a tenancy.Access) {
	id, err := exceptions.ParseID(r.PathValue("id"))
	if err != nil {
		h.fail(w, r, exceptions.ErrNotFound)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		h.fail(w, r, fmt.Errorf("%w: the Idempotency-Key header is required", exceptions.ErrInvalid))
		return
	}
	match := strings.Trim(strings.TrimSpace(r.Header.Get("If-Match")), `"`)
	version, err := strconv.ParseInt(match, 10, 64)
	if err != nil || version < 1 {
		h.fail(w, r, errPreconditionRequired)
		return
	}
	var in struct {
		AdjustedQuantity *int64 `json:"adjustedQuantity"`
		Reason           string `json:"reason"`
	}
	if err := decode(r, &in); err != nil {
		h.fail(w, r, err)
		return
	}
	if in.AdjustedQuantity == nil {
		h.fail(w, r, fmt.Errorf("%w: adjustedQuantity is required", exceptions.ErrInvalid))
		return
	}
	rec, err := h.cfg.Service.Resolve(r.Context(), a, id, *in.AdjustedQuantity, in.Reason, key, version)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	w.Header().Set("ETag", etag(rec.Exception.Version))
	if rec.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	// Accepted work is not an acknowledged resolution: only an ERP
	// acknowledgment moves the exception to resolved.
	status := http.StatusOK
	if rec.Exception.State == exceptions.StatePending {
		status = http.StatusAccepted
	}
	writeJSON(w, status, toJSON(rec.Exception))
}

// ---- cursors ----

type cursor struct {
	Tenant  string `json:"t"`
	State   string `json:"s"`
	Created string `json:"c"`
	ID      string `json:"i"`
}

func (h *Handler) sign(payload string) string {
	m := hmac.New(sha256.New, h.cfg.CursorKey)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

func (h *Handler) encodeCursor(t tenancy.TenantID, s exceptions.State, p exceptions.Position) string {
	raw, _ := json.Marshal(cursor{Tenant: string(t), State: string(s), Created: p.CreatedAt.UTC().Format(time.RFC3339Nano), ID: string(p.ID)})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return payload + "." + h.sign(payload)
}

func (h *Handler) decodeCursor(c string, t tenancy.TenantID, s exceptions.State) (*exceptions.Position, error) {
	bad := fmt.Errorf("%w: invalid cursor", exceptions.ErrInvalid)
	payload, sig, ok := strings.Cut(c, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(h.sign(payload))) {
		return nil, bad
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return nil, bad
	}
	var cur cursor
	if json.Unmarshal(raw, &cur) != nil || cur.Tenant != string(t) || cur.State != string(s) {
		return nil, bad // a cursor is bound to its tenant and filter
	}
	created, err := time.Parse(time.RFC3339Nano, cur.Created)
	if err != nil {
		return nil, bad
	}
	id, err := exceptions.ParseID(cur.ID)
	if err != nil {
		return nil, bad
	}
	return &exceptions.Position{CreatedAt: created, ID: id}, nil
}
