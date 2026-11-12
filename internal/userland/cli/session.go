package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// sessionFile is the durable human-session JSON shape. It never contains a
// service or execution credential.
type sessionFile struct {
	Kind      string    `json:"kind"`
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Save validates and persists one human session with mode 0600. A non-human,
// missing, expired or unwritable session refuses and writes nothing.
func (store FileSessionStore) Save(ctx context.Context, credential client.Credential) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return fault.Wrap(fault.Unauthenticated, "session save canceled", err)
	}
	if credential.Kind != client.KindSession {
		return fault.New(fault.Unauthenticated, "only a human session can be persisted")
	}
	if !credential.Usable(store.now()) {
		return fault.New(fault.Unauthenticated, "the session is missing, expired or unusable")
	}
	if store.Files == nil || store.Path == "" {
		return fault.New(fault.InternalFailure, "the session store is not configured")
	}
	data, err := json.Marshal(sessionFile{
		Kind:      string(credential.Kind),
		Token:     credential.Token,
		ExpiresAt: credential.ExpiresAt,
	})
	if err != nil {
		return fault.New(fault.InternalFailure, "the session is not encodable")
	}
	if err := store.Files.ReplaceFile(store.Path, data, 0o600); err != nil {
		return fault.New(fault.DependencyUnavailable, "the session file is not writable")
	}
	return nil
}

// Load reads the session file again on every call and requires a current human
// session. Every refusal uses the stable unauthenticated class and carries no
// credential material.
func (store FileSessionStore) Load(ctx context.Context) (client.Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return client.Credential{}, fault.Wrap(fault.Unauthenticated, "session load canceled", err)
	}
	if store.Files == nil || store.Path == "" {
		return client.Credential{}, fault.New(fault.Unauthenticated, "the session store is not configured")
	}
	data, err := store.Files.ReadFile(store.Path)
	if err != nil {
		return client.Credential{}, fault.New(fault.Unauthenticated, "the session file is unreadable")
	}
	var file sessionFile
	if err := json.Unmarshal(data, &file); err != nil {
		return client.Credential{}, fault.New(fault.Unauthenticated, "the session file is malformed")
	}
	credential := client.Credential{
		Kind:      client.CredentialKind(file.Kind),
		Token:     file.Token,
		ExpiresAt: file.ExpiresAt,
	}
	if credential.Kind != client.KindSession {
		return client.Credential{}, fault.New(fault.Unauthenticated, "the session file is not a human session")
	}
	if !credential.Usable(store.now()) {
		return client.Credential{}, fault.New(fault.Unauthenticated, "the session file is expired or unusable")
	}
	return credential, nil
}

// now returns the injected clock or the wall clock.
func (store FileSessionStore) now() time.Time {
	if store.Now != nil {
		return store.Now()
	}
	return time.Now()
}
