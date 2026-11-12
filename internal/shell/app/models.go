package app

// Composition of the optional configured model gateway.
//
// One gateway is built from the explicit trusted configuration with the
// gateway package, the request-local binding authenticator, the gateway secret
// policy and resolver, an execution reader that grants only
// ExecConfiguredModels, the shared current-authority reader, the neutral usage
// store and the shared identity/clock seams. The public port binds the already
// authenticated principal to a request-local credential and delegates one
// bounded invocation. No caller-supplied credential, endpoint, secret name or
// permission can widen that selection.

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	opsapp "github.com/optimiware/kodama/internal/shell/app/ops"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/store"
	"github.com/optimiware/kodama/internal/shell/store/units/platform"
)

// modelGatewayDependencies is the complete set of trusted configuration and
// injected boundaries one enabled gateway needs. The disabled path ignores
// every field.
type modelGatewayDependencies struct {
	Config         *ModelsConfig
	Profile        Profile
	InstallationID string
	Controller     store.Controller
	Credentials    *bindingAuthenticator
	Authorizer     authorize.Authorizer
	Authority      authorize.AuthorityReader
	Lookup         secret.NameLookup
	Cipher         secret.EnvelopeCipher
	Store          store.Executor
	IDs            *bridges.IDs
	Clock          Clock
}

// configuredModels is the enabled opsapp.ModelInvocation port.
type configuredModels struct {
	credentials *bindingAuthenticator
	instance    *gateway.Gateway
}

var _ opsapp.ModelInvocation = (*configuredModels)(nil)

// newConfiguredModels builds the optional model port. A nil configuration keeps
// the existing disabled refusal and constructs no policy, resolver, execution
// reader or transport. An enabled but incomplete configuration is a
// construction failure. A non-nil cleanup is returned exactly when the
// composition adopted a transport to stop at shutdown.
func newConfiguredModels(deps modelGatewayDependencies) (opsapp.ModelInvocation, func(context.Context) error, error) {
	if deps.Config == nil {
		return disabledModels{}, nil, nil
	}
	if err := deps.Config.validate(deps.Profile); err != nil {
		return nil, nil, err
	}
	switch {
	case deps.InstallationID == "":
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires an installation id")
	case deps.Controller == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires a controller")
	case deps.Credentials == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires the request-local binding authenticator")
	case deps.Authorizer == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires an authorizer")
	case deps.Authority == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires an authority reader")
	case deps.Lookup == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires a secret name lookup")
	case deps.Cipher == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires an envelope cipher")
	case deps.Store == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires a usage store")
	case deps.IDs == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires an identity source")
	case deps.Clock == nil:
		return nil, nil, fault.New(fault.InvalidInput, "model gateway requires a clock")
	}

	policy, err := bridges.NewGatewayNamePolicy(deps.Config.SecretRules)
	if err != nil {
		return nil, nil, err
	}
	secrets, err := secret.NewResolver(secret.ResolverConfig{
		InstallationID: deps.InstallationID,
		Policy:         policy,
		Lookup:         deps.Lookup,
		Cipher:         deps.Cipher,
	})
	if err != nil {
		return nil, nil, err
	}
	permissions, err := bridges.NewPermissionSource(authorize.ExecConfiguredModels, deps.Clock)
	if err != nil {
		return nil, nil, err
	}
	executions, err := platform.NewExecutionReader(deps.Controller, permissions)
	if err != nil {
		return nil, nil, err
	}
	transport, stop := deps.Config.transportFor()
	instance, err := gateway.New(deps.Config.gatewayConfig(), gateway.Dependencies{
		Principals: deps.Credentials,
		Authorizer: deps.Authorizer,
		Authority:  deps.Authority,
		Secrets:    secrets,
		Store:      deps.Store,
		Content: gateway.StoreContentRecorder{
			Executor: deps.Store,
			NewID:    deps.IDs.NewContentID,
		},
		Transport: transport,
		Clock:     deps.Clock,
		NewID:     deps.IDs.NewUsageID,
	}, gateway.WithExecutionReader(executions))
	// Every dependency and configuration field gateway.New validates was
	// checked above, so this error is unreachable; there is no adopted-resource
	// cleanup path to take here. A later startup failure is stopped through the
	// builder's stop list.
	if err != nil {
		return nil, nil, err
	}
	return &configuredModels{credentials: deps.Credentials, instance: instance}, stop, nil
}

// Explicit provider transport defaults. They mirror the documented net/http
// defaults, but the composition builds them itself so no process-wide ambient
// transport can determine a credential-bearing provider request.
const (
	modelProviderDialTimeout           = 30 * time.Second
	modelProviderDialKeepAlive         = 30 * time.Second
	modelProviderMaxIdleConns          = 100
	modelProviderIdleConnTimeout       = 90 * time.Second
	modelProviderTLSHandshakeTimeout   = 10 * time.Second
	modelProviderExpectContinueTimeout = 1 * time.Second
)

// newModelProviderTransport builds the composition-owned provider transport.
// It deliberately does not clone http.DefaultTransport: an environment proxy
// (HTTPS_PROXY/https_proxy/ALL_PROXY) or a process-wide TLS override must never
// observe, route or weaken a credential-bearing provider call. Proxy is nil,
// the TLS client config is the standard verifying one over the system roots
// with a TLS 1.2 floor, and every dial, handshake, idle and expect-continue
// timeout is explicit.
func newModelProviderTransport() *http.Transport {
	return &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   modelProviderDialTimeout,
			KeepAlive: modelProviderDialKeepAlive,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          modelProviderMaxIdleConns,
		IdleConnTimeout:       modelProviderIdleConnTimeout,
		TLSHandshakeTimeout:   modelProviderTLSHandshakeTimeout,
		ExpectContinueTimeout: modelProviderExpectContinueTimeout,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// transportFor returns the provider transport and its owned cleanup. With no
// explicit transport the composition builds its own explicit transport that
// always verifies TLS (system roots, no insecure skip) and never honours an
// environment or process-wide proxy. A supplied development/test transport is
// adopted and stopped through the same cleanup.
func (config *ModelsConfig) transportFor() (http.RoundTripper, func(context.Context) error) {
	if config != nil && config.Transport != nil {
		transport := config.Transport
		return transport, func(context.Context) error {
			switch candidate := transport.(type) {
			case io.Closer:
				return candidate.Close()
			case interface{ CloseIdleConnections() }:
				candidate.CloseIdleConnections()
				return nil
			default:
				return nil
			}
		}
	}
	owned := newModelProviderTransport()
	return owned, func(context.Context) error {
		owned.CloseIdleConnections()
		return nil
	}
}

// Invoke binds the already authenticated principal to a request-local
// credential and delegates one bounded invocation. The gateway re-authenticates
// that credential and re-reads current authority itself, so a stale principal
// or a caller-supplied permission never grants provider access.
func (models *configuredModels) Invoke(
	ctx context.Context,
	principal authorize.Principal,
	projectID id.ProjectID,
	request api.ChatCompletionRequest,
) (api.ChatCompletionResponse, error) {
	if models == nil || models.instance == nil || models.credentials == nil {
		return api.ChatCompletionResponse{}, fault.New(fault.NotFound, "the optional model gateway is not enabled")
	}
	var (
		response api.ChatCompletionResponse
		callErr  error
	)
	if err := models.credentials.withPrincipal(principal, func(credential string) error {
		response, callErr = models.instance.Invoke(ctx, gateway.Invocation{
			Credential: credential,
			ProjectID:  projectID,
			Request:    request,
		})
		return nil
	}); err != nil {
		return api.ChatCompletionResponse{}, err
	}
	return response, callErr
}
