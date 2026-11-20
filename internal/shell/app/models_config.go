package app

// Trusted configuration of the optional configured model gateway.
//
// The configuration is app-owned: every entry names one project, one caller
// alias, one upstream model, one provider endpoint and exactly one declared
// secret. It never carries a secret value, a request-supplied endpoint or a
// permission. A nil configuration keeps the optional surface disabled with the
// not_found refusal and constructs no dependency.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/id"
	"github.com/optimiware/kodema/internal/shell/app/bridges"
	"github.com/optimiware/kodema/internal/shell/gateway"
	"github.com/optimiware/kodema/internal/shell/secret"
	"github.com/optimiware/kodema/internal/shell/tenancy"
)

const (
	// DefaultLLMProxyMaxRequestBytes is the default 16 MiB request body cap for
	// streaming coding-agent LLM proxy calls (/llm/{provider}/*).
	DefaultLLMProxyMaxRequestBytes = 16 << 20
	// MaxLLMProxyRequestBytes is the upper bound on a configured LLM proxy
	// request body cap.
	MaxLLMProxyRequestBytes = 16 << 20
	// DefaultAnthropicVersion is the default anthropic-version header injected
	// on Anthropic proxy calls when neither the caller nor the provider
	// configuration specifies one.
	DefaultAnthropicVersion = "2023-06-01"
)

// ModelProviderConfig configures one upstream LLM provider for the streaming
// workload LLM proxy (/llm/{provider}/*).
type ModelProviderConfig struct {
	// Scope is the project or tenant-wide scope this provider entry serves. A
	// project-scoped entry takes precedence over a tenant-wide fallback in the
	// same tenant.
	Scope tenancy.Scope
	// Name is the lowercase URL slug matched by /llm/{provider}/* (such as
	// "anthropic" or "openai").
	Name string
	// BaseURL is the upstream provider origin/prefix (for example
	// "https://api.anthropic.com" or "https://api.openai.com"). When empty,
	// BaseURL is derived from Endpoint.
	BaseURL string
	// Endpoint is an optional full completion endpoint URL (for example
	// "https://api.openai.com/v1/chat/completions") from which the upstream
	// base URL is derived when BaseURL is empty.
	Endpoint string
	// SecretName is the one declared secret resolved per call through the
	// gateway secret policy in the execution's project scope.
	SecretName id.SecretName
	// AuthHeader optionally selects the outbound credential header:
	// "Authorization" ("Bearer <secret>") or "x-api-key" ("<secret>"). When
	// empty, "anthropic" (or an *.anthropic.com host) uses "x-api-key" and all
	// other providers use "Authorization".
	AuthHeader string
	// AnthropicVersion optionally sets the default anthropic-version header
	// when the caller omits one.
	AnthropicVersion string
	// MaxRequestBytes optionally overrides DefaultLLMProxyMaxRequestBytes
	// within (0, MaxLLMProxyRequestBytes].
	MaxRequestBytes int
	// Timeout optionally bounds one proxy call when shorter than the active
	// action's remaining deadline.
	Timeout time.Duration
}

// ModelsConfig is the complete trusted configuration of the optional model
// gateway and streaming workload LLM proxy. A non-nil value with neither
// configured models nor configured providers is incomplete and refused before
// any adapter is constructed.
type ModelsConfig struct {
	// Models lists the configured model allow-list. Every entry names exactly
	// one project and a unique caller alias inside that project.
	Models []gateway.ModelConfig
	// Providers lists the configured upstream providers exposed on the
	// workload-only streaming LLM proxy (/llm/{provider}/*).
	Providers []ModelProviderConfig
	// SecretRules is the model-gateway secret-name allow-list. It is a separate
	// consumer policy from Config.SecretRules (the workload allow-list), so a
	// workload rule can never authorize a provider credential.
	SecretRules []secret.NameRule
	// Transport is an optional caller-supplied provider transport for a
	// development or test composition. The composition adopts it and stops it
	// at shutdown. Production refuses it and builds its own TLS-verifying
	// transport.
	Transport http.RoundTripper
}

type providerScopeKey struct {
	scope tenancy.Scope
	name  string
}

// Validate checks the complete trusted configuration before any adapter exists.
func (config *ModelsConfig) Validate(profile Profile) error {
	return config.validate(profile)
}

// validate checks the complete trusted configuration before any adapter exists.
// Every ambiguity is a refusal: last-entry-wins and a silently dropped or
// weakened endpoint are never resolved.
func (config *ModelsConfig) validate(profile Profile) error {
	if config == nil {
		return nil
	}
	if len(config.Models) > 0 || len(config.Providers) == 0 {
		if err := config.gatewayConfig().Validate(); err != nil {
			return err
		}
	}
	seenProviders := make(map[providerScopeKey]bool, len(config.Providers))
	for index, provider := range config.Providers {
		if err := provider.validate(index); err != nil {
			return err
		}
		key := providerScopeKey{scope: provider.Scope, name: provider.Name}
		if seenProviders[key] {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("configured model provider %q is declared twice in one scope", provider.Name))
		}
		seenProviders[key] = true
	}
	// The gateway allow-list is validated as the gateway consumer even when a
	// model names no secret, so a malformed rule cannot hide behind an unused
	// policy.
	if _, err := bridges.NewGatewayNamePolicy(config.SecretRules); err != nil {
		return err
	}
	if profile != ProfileProduction {
		return nil
	}
	// Production reaches configured providers only over verified TLS. A caller
	// supplied transport could replace that verification, so it is refused
	// rather than trusted.
	if config.Transport != nil {
		return fault.New(fault.InvalidInput, "production composition refuses an injected model transport")
	}
	for index, model := range config.Models {
		if !strings.HasPrefix(strings.ToLower(model.Endpoint), "https://") {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("production configured model %d endpoint must be a verified https URL", index+1))
		}
	}
	for index, provider := range config.Providers {
		if provider.BaseURL != "" && !strings.HasPrefix(strings.ToLower(provider.BaseURL), "https://") {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("production configured model provider %d base_url must be a verified https URL", index+1))
		}
		if provider.Endpoint != "" && !strings.HasPrefix(strings.ToLower(provider.Endpoint), "https://") {
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("production configured model provider %d endpoint must be a verified https URL", index+1))
		}
	}
	return nil
}

// enabledForScope reports whether this configuration enables configured models
// or LLM proxy providers for the given execution scope.
func (config *ModelsConfig) enabledForScope(scope tenancy.Scope) bool {
	if config == nil {
		return false
	}
	for _, model := range config.Models {
		if model.Scope == scope {
			return true
		}
	}
	for _, provider := range config.Providers {
		if provider.Scope == scope || (provider.Scope.TenantID == scope.TenantID && provider.Scope.ProjectID == "") {
			return true
		}
	}
	return false
}

// providersForScope returns the effective LLM proxy providers enabled for
// scope, sorted deterministically by provider name. A project-scoped entry
// overrides a tenant-wide entry of the same name.
func (config *ModelsConfig) providersForScope(scope tenancy.Scope) []ModelProviderConfig {
	if config == nil || len(config.Providers) == 0 {
		return nil
	}
	byName := make(map[string]ModelProviderConfig, len(config.Providers))
	for _, provider := range config.Providers {
		if provider.Scope.TenantID == scope.TenantID && provider.Scope.ProjectID == "" {
			byName[provider.Name] = provider
		}
	}
	if scope.ProjectScoped() {
		for _, provider := range config.Providers {
			if provider.Scope == scope {
				byName[provider.Name] = provider
			}
		}
	}
	if len(byName) == 0 {
		return nil
	}
	providers := make([]ModelProviderConfig, 0, len(byName))
	for _, provider := range byName {
		providers = append(providers, provider)
	}
	sort.Slice(providers, func(i, j int) bool {
		return providers[i].Name < providers[j].Name
	})
	return providers
}

// EnvLLMProviderBaseURL returns the sanitized workload environment variable
// name for one LLM proxy provider: KODEMA_LLM_<NORMALIZED_PROVIDER>_BASE_URL.
// Non-alphanumeric characters are collapsed to single underscores and uppercased.
// Empty, oversized, invalid, or standard KODEMA_* runtime names return false.
func EnvLLMProviderBaseURL(providerName string) (id.EnvName, bool) {
	segment := normalizeProviderEnvSegment(providerName)
	if segment == "" {
		return "", false
	}
	candidate := "KODEMA_LLM_" + segment + "_BASE_URL"
	parsed, err := id.ParseEnvName(candidate)
	if err != nil {
		return "", false
	}
	switch string(parsed) {
	case id.EnvURL, id.EnvExecution, id.EnvAction, id.EnvTask, id.SecretTokenName, "KODEMA_CA_FILE", "KODEMA_GIT_URL":
		return "", false
	}
	return parsed, true
}

func normalizeProviderEnvSegment(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	lastUnderscore := true
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		switch {
		case ch >= 'a' && ch <= 'z':
			b.WriteByte(ch - ('a' - 'A'))
			lastUnderscore = false
		case (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9'):
			b.WriteByte(ch)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	return strings.Trim(b.String(), "_")
}

// proxyBaseURLForProvider returns the container-facing base URL exposed in
// KODEMA_LLM_<PROVIDER>_BASE_URL. Anthropic-style providers (x-api-key) omit
// /v1 because Anthropic SDKs append /v1/messages; OpenAI-compatible providers
// include /v1 because OpenAI SDKs append /chat/completions or /responses.
func proxyBaseURLForProvider(workloadBaseURL string, provider ModelProviderConfig) string {
	base := strings.TrimRight(strings.TrimSpace(workloadBaseURL), "/")
	parsed, err := deriveProviderBaseURL(provider.BaseURL, provider.Endpoint)
	if err == nil && providerUsesXAPIKey(provider, parsed) {
		return base + "/llm/" + provider.Name
	}
	return base + "/llm/" + provider.Name + "/v1"
}

// llmProxySecretRules returns the gateway-consumer secret-name rules for the
// streaming LLM proxy. Explicit ModelsConfig.SecretRules are preserved as-is;
// when omitted on a provider configuration, exact ConsumerGateway rules are
// derived strictly from the trusted provider declarations so the proxy can
// resolve its configured provider secrets without widening workload policies.
func llmProxySecretRules(config *ModelsConfig) []secret.NameRule {
	if config == nil {
		return nil
	}
	if len(config.SecretRules) > 0 {
		return config.SecretRules
	}
	if len(config.Providers) == 0 {
		return nil
	}
	byScope := make(map[tenancy.Scope]map[id.SecretName]struct{}, len(config.Providers))
	scopes := make([]tenancy.Scope, 0, len(config.Providers))
	for _, provider := range config.Providers {
		names, ok := byScope[provider.Scope]
		if !ok {
			names = make(map[id.SecretName]struct{})
			byScope[provider.Scope] = names
			scopes = append(scopes, provider.Scope)
		}
		names[provider.SecretName] = struct{}{}
	}
	rules := make([]secret.NameRule, 0, len(scopes))
	for _, scope := range scopes {
		nameSet := byScope[scope]
		names := make([]id.SecretName, 0, len(nameSet))
		for name := range nameSet {
			names = append(names, name)
		}
		sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
		rules = append(rules, secret.NameRule{Scope: scope, Names: names})
	}
	return rules
}

type ownedSecretKey struct {
	scope tenancy.Scope
	name  id.SecretName
}

// proxyOwnedSecretPolicy wraps the workload secret NamePolicy to prevent
// container workloads from materializing raw provider credentials owned by
// configured LLM proxy providers or configured models in the same scope, even
// when workload SecretRules includes allow_all.
type proxyOwnedSecretPolicy struct {
	inner   secret.NamePolicy
	blocked map[ownedSecretKey]struct{}
}

var _ secret.NamePolicy = (*proxyOwnedSecretPolicy)(nil)

func newProxyOwnedSecretPolicy(inner secret.NamePolicy, models *ModelsConfig) secret.NamePolicy {
	if models == nil || (len(models.Providers) == 0 && len(models.Models) == 0) {
		return inner
	}
	blocked := make(map[ownedSecretKey]struct{}, len(models.Providers)+len(models.Models))
	for _, provider := range models.Providers {
		if provider.SecretName != "" {
			blocked[ownedSecretKey{scope: provider.Scope, name: provider.SecretName}] = struct{}{}
		}
	}
	for _, model := range models.Models {
		if model.SecretName != "" {
			blocked[ownedSecretKey{scope: model.Scope, name: model.SecretName}] = struct{}{}
		}
	}
	if len(blocked) == 0 {
		return inner
	}
	return &proxyOwnedSecretPolicy{inner: inner, blocked: blocked}
}

func (policy *proxyOwnedSecretPolicy) Check(ctx context.Context, scope tenancy.Scope, name id.SecretName) error {
	if policy != nil {
		if _, reserved := policy.blocked[ownedSecretKey{scope: scope, name: name}]; reserved {
			return fault.New(fault.Forbidden,
				fmt.Sprintf("secret %q is managed by the LLM proxy and cannot be mounted into workload containers", name))
		}
		if scope.ProjectScoped() {
			tenantScope := tenancy.Scope{TenantID: scope.TenantID}
			if _, reserved := policy.blocked[ownedSecretKey{scope: tenantScope, name: name}]; reserved {
				return fault.New(fault.Forbidden,
					fmt.Sprintf("secret %q is managed by the LLM proxy and cannot be mounted into workload containers", name))
			}
		}
	}
	if policy == nil || policy.inner == nil {
		return fault.New(fault.Forbidden, fmt.Sprintf("secret %q is not authorized by server secret rules", name))
	}
	return policy.inner.Check(ctx, scope, name)
}

func (provider ModelProviderConfig) validate(index int) error {
	position := fmt.Sprintf("configured model provider %d", index+1)
	if err := provider.Scope.Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, position+" scope is invalid", err)
	}
	if _, err := id.ParseActionName(provider.Name); err != nil {
		return fault.Wrap(fault.InvalidInput, position+" name must be a lowercase slug", err)
	}
	if _, err := deriveProviderBaseURL(provider.BaseURL, provider.Endpoint); err != nil {
		return fault.Wrap(fault.InvalidInput, position+" "+err.Error(), err)
	}
	if _, err := id.ParseSecretName(string(provider.SecretName)); err != nil {
		return fault.Wrap(fault.InvalidInput, position+" secret name is invalid", err)
	}
	resolveReq := secret.ResolveRequest{Scope: provider.Scope, Names: []id.SecretName{provider.SecretName}}
	if err := resolveReq.Validate(); err != nil {
		return fault.Wrap(fault.InvalidInput, position+" secret reference is invalid", err)
	}
	switch strings.ToLower(strings.TrimSpace(provider.AuthHeader)) {
	case "", "authorization", "x-api-key":
	default:
		return fault.New(fault.InvalidInput,
			position+` auth_header must be empty, "Authorization" or "x-api-key"`)
	}
	if strings.ContainsAny(provider.AnthropicVersion, "\r\n\x00") {
		return fault.New(fault.InvalidInput, position+" anthropic_version contains control characters")
	}
	if provider.MaxRequestBytes < 0 || provider.MaxRequestBytes > MaxLLMProxyRequestBytes {
		return fault.New(fault.InvalidInput,
			fmt.Sprintf("%s request bound %d is outside [0, %d]", position, provider.MaxRequestBytes, MaxLLMProxyRequestBytes))
	}
	if provider.Timeout < 0 {
		return fault.New(fault.InvalidInput, position+" timeout is negative")
	}
	return nil
}

// knownProviderEndpointSuffixes lists standard completion/responses leaf paths
// stripped when deriving a provider base URL from a full Endpoint URL.
var knownProviderEndpointSuffixes = []string{
	"/v1/chat/completions",
	"/v1/responses",
	"/v1/messages",
	"/v1/completions",
	"/chat/completions",
	"/responses",
	"/messages",
}

// deriveProviderBaseURL validates and resolves the upstream base URL of one
// ModelProviderConfig from BaseURL and/or Endpoint.
func deriveProviderBaseURL(baseURL, endpoint string) (*url.URL, error) {
	if baseURL == "" && endpoint == "" {
		return nil, fault.New(fault.InvalidInput, "base_url or endpoint is required")
	}
	var parsedBase *url.URL
	if baseURL != "" {
		parsed, err := parseProviderConfigURL(baseURL, "base_url")
		if err != nil {
			return nil, err
		}
		parsed.Path = strings.TrimSuffix(parsed.Path, "/")
		parsed.RawPath = ""
		parsedBase = parsed
	}
	if endpoint != "" {
		parsedEndpoint, err := parseProviderConfigURL(endpoint, "endpoint")
		if err != nil {
			return nil, err
		}
		if parsedBase != nil {
			if !strings.EqualFold(parsedBase.Scheme, parsedEndpoint.Scheme) ||
				!strings.EqualFold(parsedBase.Host, parsedEndpoint.Host) {
				return nil, fault.New(fault.InvalidInput, "base_url and endpoint name different origins")
			}
			return parsedBase, nil
		}
		trimmed := strings.TrimSuffix(parsedEndpoint.Path, "/")
		for _, suffix := range knownProviderEndpointSuffixes {
			if strings.HasSuffix(trimmed, suffix) {
				trimmed = strings.TrimSuffix(trimmed, suffix)
				break
			}
		}
		parsedEndpoint.Path = strings.TrimSuffix(trimmed, "/")
		parsedEndpoint.RawPath = ""
		return parsedEndpoint, nil
	}
	return parsedBase, nil
}

func parseProviderConfigURL(raw, field string) (*url.URL, error) {
	if raw == "" {
		return nil, fault.New(fault.InvalidInput, field+" is empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return nil, fault.New(fault.InvalidInput, field+" contains whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput, field+" is invalid", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return nil, fault.New(fault.InvalidInput, field+" is not an http(s) URL")
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" || strings.Contains(parsed.Host, "@") {
		return nil, fault.New(fault.InvalidInput, field+" has no usable host")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return nil, fault.New(fault.InvalidInput, field+" must not carry a query or fragment; the provider secret is resolved per call")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		trimmed := strings.TrimSuffix(parsed.Path, "/")
		for _, segment := range strings.Split(strings.TrimPrefix(trimmed, "/"), "/") {
			if segment == "" || segment == "." || segment == ".." {
				return nil, fault.New(fault.InvalidInput, field+" path contains an empty or relative segment")
			}
		}
	}
	parsed.Scheme = scheme
	return parsed, nil
}

// gatewayConfig renders the configuration into the gateway's own value. The
// slice is copied so a later caller mutation cannot change a constructed
// gateway.
func (config *ModelsConfig) gatewayConfig() gateway.Config {
	if config == nil {
		return gateway.Config{}
	}
	return gateway.Config{
		Enabled: true,
		Models:  append([]gateway.ModelConfig(nil), config.Models...),
	}
}
