package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
)

// ParseSessionRecord strictly decodes one bounded durable session record. A
// version 2 record must carry the complete binding and a refresh token; a
// version 1 record is a legacy access-token-only session. Unknown members,
// trailing data, a non-object body, an unsupported version, an oversize file
// and an incomplete record are refused.
func ParseSessionRecord(data []byte) (SessionRecord, error) {
	if len(data) == 0 {
		return SessionRecord{}, fmt.Errorf("session file is empty")
	}
	if len(data) > MaxSessionFileBytes {
		return SessionRecord{}, fmt.Errorf("session file is %d bytes; at most %d are allowed", len(data), MaxSessionFileBytes)
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return SessionRecord{}, fmt.Errorf("session file must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var record SessionRecord
	if err := decoder.Decode(&record); err != nil {
		return SessionRecord{}, fmt.Errorf("invalid session file: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return SessionRecord{}, fmt.Errorf("invalid session file: trailing data")
	}
	switch record.Version {
	case SessionFileVersion:
		if err := record.validateV2(); err != nil {
			return SessionRecord{}, err
		}
	case 0, 1:
		record.Version = 1
		if err := record.validateV1(); err != nil {
			return SessionRecord{}, err
		}
	default:
		return SessionRecord{}, fmt.Errorf("session file version %d is not supported", record.Version)
	}
	return record, nil
}

// validateV2 checks the complete v2 session shape. A v2 record is always bound
// to the issuer, client, audience, API URL and the endpoints that minted it.
func (record SessionRecord) validateV2() error {
	if record.Kind != "session" {
		return fmt.Errorf("session file is not a human session")
	}
	if record.Token == "" {
		return fmt.Errorf("session file has an empty token")
	}
	if record.ExpiresAt.IsZero() {
		return fmt.Errorf("session file has no expiry")
	}
	if record.RefreshToken == "" {
		return fmt.Errorf("session file is missing its refresh token")
	}
	binding := map[string]string{
		"issuer":              record.Issuer,
		"client_id":           record.ClientID,
		"audience":            record.Audience,
		"api_url":             record.APIURL,
		"token_endpoint":      record.TokenEndpoint,
		"revocation_endpoint": record.RevocationEndpoint,
	}
	for name, value := range binding {
		if value == "" {
			return fmt.Errorf("session file is missing %s", name)
		}
	}
	// The recorded endpoints are used verbatim by refresh and logout, so they
	// must belong to the issuer that minted the session.
	if !api.SameOrigin(record.Issuer, record.TokenEndpoint) {
		return fmt.Errorf("session token endpoint does not share the issuer origin")
	}
	if !api.SameOrigin(record.Issuer, record.RevocationEndpoint) {
		return fmt.Errorf("session revocation endpoint does not share the issuer origin")
	}
	return nil
}

// validateV1 checks the legacy access-token-only shape. A v1 record cannot
// carry any v2-only member.
func (record SessionRecord) validateV1() error {
	if record.Kind != "session" {
		return fmt.Errorf("session file is not a human session")
	}
	if record.Token == "" {
		return fmt.Errorf("session file has an empty token")
	}
	if record.ExpiresAt.IsZero() {
		return fmt.Errorf("session file has no expiry")
	}
	if record.RefreshToken != "" || record.Issuer != "" || record.ClientID != "" ||
		record.Audience != "" || record.APIURL != "" || record.TokenEndpoint != "" ||
		record.RevocationEndpoint != "" {
		return fmt.Errorf("legacy session file carries version 2 members")
	}
	return nil
}

// Legacy reports whether the record is a version 1 access-token-only session.
func (record SessionRecord) Legacy() bool {
	return record.Version < SessionFileVersion
}

// Usable reports whether the access token is present and current at now. An
// expiry equal to now is expired.
func (record SessionRecord) Usable(now time.Time) bool {
	return record.Token != "" && !record.ExpiresAt.IsZero() && now.Before(record.ExpiresAt)
}

// NeedsRefresh reports whether the session expires at or inside the refresh
// leeway, so a rotation must happen before the next request.
func (record SessionRecord) NeedsRefresh(now time.Time) bool {
	return !now.Add(RefreshLeeway).Before(record.ExpiresAt)
}

// BoundTo refuses a session minted for another issuer, client, audience, API
// URL or endpoints than the discovered configuration.
func (record SessionRecord) BoundTo(discovery api.DiscoveryDocument, apiURL string) error {
	if apiURL == "" || record.APIURL != apiURL {
		return fmt.Errorf("the session was issued for another API URL")
	}
	switch {
	case record.Issuer == "" || record.Issuer != discovery.Issuer:
		return fmt.Errorf("the session was issued by another issuer")
	case record.ClientID == "" || record.ClientID != discovery.ClientID:
		return fmt.Errorf("the session was issued to another client")
	case record.Audience == "" || record.Audience != discovery.Audience:
		return fmt.Errorf("the session was issued for another audience")
	case record.APIURL != discovery.APIURL:
		return fmt.Errorf("the session was issued for another API URL")
	}
	return nil
}

// LoadRecord reads the durable session file on every call. A missing file is
// returned as an error satisfying errors.Is(err, fs.ErrNotExist).
func (store FileSessionStore) LoadRecord(ctx context.Context) (SessionRecord, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return SessionRecord{}, err
	}
	if store.Files == nil || store.Path == "" {
		return SessionRecord{}, fault.New(fault.InternalFailure, "the session store is not configured")
	}
	data, err := store.Files.ReadFile(store.Path)
	if err != nil {
		return SessionRecord{}, err
	}
	return ParseSessionRecord(data)
}

// SaveRecord validates and durably persists one version 2 session through
// ReplaceFile at mode 0600. A legacy, expired, unbound or refreshless record is
// refused before anything is written.
func (store FileSessionStore) SaveRecord(ctx context.Context, record SessionRecord) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.Files == nil || store.Path == "" {
		return fault.New(fault.InternalFailure, "the session store is not configured")
	}
	if record.Version != SessionFileVersion {
		return fault.New(fault.Unauthenticated, "only a version 2 session can be persisted")
	}
	if err := record.validateV2(); err != nil {
		return fault.New(fault.Unauthenticated, "the session is incomplete")
	}
	if !store.now().Before(record.ExpiresAt) {
		return fault.New(fault.Unauthenticated, "the session is expired")
	}
	data, err := json.Marshal(record)
	if err != nil {
		return fault.New(fault.InternalFailure, "the session is not encodable")
	}
	if len(data) > MaxSessionFileBytes {
		return fault.New(fault.Unauthenticated, "the session is too large")
	}
	if err := store.Files.ReplaceFile(store.Path, data, 0o600); err != nil {
		return fault.Wrap(fault.DependencyUnavailable, "the session file is not writable", err)
	}
	return nil
}

// DeleteRecord removes the durable session file. A missing file is an
// idempotent success; every other removal failure is reported.
func (store FileSessionStore) DeleteRecord(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if store.Files == nil || store.Path == "" {
		return fault.New(fault.InternalFailure, "the session store is not configured")
	}
	if err := store.Files.Remove(store.Path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fault.Wrap(fault.DependencyUnavailable, "the session file could not be deleted", err)
	}
	return nil
}
