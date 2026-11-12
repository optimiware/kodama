package app

import (
	"context"
	"slices"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/integration/oidc"
	"github.com/optimiware/kodama/internal/shell/transport/httpapi"
)

// DiscoveryConfig is the optional public login discovery configuration.
//
// The document is public: it carries the public client id, the OAuth endpoints,
// the granted scopes and the public API base URL, and it never carries a client
// secret or any key material. The issuer and audience are the same values the
// OIDC access-token verifier uses. Production reuses ProductionConfig.OIDC and
// refuses an explicit copy that differs; development and test compositions have
// no verifier, so there the fields are the single declared source.
type DiscoveryConfig struct {
	// Issuer is the authorization server issuer. Production reuses the OIDC
	// verifier issuer when this is empty and refuses a differing value.
	Issuer string
	// ClientID is the public OAuth client id. It is not a secret.
	ClientID string
	// Audience is the API audience the issued access tokens target. Production
	// reuses the OIDC verifier audience when this is empty and refuses a
	// differing value.
	Audience string
	// AuthorizationEndpoint is the browser authorization-code endpoint.
	AuthorizationEndpoint string
	// TokenEndpoint is the code/refresh token exchange endpoint.
	TokenEndpoint string
	// DeviceAuthorizationEndpoint is the RFC 8628 device authorization endpoint.
	DeviceAuthorizationEndpoint string
	// RevocationEndpoint is the RFC 7009 token revocation endpoint.
	RevocationEndpoint string
	// Scopes are the scopes a login requests.
	Scopes []string
	// APIURL is the public API base URL clients authenticate against. It is a
	// client configuration target and may live on another https origin.
	APIURL string
}

// document copies the configured fields into one document candidate. Issuer
// and audience are filled in (or checked) against the OIDC verifier by
// Config.discoveryDocument for the production profile.
func (config *DiscoveryConfig) document() api.DiscoveryDocument {
	if config == nil {
		return api.DiscoveryDocument{}
	}
	return api.DiscoveryDocument{
		Issuer:                      config.Issuer,
		ClientID:                    config.ClientID,
		Audience:                    config.Audience,
		AuthorizationEndpoint:       config.AuthorizationEndpoint,
		TokenEndpoint:               config.TokenEndpoint,
		DeviceAuthorizationEndpoint: config.DeviceAuthorizationEndpoint,
		RevocationEndpoint:          config.RevocationEndpoint,
		Scopes:                      append([]string(nil), config.Scopes...),
		APIURL:                      config.APIURL,
	}
}

// discoveryConfigEqual reports whether two explicit discovery configurations
// declare the same document. It is used to refuse one document configured
// inconsistently in Config and ProductionConfig.
func discoveryConfigEqual(left, right *DiscoveryConfig) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Issuer == right.Issuer &&
		left.ClientID == right.ClientID &&
		left.Audience == right.Audience &&
		left.AuthorizationEndpoint == right.AuthorizationEndpoint &&
		left.TokenEndpoint == right.TokenEndpoint &&
		left.DeviceAuthorizationEndpoint == right.DeviceAuthorizationEndpoint &&
		left.RevocationEndpoint == right.RevocationEndpoint &&
		left.APIURL == right.APIURL &&
		slices.Equal(left.Scopes, right.Scopes)
}

// discoveryDocument resolves, validates and copies the optional public
// discovery document. The second result reports whether one is configured.
//
// In production the issuer and audience are the OIDC verifier's values: an
// omitted copy is filled from OIDC and an explicit differing copy is refused,
// so the served document can never drift from the tokens the API verifies.
// api.DiscoveryDocument.Validate is the one strict check every profile uses.
func (config Config) discoveryDocument() (api.DiscoveryDocument, bool, error) {
	discovery := config.Discovery
	if config.Profile == ProfileProduction && config.Production != nil {
		production := config.Production.Discovery
		if discovery != nil && production != nil && !discoveryConfigEqual(discovery, production) {
			return api.DiscoveryDocument{}, false, fault.New(fault.InvalidInput,
				"discovery configuration is declared differently in the composition and the production configuration")
		}
		if production != nil {
			discovery = production
		}
	}
	if discovery == nil {
		return api.DiscoveryDocument{}, false, nil
	}

	document := discovery.document()
	if verifier, ok := config.discoveryVerifier(); ok {
		if document.Issuer != "" && verifier.Issuer != "" && document.Issuer != verifier.Issuer {
			return api.DiscoveryDocument{}, false, fault.New(fault.InvalidInput,
				"discovery issuer differs from the OIDC verifier issuer")
		}
		if document.Audience != "" && verifier.Audience != "" && document.Audience != verifier.Audience {
			return api.DiscoveryDocument{}, false, fault.New(fault.InvalidInput,
				"discovery audience differs from the OIDC verifier audience")
		}
		// The verifier configuration is the single source of truth.
		if verifier.Issuer != "" {
			document.Issuer = verifier.Issuer
		}
		if verifier.Audience != "" {
			document.Audience = verifier.Audience
		}
	}
	if err := document.Validate(); err != nil {
		// The validation detail names a member or origin rule, never a value or
		// credential, so it is safe as a startup diagnostic.
		return api.DiscoveryDocument{}, false, fault.Wrap(fault.InvalidInput,
			"public discovery configuration is invalid: "+err.Error(), err)
	}
	return document, true, nil
}

// staticDiscovery is the composition-owned DiscoveryProvider. It serves one
// document validated at startup and returns a fresh copy on every call, so a
// caller can never mutate the shared configuration or another response.
type staticDiscovery struct {
	document api.DiscoveryDocument
}

var _ httpapi.DiscoveryProvider = (*staticDiscovery)(nil)

// newStaticDiscovery validates and copies the document once at construction.
func newStaticDiscovery(document api.DiscoveryDocument) (*staticDiscovery, error) {
	if err := document.Validate(); err != nil {
		return nil, fault.Wrap(fault.InvalidInput, "public discovery document is invalid: "+err.Error(), err)
	}
	return &staticDiscovery{document: copyDiscoveryDocument(document)}, nil
}

// Discovery returns a private copy of the startup document.
func (provider *staticDiscovery) Discovery(ctx context.Context) (api.DiscoveryDocument, error) {
	if err := ctx.Err(); err != nil {
		return api.DiscoveryDocument{}, err
	}
	return copyDiscoveryDocument(provider.document), nil
}

// copyDiscoveryDocument deep-copies the one slice the document owns.
func copyDiscoveryDocument(document api.DiscoveryDocument) api.DiscoveryDocument {
	copied := document
	copied.Scopes = append([]string(nil), document.Scopes...)
	return copied
}

// discoveryVerifier returns the OIDC verifier configuration a durable
// composition authenticates humans with, so the served discovery document can
// never drift from the tokens the API verifies.
func (config Config) discoveryVerifier() (oidc.Config, bool) {
	switch {
	case config.Profile == ProfileProduction && config.Production != nil:
		return config.Production.OIDC, true
	case config.Profile == ProfileTest && config.Test != nil:
		return config.Test.OIDC, true
	default:
		return oidc.Config{}, false
	}
}
