package app

// Trusted configuration of the optional configured model gateway.
//
// The configuration is app-owned: every entry names one project, one caller
// alias, one upstream model, one provider endpoint and exactly one declared
// secret. It never carries a secret value, a request-supplied endpoint or a
// permission. A nil configuration keeps the optional surface disabled with the
// not_found refusal and constructs no dependency.

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/app/bridges"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
)

// ModelsConfig is the complete trusted configuration of the optional model
// gateway. A non-nil value with no models is incomplete and refused before any
// adapter is constructed.
type ModelsConfig struct {
	// Models lists the configured model allow-list. Every entry names exactly
	// one project and a unique caller alias inside that project.
	Models []gateway.ModelConfig
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

// validate checks the complete trusted configuration before any adapter exists.
// Every ambiguity is a refusal: last-entry-wins and a silently dropped or
// weakened endpoint are never resolved.
func (config *ModelsConfig) validate(profile Profile) error {
	if config == nil {
		return nil
	}
	if err := config.gatewayConfig().Validate(); err != nil {
		return err
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
	return nil
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
