package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Principal is a verified caller.
type Principal struct {
	UserID      string
	Issuer      string
	Subject     string
	DisplayName string
}

type ctxKey struct{}

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

func principal(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// Authenticator verifies one bearer credential.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (Principal, error)
}

// OIDC authenticates JWT access tokens against one issuer.
type OIDC struct {
	verifier *oidc.IDTokenVerifier
}

func NewOIDC(ctx context.Context, issuer, audience, clientID string) (*OIDC, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	cfg := &oidc.Config{ClientID: clientID}
	if audience != "" {
		cfg = &oidc.Config{ClientID: audience, SkipClientIDCheck: false}
	}
	return &OIDC{verifier: provider.Verifier(cfg)}, nil
}

func (o *OIDC) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	token, err := bearer(r)
	if err != nil {
		return Principal{}, err
	}
	id, err := o.verifier.Verify(ctx, token)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	var claims struct {
		Email string `json:"email"`
		Name  string `json:"name"`
	}
	_ = id.Claims(&claims)
	name := claims.Name
	if name == "" {
		name = claims.Email
	}
	return Principal{Issuer: id.Issuer, Subject: id.Subject, DisplayName: name}, nil
}

// StaticToken is the explicit development credential. It must never be enabled
// in production: the composition refuses to construct it there.
type StaticToken struct {
	Token  string
	UserID string
	Name   string
}

func (s StaticToken) Authenticate(_ context.Context, r *http.Request) (Principal, error) {
	token, err := bearer(r)
	if err != nil {
		return Principal{}, err
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.Token)) != 1 {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{UserID: s.UserID, Issuer: "kodama:dev", Subject: s.UserID, DisplayName: s.Name}, nil
}

func bearer(r *http.Request) (string, error) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", ErrUnauthenticated
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if token == "" {
		return "", ErrUnauthenticated
	}
	return token, nil
}

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrForbidden       = errors.New("forbidden")
)
