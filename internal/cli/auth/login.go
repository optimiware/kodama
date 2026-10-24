package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
)

// DefaultLoginTimeout bounds how long one login attempt waits for the browser.
const DefaultLoginTimeout = 5 * time.Minute

const exchangeTimeout = 30 * time.Second

// ServiceConfig wires a Service. Every field is optional; the zero value gives
// the OS keyring, the platform browser opener and a default HTTP client.
type ServiceConfig struct {
	// Keyring is the default credential store; nil uses the OS keyring.
	Keyring CredentialStore
	// File is used only for an explicit `--credential-store file`; nil uses a
	// FileStore under DefaultFileStoreDir. It is never a fallback.
	File    CredentialStore
	Browser BrowserOpener
	// HTTP is copied for the token exchange; redirects are never followed.
	HTTP *http.Client
	// AllowInsecureLoopbackEndpoints permits http:// provider endpoints on
	// loopback hosts (local development and tests only).
	AllowInsecureLoopbackEndpoints bool
	LoginTimeout                   time.Duration
	Now                            func() time.Time
	// Rand supplies state and verifier randomness; nil uses crypto/rand.
	Rand io.Reader
}

// Service performs login, logout and credential lookup. It implements
// root.TokenResolver.
type Service struct {
	cfg ServiceConfig

	mu      sync.Mutex
	keyring CredentialStore
	file    CredentialStore
}

var _ root.TokenResolver = (*Service)(nil)

// NewService applies defaults.
func NewService(cfg ServiceConfig) *Service {
	if cfg.Browser == nil {
		cfg.Browser = SystemBrowser{}
	}
	if cfg.LoginTimeout <= 0 {
		cfg.LoginTimeout = DefaultLoginTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	return &Service{cfg: cfg}
}

// store selects the one credential store the context names. It never falls
// back to or enumerates the other.
func (s *Service) store(kind string) (CredentialStore, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch kind {
	case root.StoreKeyring, "":
		if s.keyring == nil {
			s.keyring = s.cfg.Keyring
			if s.keyring == nil {
				s.keyring = NewKeyringStore(nil, "")
			}
		}
		return s.keyring, nil
	case root.StoreFile:
		if s.file == nil {
			s.file = s.cfg.File
			if s.file == nil {
				dir, err := DefaultFileStoreDir()
				if err != nil {
					return nil, storeErr(ErrStoreUnavailable, err, "no credential directory is available")
				}
				s.file = NewFileStore(dir)
			}
		}
		return s.file, nil
	}
	return nil, root.Usagef("--credential-store must be keyring or file")
}

// Attempt states. A login attempt is single use: it moves forward only, and
// any failure is terminal.
type attemptState int

const (
	stateCreated attemptState = iota
	stateListening
	stateCallbackAccepted
	stateExchanged
	statePersisted
	stateFailed
)

func (s attemptState) String() string {
	return [...]string{"created", "listening", "callback_accepted", "exchanged", "persisted", "failed"}[s]
}

var errBadTransition = errors.New("invalid login attempt transition")

// attempt guards the lifecycle. advance fails for a repeated or out-of-order
// transition; fail is terminal and allowed from any non-persisted state.
type attempt struct {
	mu    sync.Mutex
	state attemptState
}

func (a *attempt) advance(from, to attemptState) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != from || to != from+1 || to == stateFailed {
		return fmt.Errorf("%w: %s -> %s while %s", errBadTransition, from, to, a.state)
	}
	a.state = to
	return nil
}

func (a *attempt) fail() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state != statePersisted {
		a.state = stateFailed
	}
}

// LoginResult describes a successful, persisted login. It never carries the token.
type LoginResult struct {
	Factory         string `json:"factory"`
	CredentialStore string `json:"credentialStore"`
	ExpiresAt       string `json:"expiresAt"`
}

// Login runs one bounded authorization-code-with-PKCE attempt for the
// effective context. Success means the credential was persisted; the token is
// never returned to the caller. Any failure closes the loopback listener and
// leaves any previously stored credential untouched.
func (s *Service) Login(parent context.Context, ec root.EffectiveContext) (res LoginResult, err error) {
	if err := ec.RequireFactory(); err != nil {
		return LoginResult{}, err
	}
	if ec.NonInteractive {
		return LoginResult{}, root.NewError(root.ExitUsage, "interactive_required",
			"login needs a browser and cannot run with --non-interactive; log in interactively first")
	}
	if err := ec.Auth.Validate(); err != nil {
		return LoginResult{}, err
	}
	if err := validateEndpoints(ec.Auth, s.cfg.AllowInsecureLoopbackEndpoints); err != nil {
		return LoginResult{}, err
	}
	redirect, err := parseRedirect(ec.Auth.RedirectURI)
	if err != nil {
		return LoginResult{}, root.Usagef("%v", err)
	}
	store, err := s.store(ec.CredentialStore)
	if err != nil {
		return LoginResult{}, err
	}

	// The timeout bounds the whole attempt, not only the wait for the
	// callback: browser launch, the code exchange and credential persistence
	// all run under it, so a blocked keyring prompt cannot hang the command.
	ctx, cancelAttempt := context.WithTimeout(parent, s.cfg.LoginTimeout)
	defer cancelAttempt()
	if cerr := ctx.Err(); cerr != nil {
		return LoginResult{}, ctxFailure(cerr)
	}

	var at attempt
	defer func() {
		if err != nil {
			at.fail()
		}
	}()
	state, err := randomToken(s.cfg.Rand)
	if err != nil {
		return LoginResult{}, root.NewError(root.ExitFailure, "login_failed", "cannot generate login state").WithCause(err)
	}
	verifier, err := randomToken(s.cfg.Rand) // independent of state
	if err != nil {
		return LoginResult{}, root.NewError(root.ExitFailure, "login_failed", "cannot generate the PKCE verifier").WithCause(err)
	}

	deadline := s.cfg.Now().Add(s.cfg.LoginTimeout)
	lb, redirectURL, err := newLoopback(redirect, state, deadline, s.cfg.Now)
	if err != nil {
		return LoginResult{}, root.NewError(root.ExitFailure, "loopback_unavailable",
			"cannot listen on the loopback redirect address (is another login running?)").WithCause(err)
	}
	defer lb.Close()
	if err = at.advance(stateCreated, stateListening); err != nil {
		return LoginResult{}, err
	}

	cfg := oauthConfig(ec.Auth, redirectURL)
	authURL := cfg.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	if err = s.cfg.Browser.Open(ctx, authURL); err != nil {
		return LoginResult{}, root.NewError(root.ExitFailure, "browser_unavailable", "cannot open the browser for login").WithCause(err)
	}

	var cb callbackResult
	select {
	case cb = <-lb.result:
	case <-ctx.Done():
		if parent.Err() == nil { // the attempt's own deadline, not the caller's
			err = root.NewError(root.ExitTimeout, "login_timeout", "no login callback arrived in time; run `kodama auth login` again")
		} else {
			err = ctxFailure(parent.Err())
		}
		return LoginResult{}, err
	}
	if err = at.advance(stateListening, stateCallbackAccepted); err != nil {
		return LoginResult{}, err
	}
	lb.Close() // the callback is consumed; stop accepting connections before exchanging

	if cb.Denied != "" {
		err = root.NewError(root.ExitAuth, "login_denied", "the provider refused the login (%s)", cb.Denied)
		return LoginResult{}, err
	}

	xctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	cred, xerr := exchange(xctx, cfg, httpClientFor(s.cfg.HTTP), cb.Code, verifier, s.cfg.Now())
	switch {
	case xerr == nil:
	case ctx.Err() != nil:
		e := ctxFailure(ctx.Err()).(*root.Error)
		e.Details = root.UncertainDetails(exchangeRecovery)
		err = e
		return LoginResult{}, err
	case errors.Is(xerr, ErrExchangeRejected):
		err = root.NewError(root.ExitAuth, "token_exchange_rejected", "the provider rejected the authorization code; run `kodama auth login` again").WithCause(xerr)
		return LoginResult{}, err
	case errors.Is(xerr, ErrBadToken):
		err = root.NewError(root.ExitFailure, "token_unusable", "the provider returned no usable access token").WithCause(xerr)
		return LoginResult{}, err
	case exchangeTimedOut(xerr):
		e := root.NewError(root.ExitTimeout, "timeout", "the code exchange timed out; the code may have been consumed, so run `kodama auth login` again").WithCause(xerr)
		e.Details = root.UncertainDetails(exchangeRecovery)
		return LoginResult{}, e
	default:
		e := root.NewError(root.ExitFailure, "token_exchange_uncertain",
			"the code exchange did not complete; the code may have been consumed, so run `kodama auth login` again").WithCause(xerr)
		e.Details = root.UncertainDetails(exchangeRecovery)
		err = e
		return LoginResult{}, err
	}
	if err = at.advance(stateCallbackAccepted, stateExchanged); err != nil {
		return LoginResult{}, err
	}

	// The credential becomes usable only after it is durably stored.
	if err = store.Save(ctx, ec.CredentialKey(), cred); err != nil {
		if ctx.Err() != nil {
			// The save was started and then abandoned: the backend may still
			// complete it, so the outcome is unknown. Never report success.
			var le *root.Error
			if errors.As(err, &le) && le.Exit == root.ExitTimeout {
				e := *le
				e.Details = root.UncertainDetails(persistRecovery)
				err = &e
			}
		}
		return LoginResult{}, err
	}
	if err = at.advance(stateExchanged, statePersisted); err != nil {
		return LoginResult{}, err
	}
	out := LoginResult{Factory: ec.Factory, CredentialStore: ec.CredentialStore}
	if !cred.Expiry.IsZero() {
		out.ExpiresAt = cred.Expiry.UTC().Format(time.RFC3339)
	}
	return out, nil
}

// Recovery guidance for a login whose outcome is unknown. It concerns the
// authorization code and the local credential, never a resource mutation, so it
// must not be replaced by root.RecoveryGuidance.
const (
	exchangeRecovery = "the authorization code may have been consumed and no credential was saved: run `kodama auth login` again"
	persistRecovery  = "the credential store write was abandoned and may still complete, saving or replacing the credential for this factory: run `kodama auth login` again to replace it, or `kodama auth logout` to remove it"
)

func exchangeTimedOut(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func ctxFailure(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return root.NewError(root.ExitTimeout, "timeout", "the operation timed out").WithCause(err)
	}
	return root.NewError(root.ExitTimeout, "interrupted", "the operation was interrupted").WithCause(err)
}

// Logout deletes the local credential for the effective context. It is
// idempotent and does not revoke the token at the provider.
func (s *Service) Logout(ctx context.Context, ec root.EffectiveContext) error {
	if err := ec.RequireFactory(); err != nil {
		return err
	}
	store, err := s.store(ec.CredentialStore)
	if err != nil {
		return err
	}
	return store.Delete(storeContext(ctx, ec), ec.CredentialKey())
}

// storeContext carries the invocation's interaction policy to the store, so a
// non-interactive command can never block on a keyring prompt.
func storeContext(ctx context.Context, ec root.EffectiveContext) context.Context {
	if ec.NonInteractive {
		return WithNonInteractive(ctx)
	}
	return ctx
}

// TokenSource implements root.TokenResolver. Lookup is bound to the effective
// context's credential key and selected store, is lazy, and never starts a
// login: a missing or expired credential is client.ErrNotAuthenticated.
func (s *Service) TokenSource(ec root.EffectiveContext) (client.AccessTokenSource, error) {
	store, err := s.store(ec.CredentialStore)
	if err != nil {
		return nil, err
	}
	return &tokenSource{store: store, key: ec.CredentialKey(), now: s.cfg.Now, nonInteractive: ec.NonInteractive}, nil
}

type tokenSource struct {
	store CredentialStore
	key   string
	now   func() time.Time
	// nonInteractive forbids the store from prompting (--non-interactive).
	nonInteractive bool
}

func (t *tokenSource) AccessToken(ctx context.Context) (string, error) {
	if t.nonInteractive {
		ctx = WithNonInteractive(ctx)
	}
	c, err := t.store.Load(ctx, t.key)
	switch {
	case errors.Is(err, ErrCredentialNotFound):
		return "", fmt.Errorf("%w: not logged in to this factory", client.ErrNotAuthenticated)
	case err != nil:
		return "", err
	}
	if c.Expired(t.now()) {
		return "", fmt.Errorf("%w: the stored credential has expired", client.ErrNotAuthenticated)
	}
	return c.AccessToken, nil
}
