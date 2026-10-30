package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// ArtifactVerifier checks a state entry's explicit retention roots against the
// artifact module's current ownership and content metadata.
type ArtifactVerifier interface {
	VerifyArtifact(ctx context.Context, scope Scope, id domain.ArtifactID, digest string) error
}

// BindingResolver authenticates the attempt and resolves one declared alias.
// It is supplied by composition; the state package never accepts a caller's
// namespace, tenant or project as authority.
type BindingResolver interface {
	ResolveBinding(ctx context.Context, r *http.Request, attempt, alias string) (Scope, Binding, error)
}

// BindingReauthorizer is the optional check made after the body is read and
// immediately before a durable mutation. It closes the gap between the first
// authorization and the write; it is not a transaction fence.
type BindingReauthorizer interface {
	ReauthorizeBinding(ctx context.Context, r *http.Request, attempt, alias string) error
}

// Handler serves the runtime state API under
// /runtime/v3/attempts/{attempt}/state/{binding}/entries.
type Handler struct {
	Store     Store
	Resolver  BindingResolver
	Artifacts ArtifactVerifier
}

// NewHandler constructs a handler. Both dependencies are required.
func NewHandler(store Store, resolver BindingResolver) (*Handler, error) {
	if store == nil || resolver == nil {
		return nil, fmt.Errorf("state: a store and binding resolver are required")
	}
	return &Handler{Store: store, Resolver: resolver}, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	attempt, alias, ok := statePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "route not found")
		return
	}
	scope, binding, err := h.Resolver.ResolveBinding(r.Context(), r, attempt, alias)
	if err != nil {
		writeError(w, http.StatusForbidden, "state_binding_denied", "state binding denied")
		return
	}
	if !binding.Read && r.Method == http.MethodGet {
		writeError(w, http.StatusForbidden, "state_binding_denied", "state read denied")
		return
	}
	if !binding.Write && (r.Method == http.MethodPut || r.Method == http.MethodDelete) {
		writeError(w, http.StatusForbidden, "state_binding_denied", "state write denied")
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.get(w, r, scope, binding)
	case http.MethodPut:
		h.put(w, r, scope, binding)
	case http.MethodDelete:
		h.delete(w, r, scope, binding)
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request, scope Scope, binding Binding) {
	if prefix := r.URL.Query().Get("prefix"); prefix != "" || r.URL.Query().Has("prefix") {
		absolutePrefix, err := AbsolutePrefix(binding, prefix)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_key", "invalid state prefix")
			return
		}
		limit := statev1.DefaultListLimit
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > statev1.MaxListLimit {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid limit")
				return
			}
			limit = n
		}
		cursor := r.URL.Query().Get("cursor")
		if cursor != "" {
			absoluteCursor, err := AbsoluteKey(binding, cursor)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_cursor", "invalid cursor")
				return
			}
			cursor = absoluteCursor
		}
		items, next, err := h.Store.List(r.Context(), scope, binding.Namespace, absolutePrefix, cursor, limit)
		if err != nil {
			writeStoreError(w, err)
			return
		}
		for i := range items {
			relative, err := RelativeKey(binding, items[i].Key)
			if err != nil {
				writeStoreError(w, err)
				return
			}
			items[i].Key = relative
		}
		if next != "" {
			next, err = RelativeKey(binding, next)
			if err != nil {
				writeStoreError(w, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "nextCursor": next})
		return
	}
	key, ok := h.key(w, r, binding)
	if !ok {
		return
	}
	entry, err := h.Store.Get(r.Context(), scope, binding.Namespace, key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	relative, err := RelativeKey(binding, entry.Key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	entry.Key = relative
	w.Header().Set("ETag", strconv.Quote(entry.ETag()))
	writeJSON(w, http.StatusOK, entry)
}

func (h *Handler) put(w http.ResponseWriter, r *http.Request, scope Scope, binding Binding) {
	key, ok := h.key(w, r, binding)
	if !ok {
		return
	}
	var req statev1.WriteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, statev1.MaxValueBytes+16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid state request")
		return
	}
	if err := h.reauthorize(r); err != nil {
		writeError(w, http.StatusForbidden, "state_binding_denied", "state write denied")
		return
	}
	if err := h.verifyReferences(r, scope, req); err != nil {
		writeStoreError(w, err)
		return
	}
	actor := attemptActor(r)
	var (
		entry statev1.Entry
		err   error
	)
	if r.Header.Get("If-None-Match") == "*" {
		entry, err = h.Store.Create(r.Context(), scope, binding.Namespace, key, actor, req)
	} else if expected := strings.Trim(r.Header.Get("If-Match"), `"`); expected != "" {
		entry, err = h.Store.Update(r.Context(), scope, binding.Namespace, key, actor, expected, req)
	} else {
		writeError(w, http.StatusPreconditionRequired, "version_required", "If-Match or If-None-Match is required")
		return
	}
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("ETag", strconv.Quote(entry.ETag()))
	writeJSON(w, http.StatusOK, entry)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request, scope Scope, binding Binding) {
	key, ok := h.key(w, r, binding)
	if !ok {
		return
	}
	expected := strings.Trim(r.Header.Get("If-Match"), `"`)
	if expected == "" {
		writeError(w, http.StatusPreconditionRequired, "version_required", "If-Match is required")
		return
	}
	if err := h.reauthorize(r); err != nil {
		writeError(w, http.StatusForbidden, "state_binding_denied", "state write denied")
		return
	}
	if err := h.Store.Delete(r.Context(), scope, binding.Namespace, key, attemptActor(r), expected); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) key(w http.ResponseWriter, r *http.Request, binding Binding) (string, bool) {
	key := r.URL.Query().Get("key")
	absolute, err := AbsoluteKey(binding, key)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_key", "invalid state key")
		return "", false
	}
	return absolute, true
}

func statePath(path string) (attempt, alias string, ok bool) {
	const prefix = "/runtime/v3/attempts/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	attempt, rest, ok = strings.Cut(rest, "/state/")
	if !ok || attempt == "" || strings.Contains(attempt, "/") {
		return "", "", false
	}
	alias, suffix, ok := strings.Cut(rest, "/entries")
	if !ok || suffix != "" || alias == "" || strings.Contains(alias, "/") {
		return "", "", false
	}
	return attempt, alias, true
}

func (h *Handler) verifyReferences(r *http.Request, scope Scope, req statev1.WriteRequest) error {
	if len(req.ArtifactReferences) == 0 {
		return nil
	}
	if h.Artifacts == nil {
		return fmt.Errorf("%w: artifact verification is not configured", ErrUnavailable)
	}
	for _, ref := range req.ArtifactReferences {
		if err := h.Artifacts.VerifyArtifact(r.Context(), scope, domain.ArtifactID(ref.ArtifactID), ref.Digest); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) reauthorize(r *http.Request) error {
	checker, ok := h.Resolver.(BindingReauthorizer)
	if !ok {
		return nil
	}
	attempt, alias, ok := statePath(r.URL.Path)
	if !ok {
		return ErrDenied
	}
	return checker.ReauthorizeBinding(r.Context(), r, attempt, alias)
}

func attemptActor(r *http.Request) string {
	_, attempt, _ := statePath(r.URL.Path)
	return attempt
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "state entry not found")
	case errors.Is(err, ErrExists):
		writeError(w, http.StatusPreconditionFailed, "version_conflict", "state entry already exists")
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusPreconditionFailed, "version_conflict", "state version does not match")
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid_request", "invalid state request")
	case errors.Is(err, ErrQuota):
		writeError(w, http.StatusTooManyRequests, "state_quota_exceeded", "state quota exceeded")
	case errors.Is(err, ErrDenied):
		writeError(w, http.StatusForbidden, "state_binding_denied", "state access denied")
	default:
		writeError(w, http.StatusServiceUnavailable, "state_unavailable", "state service unavailable")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
