package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/client"
	"github.com/optimiware/kodama/internal/fault"
)

// CommandDeps carries the optional command-runtime collaborators that are not
// part of the Ports declaration. A zero CommandDeps keeps Run's legacy
// entrypoint behavior; kodactl passes the real values through RunWithDeps.
type CommandDeps struct {
	Login LoginRuntime
}

// LoginRuntime is the injected production collaborator set for the
// login/logout/refresh/status commands. kodactl supplies the real
// cross-process SessionLock; this command logic only consumes the port.
type LoginRuntime struct {
	OIDC        OIDCClient
	Browser     BrowserOpener
	Loopback    LoopbackListener
	Random      Random
	Clock       Clock
	Lock        SessionLock
	Store       SessionRecordStore
	Environment func(string) string
	Timeout     time.Duration
	// Issuer, ClientID and Audience are the trusted expected identity for the
	// refresh and logout binding checks. A missing value is filled from one
	// discovery of the configured API; the record being validated is never
	// used as its own expectation.
	Issuer   string
	ClientID string
	Audience string
}

// loginRuntimeConfigured reports whether a real OAuth login runtime was
// injected.
func loginRuntimeConfigured(runtime LoginRuntime) bool {
	return runtime.OIDC != nil || runtime.Store != nil || runtime.Lock != nil
}

// parseLoginFlags accepts only the two flags that force the headless device
// flow.
func parseLoginFlags(args []string) (bool, error) {
	forceDevice := false
	seen := make(map[string]bool, len(args))
	for _, argument := range args {
		switch argument {
		case "--device", "--no-browser":
			if seen[argument] {
				return false, fmt.Errorf("flag %q is repeated", argument)
			}
			seen[argument] = true
			forceDevice = true
		default:
			return false, fmt.Errorf("unexpected argument %q", argument)
		}
	}
	return forceDevice, nil
}

// selectLoginMode applies an explicit headless force or derives the mode from
// the injected environment.
func selectLoginMode(runtime LoginRuntime, forceDevice bool) (LoginMode, error) {
	if forceDevice {
		return LoginDevice, nil
	}
	environment := runtime.Environment
	if environment == nil {
		environment = os.Getenv
	}
	display := environment("DISPLAY")
	if display == "" {
		display = environment("WAYLAND_DISPLAY")
	}
	return SelectLoginMode(LoginEnvironment{
		Display:       display,
		SSHConnection: environment("SSH_CONNECTION"),
	})
}

// loginFallbackBrowser prints the authorization URL before it tries the
// injected system browser. A missing or failing browser keeps the printed URL
// as the manual fallback instead of aborting the login.
type loginFallbackBrowser struct {
	opener BrowserOpener
	out    io.Writer
	errOut io.Writer
}

func (browser loginFallbackBrowser) Open(ctx context.Context, url string) error {
	fmt.Fprintf(browser.out, "Open this URL to sign in:\n%s\n", url)
	if browser.opener == nil {
		return nil
	}
	if err := browser.opener.Open(ctx, url); err != nil {
		fmt.Fprintln(browser.errOut, "the system browser could not be opened; open the URL above manually")
	}
	return nil
}

// runConfiguredLogin performs the real OAuth login over the injected runtime.
func runConfiguredLogin(ctx context.Context, config Config, ports Ports, runtime LoginRuntime, forceDevice bool) int {
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	mode, err := selectLoginMode(runtime, forceDevice)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if runtime.OIDC == nil || runtime.Store == nil || runtime.Lock == nil {
		if runtime.OIDC == nil {
			return failure(ports.Err, fault.New(fault.InternalFailure, "no OIDC client is configured"))
		}
		return failure(ports.Err, fault.New(fault.InternalFailure, "the login runtime is incomplete"))
	}
	if mode == LoginBrowser && runtime.Loopback == nil {
		return failure(ports.Err, fault.New(fault.InternalFailure, "no loopback callback listener is configured"))
	}
	record, err := Login(ctx, LoginDeps{
		APIURL:   config.ServerURL,
		Mode:     mode,
		OIDC:     runtime.OIDC,
		Browser:  loginFallbackBrowser{opener: runtime.Browser, out: ports.Out, errOut: ports.Err},
		Loopback: runtime.Loopback,
		Random:   runtime.Random,
		Clock:    runtime.Clock,
		Lock:     runtime.Lock,
		Store:    runtime.Store,
		Out:      ports.Out,
		Err:      ports.Err,
		Timeout:  runtime.Timeout,
	})
	if err != nil {
		return failure(ports.Err, err)
	}
	fmt.Fprintf(ports.Out, "logged in; issuer %s; access token expires %s\n",
		record.Issuer, formatSessionTime(record.ExpiresAt))
	return ExitSuccess
}

// runLogout revokes the recorded refresh token and deletes the local session.
// A missing session is a successful no-op. The token is only revoked when the
// session binding can be verified from an explicit expectation or the
// configured API's own discovery document; an unverifiable binding still
// deletes the local file but reports the refusal and never sends the token.
// A revocation failure after the local delete is a visible failure.
func runLogout(ctx context.Context, config Config, args []string, ports Ports, runtime LoginRuntime) int {
	if len(args) != 0 {
		return usage(ports.Err, "logout takes no arguments")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if runtime.Store == nil || runtime.Lock == nil {
		return failure(ports.Err, fault.New(fault.InternalFailure, "the login runtime is incomplete"))
	}
	apiURL, err := normalizeAPIURL(config.ServerURL)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	record, err := runtime.Store.LoadRecord(ctx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintln(ports.Out, "not logged in")
			return ExitSuccess
		}
		return failure(ports.Err, fault.Wrap(fault.DependencyUnavailable, "the session could not be loaded", err))
	}
	if record.RefreshToken != "" {
		issuer, clientID, audience, bindErr := trustedBinding(ctx, runtime.OIDC, apiURL,
			runtime.Issuer, runtime.ClientID, runtime.Audience)
		if bindErr != nil {
			return refuseUnverifiedLogout(ctx, ports, runtime.Store, bindErr)
		}
		err = Logout(ctx, LogoutDeps{
			APIURL:   apiURL,
			Issuer:   issuer,
			ClientID: clientID,
			Audience: audience,
			OIDC:     runtime.OIDC,
			Lock:     runtime.Lock,
			Store:    runtime.Store,
		})
	} else {
		// A legacy or unbound session has no refresh token to revoke; deleting
		// it locally is the whole logout.
		err = Logout(ctx, LogoutDeps{
			APIURL: apiURL,
			OIDC:   runtime.OIDC,
			Lock:   runtime.Lock,
			Store:  runtime.Store,
		})
	}
	if err != nil {
		return failure(ports.Err, err)
	}
	fmt.Fprintln(ports.Out, "logged out")
	return ExitSuccess
}

// refuseUnverifiedLogout deletes the local session without revoking it and
// reports the binding refusal. This mirrors Logout's own binding-error path:
// the token is never sent and the local file does not survive.
func refuseUnverifiedLogout(ctx context.Context, ports Ports, store SessionRecordStore, cause error) int {
	refusal := loginRequired("the session binding could not be verified", cause)
	if deleteErr := deleteSessionRecord(ctx, store); deleteErr != nil {
		return failure(ports.Err, fault.Wrap(fault.Unauthenticated,
			"login required: the session binding could not be verified and its file could not be deleted",
			errors.Join(ErrReauthenticationRequired, cause, deleteErr)))
	}
	return failure(ports.Err, refusal)
}

// runRefresh rotates the session under the session lock and reports the new
// expiry. A session that cannot be refreshed is a visible failure that never
// renders a token.
func runRefresh(ctx context.Context, config Config, args []string, ports Ports, runtime LoginRuntime) int {
	if len(args) != 0 {
		return usage(ports.Err, "refresh takes no arguments")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if runtime.Store == nil || runtime.Lock == nil || runtime.OIDC == nil {
		return failure(ports.Err, fault.New(fault.InternalFailure, "the login runtime is incomplete"))
	}
	apiURL, err := normalizeAPIURL(config.ServerURL)
	if err != nil {
		return usage(ports.Err, err.Error())
	}
	if _, err := runtime.Store.LoadRecord(ctx); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return failure(ports.Err, loginRequired("the session must be established again", nil))
		}
		return failure(ports.Err, fault.Wrap(fault.DependencyUnavailable, "the session could not be loaded", err))
	}
	issuer, clientID, audience, bindErr := trustedBinding(ctx, runtime.OIDC, apiURL,
		runtime.Issuer, runtime.ClientID, runtime.Audience)
	if bindErr != nil {
		return failure(ports.Err, loginRequired("the session binding could not be verified", bindErr))
	}
	updated, err := Refresh(ctx, RefreshDeps{
		APIURL:   apiURL,
		Issuer:   issuer,
		ClientID: clientID,
		Audience: audience,
		OIDC:     runtime.OIDC,
		Clock:    runtime.Clock,
		Lock:     runtime.Lock,
		Store:    runtime.Store,
	})
	if err != nil {
		return failure(ports.Err, err)
	}
	fmt.Fprintf(ports.Out, "session refreshed; issuer %s; access token expires %s\n",
		updated.Issuer, formatSessionTime(updated.ExpiresAt))
	return ExitSuccess
}

// sessionStatus is the non-secret status view. It never carries a token.
type sessionStatus struct {
	Issuer      string `json:"issuer,omitempty"`
	APIURL      string `json:"api_url,omitempty"`
	ExpiresAt   string `json:"expires_at"`
	Refreshable bool   `json:"refreshable"`
}

// runStatus renders the session issuer and expiry without a token.
func runStatus(ctx context.Context, config Config, args []string, ports Ports, runtime LoginRuntime) int {
	if len(args) != 0 {
		return usage(ports.Err, "status takes no arguments")
	}
	if err := config.Validate(); err != nil {
		return usage(ports.Err, err.Error())
	}
	if runtime.Store == nil {
		return failure(ports.Err, fault.New(fault.InternalFailure, "the login runtime is incomplete"))
	}
	record, err := runtime.Store.LoadRecord(ctx)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return failure(ports.Err, loginRequired("the session must be established again", nil))
		}
		return failure(ports.Err, fault.Wrap(fault.DependencyUnavailable, "the session could not be loaded", err))
	}
	view := sessionStatus{
		Issuer:      record.Issuer,
		APIURL:      record.APIURL,
		ExpiresAt:   formatSessionTime(record.ExpiresAt),
		Refreshable: !record.Legacy() && record.RefreshToken != "",
	}
	if config.Output == OutputJSON {
		if err := json.NewEncoder(ports.Out).Encode(view); err != nil {
			return failure(ports.Err, err)
		}
		return ExitSuccess
	}
	fmt.Fprintf(ports.Out, "issuer\t%s\napi_url\t%s\nexpires_at\t%s\nrefreshable\t%t\n",
		view.Issuer, view.APIURL, view.ExpiresAt, view.Refreshable)
	return ExitSuccess
}

// trustedBinding resolves the expected issuer, public client and audience for
// one refresh, logout or credential attempt. Explicit caller expectations win;
// every missing value is filled from ONE discovery of the configured API, so
// the record is never compared to itself. A discovery that fails, is invalid
// or names another API URL is an error; the result is never cached beyond this
// invocation or shared across processes.
func trustedBinding(ctx context.Context, oidc OIDCClient, apiURL, issuer, clientID, audience string) (string, string, string, error) {
	if issuer != "" && clientID != "" && audience != "" {
		return issuer, clientID, audience, nil
	}
	if oidc == nil {
		return "", "", "", fault.New(fault.InternalFailure, "no OIDC client is configured")
	}
	document, err := oidc.Discover(ctx, apiURL+api.DiscoveryPath)
	if err != nil {
		return "", "", "", fault.Wrap(fault.DependencyUnavailable, "the login configuration is unavailable", err)
	}
	if err := document.Validate(); err != nil {
		return "", "", "", fault.Wrap(fault.DependencyUnavailable, "the login configuration is invalid", err)
	}
	if document.APIURL != apiURL {
		return "", "", "", fault.New(fault.DependencyUnavailable, "the login configuration is bound to another API URL")
	}
	if issuer == "" {
		issuer = document.Issuer
	}
	if clientID == "" {
		clientID = document.ClientID
	}
	if audience == "" {
		audience = document.Audience
	}
	return issuer, clientID, audience, nil
}

// formatSessionTime renders one expiry in UTC without any credential material.
func formatSessionTime(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// loginRequired returns the stable unauthenticated "login required" refusal.
// The reauthentication sentinel stays reachable through errors.Is.
func loginRequired(message string, cause error) error {
	if cause == nil {
		return fault.Wrap(fault.Unauthenticated, "login required: "+message, ErrReauthenticationRequired)
	}
	return fault.Wrap(fault.Unauthenticated, "login required: "+message, errors.Join(ErrReauthenticationRequired, cause))
}

// SessionCredentialSource is the session-backed client.CredentialSource. It
// reloads the version 2 record on every attempt and refreshes only inside the
// refresh leeway through Refresh under the shared SessionLock, so two clients
// starting at the same time make at most one token call. The expected issuer,
// client and audience come from explicit caller fields or the configured API's
// own discovery document, never from the record being validated. A legacy
// version 1 record, a missing record, an unverifiable binding and an
// unrefreshable or expired session refuse with a stable unauthenticated
// "login required" error instead of returning a stale access token.
type SessionCredentialSource struct {
	Store    SessionRecordStore
	Lock     SessionLock
	OIDC     OIDCClient
	APIURL   string
	Issuer   string
	ClientID string
	Audience string
	// Now is the injected authoritative time. Clock, when set, is used for the
	// refresh decision and is preferred.
	Now   func() time.Time
	Clock Clock
}

var _ client.CredentialSource = SessionCredentialSource{}

// Credential returns the current human session credential.
func (source SessionCredentialSource) Credential(ctx context.Context) (client.Credential, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return client.Credential{}, loginRequired("the credential read was canceled", err)
	}
	if source.Store == nil {
		return client.Credential{}, loginRequired("no session store is configured", nil)
	}
	record, err := source.Store.LoadRecord(ctx)
	if err != nil {
		return client.Credential{}, loginRequired("the session must be established again", err)
	}
	if record.Legacy() {
		return client.Credential{}, loginRequired("the stored session cannot be refreshed", nil)
	}
	apiURL, err := normalizeAPIURL(source.APIURL)
	if err != nil {
		return client.Credential{}, loginRequired("the configured API URL is invalid", err)
	}
	// The trusted binding comes from the API's own discovery document (or
	// an explicit caller expectation), never from the record being checked.
	issuer, clientID, audience, bindErr := trustedBinding(ctx, source.OIDC, apiURL,
		source.Issuer, source.ClientID, source.Audience)
	if bindErr != nil {
		return client.Credential{}, loginRequired("the session binding could not be verified", bindErr)
	}
	if err := checkSessionBinding(record, apiURL, issuer, clientID, audience); err != nil {
		return client.Credential{}, loginRequired("the session must be established again", err)
	}
	clock, now := source.timeSource()
	if record.NeedsRefresh(now()) {
		if source.Lock == nil || source.OIDC == nil {
			return client.Credential{}, loginRequired("the session cannot be refreshed", nil)
		}
		refreshed, err := Refresh(ctx, RefreshDeps{
			APIURL:   apiURL,
			Issuer:   issuer,
			ClientID: clientID,
			Audience: audience,
			OIDC:     source.OIDC,
			Clock:    clock,
			Lock:     source.Lock,
			Store:    source.Store,
		})
		if err != nil {
			if errors.Is(err, ErrReauthenticationRequired) {
				return client.Credential{}, loginRequired("the session must be established again", err)
			}
			return client.Credential{}, err
		}
		record = refreshed
	}
	if !record.Usable(now()) {
		return client.Credential{}, loginRequired("the session is expired", nil)
	}
	return client.Credential{
		Kind:      client.KindSession,
		Token:     record.Token,
		ExpiresAt: record.ExpiresAt,
	}, nil
}

// timeSource returns the injected refresh clock and authoritative time.
func (source SessionCredentialSource) timeSource() (Clock, func() time.Time) {
	if source.Clock != nil {
		return source.Clock, source.Clock.Now
	}
	if source.Now != nil {
		return sourceNowClock{now: source.Now}, source.Now
	}
	return nil, time.Now
}

// sourceNowClock adapts a bare Now function to the Clock port. A credential
// source never sleeps.
type sourceNowClock struct {
	now func() time.Time
}

func (clock sourceNowClock) Now() time.Time { return clock.now() }

func (sourceNowClock) Sleep(context.Context, time.Duration) error { return nil }
