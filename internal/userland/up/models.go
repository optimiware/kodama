package up

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/optimiware/kodema/internal/id"
)

const (
	modelsFileName          = "models.json"
	maxLocalModelsFileBytes = 1 << 20

	// DefaultOpenAIBaseURL is the built-in upstream base URL for the OpenAI
	// LLM proxy provider (/llm/openai/*).
	DefaultOpenAIBaseURL = "https://api.openai.com"
	// DefaultAnthropicBaseURL is the built-in upstream base URL for the
	// Anthropic LLM proxy provider (/llm/anthropic/*).
	DefaultAnthropicBaseURL = "https://api.anthropic.com"
	// DefaultOpenCodeBaseURL is the built-in upstream base URL for OpenCode Zen
	// (/llm/opencode/*).
	DefaultOpenCodeBaseURL = "https://opencode.ai/inference/openai/v1"
)

// LLMProviderSpec describes one streaming LLM proxy provider configured for a
// local `kodema up` stack.
type LLMProviderSpec struct {
	Name       string
	BaseURL    string
	SecretName string
	AuthHeader string
}

type localModelsFile struct {
	Providers   []localModelProvider     `json:"providers"`
	SecretRules []localGatewaySecretRule `json:"secret_rules"`
}

type localModelProvider struct {
	TenantID   string `json:"tenant_id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	SecretName string `json:"secret_name"`
	AuthHeader string `json:"auth_header,omitempty"`
}

type localGatewaySecretRule struct {
	TenantID string   `json:"tenant_id"`
	Names    []string `json:"names"`
}

// DefaultLLMProviders returns the built-in LLM proxy providers wired out of the
// box by `kodema up`: openai, anthropic, and opencode (OpenCode Zen).
func DefaultLLMProviders() []LLMProviderSpec {
	return []LLMProviderSpec{
		{
			Name:       "openai",
			BaseURL:    DefaultOpenAIBaseURL,
			SecretName: "OPENAI_API_KEY",
		},
		{
			Name:       "anthropic",
			BaseURL:    DefaultAnthropicBaseURL,
			SecretName: "ANTHROPIC_API_KEY",
			AuthHeader: "x-api-key",
		},
		{
			Name:       "opencode",
			BaseURL:    DefaultOpenCodeBaseURL,
			SecretName: "OPENCODE_API_KEY",
		},
	}
}

// ParseLLMProviderSpec parses a CLI `--llm-provider` value of the form:
//
//	<name>=<base_url>[,secret=<SECRET_NAME>][,auth=<Authorization|x-api-key>]
//
// When `secret=...` is omitted, the secret name defaults to
// `<NORMALIZED_NAME>_API_KEY` (for example `openrouter` -> `OPENROUTER_API_KEY`).
func ParseLLMProviderSpec(raw string) (LLMProviderSpec, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider: value is empty")
	}
	parts := strings.Split(trimmed, ",")
	name, baseURL, ok := strings.Cut(parts[0], "=")
	if !ok {
		return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: expected <name>=<base_url>[,secret=<NAME>][,auth=<Authorization|x-api-key>]", raw)
	}
	spec := LLMProviderSpec{
		Name:    strings.TrimSpace(name),
		BaseURL: strings.TrimSpace(baseURL),
	}
	for _, option := range parts[1:] {
		option = strings.TrimSpace(option)
		if option == "" {
			return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: empty option segment", raw)
		}
		key, val, hasEq := strings.Cut(option, "=")
		if !hasEq {
			return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: option %q must be key=value", raw, option)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		switch key {
		case "secret", "secret_name":
			if spec.SecretName != "" {
				return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: duplicate secret option", raw)
			}
			spec.SecretName = val
		case "auth", "auth_header":
			if spec.AuthHeader != "" {
				return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: duplicate auth option", raw)
			}
			spec.AuthHeader = val
		default:
			return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: unknown option %q (supported: secret, auth)", raw, key)
		}
	}
	normalized, err := validateLLMProviderSpec(spec)
	if err != nil {
		return LLMProviderSpec{}, fmt.Errorf("invalid --llm-provider %q: %w", raw, err)
	}
	return normalized, nil
}

func validateLLMProviderSpec(spec LLMProviderSpec) (LLMProviderSpec, error) {
	name := strings.TrimSpace(spec.Name)
	if _, err := id.ParseActionName(name); err != nil {
		return LLMProviderSpec{}, fmt.Errorf("provider name %q must be a lowercase slug: %w", name, err)
	}
	baseURL := strings.TrimSpace(spec.BaseURL)
	if err := validateProviderBaseURL(baseURL); err != nil {
		return LLMProviderSpec{}, err
	}
	secretName := strings.TrimSpace(spec.SecretName)
	if secretName == "" {
		secretName = defaultProviderSecretName(name)
	}
	if _, err := id.ParseSecretName(secretName); err != nil {
		return LLMProviderSpec{}, fmt.Errorf("secret name %q is invalid: %w", secretName, err)
	}
	authHeader := strings.TrimSpace(spec.AuthHeader)
	switch strings.ToLower(authHeader) {
	case "":
		authHeader = ""
	case "authorization":
		authHeader = "Authorization"
	case "x-api-key":
		authHeader = "x-api-key"
	default:
		return LLMProviderSpec{}, fmt.Errorf("auth header %q must be empty, Authorization, or x-api-key", authHeader)
	}
	return LLMProviderSpec{
		Name:       name,
		BaseURL:    strings.TrimRight(baseURL, "/"),
		SecretName: secretName,
		AuthHeader: authHeader,
	}, nil
}

func validateProviderBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("base_url is empty")
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return fmt.Errorf("base_url contains whitespace")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("base_url is invalid: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("base_url must use http or https")
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.Host == "" || parsed.Hostname() == "" || strings.Contains(parsed.Host, "@") {
		return fmt.Errorf("base_url has no usable host")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fmt.Errorf("base_url must not carry a query or fragment")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		trimmed := strings.TrimSuffix(parsed.Path, "/")
		for _, segment := range strings.Split(strings.TrimPrefix(trimmed, "/"), "/") {
			if segment == "" || segment == "." || segment == ".." {
				return fmt.Errorf("base_url path contains an empty or relative segment")
			}
		}
	}
	return nil
}

func defaultProviderSecretName(providerName string) string {
	var b strings.Builder
	b.Grow(len(providerName) + len("_API_KEY"))
	for i := 0; i < len(providerName); i++ {
		ch := providerName[i]
		switch {
		case ch >= 'a' && ch <= 'z':
			b.WriteByte(ch - ('a' - 'A'))
		case (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9'):
			b.WriteByte(ch)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString("_API_KEY")
	return b.String()
}

func validateLLMProviders(specs []LLMProviderSpec) ([]LLMProviderSpec, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(specs))
	out := make([]LLMProviderSpec, 0, len(specs))
	for _, spec := range specs {
		normalized, err := validateLLMProviderSpec(spec)
		if err != nil {
			return nil, fmt.Errorf("invalid LLM provider %q: %w", spec.Name, err)
		}
		if seen[normalized.Name] {
			return nil, fmt.Errorf("duplicate LLM provider %q", normalized.Name)
		}
		seen[normalized.Name] = true
		out = append(out, normalized)
	}
	return out, nil
}

// reconcileModelsFile writes or removes <state-dir>/models.json for the local
// stack's server container. When disable is true it removes any stale file and
// returns an empty path. When customModelsFile is non-empty it validates and
// copies that file into stateDir (which is bind-mounted into the server
// container). Otherwise it emits the built-in providers (openai, anthropic,
// opencode) merged with any extraProviders (which may add custom providers or
// override built-in ones) plus matching tenant-scoped gateway secret_rules.
func reconcileModelsFile(
	stateDir, tenantID string,
	disable bool,
	customModelsFile string,
	extraProviders []LLMProviderSpec,
) (string, []string, error) {
	path := filepath.Join(stateDir, modelsFileName)
	if disable {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", nil, fmt.Errorf("remove %s: %w", modelsFileName, err)
		}
		return "", nil, nil
	}
	if customPath := strings.TrimSpace(customModelsFile); customPath != "" {
		data, names, err := readAndValidateCustomModelsFile(customPath)
		if err != nil {
			return "", nil, err
		}
		if err := writeFileAtomic(path, data, 0o600); err != nil {
			return "", nil, fmt.Errorf("write %s: %w", modelsFileName, err)
		}
		return path, names, nil
	}

	merged := DefaultLLMProviders()
	indexByName := make(map[string]int, len(merged))
	for i, p := range merged {
		indexByName[p.Name] = i
	}
	for _, extra := range extraProviders {
		if idx, exists := indexByName[extra.Name]; exists {
			merged[idx] = extra
		} else {
			indexByName[extra.Name] = len(merged)
			merged = append(merged, extra)
		}
	}

	providers := make([]localModelProvider, 0, len(merged))
	providerNames := make([]string, 0, len(merged))
	secretSet := make(map[string]struct{}, len(merged))
	for _, spec := range merged {
		providers = append(providers, localModelProvider{
			TenantID:   tenantID,
			Name:       spec.Name,
			BaseURL:    spec.BaseURL,
			SecretName: spec.SecretName,
			AuthHeader: spec.AuthHeader,
		})
		providerNames = append(providerNames, spec.Name)
		secretSet[spec.SecretName] = struct{}{}
	}
	secretNames := make([]string, 0, len(secretSet))
	for name := range secretSet {
		secretNames = append(secretNames, name)
	}
	sort.Strings(secretNames)

	doc := localModelsFile{
		Providers: providers,
		SecretRules: []localGatewaySecretRule{{
			TenantID: tenantID,
			Names:    secretNames,
		}},
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", nil, fmt.Errorf("marshal %s: %w", modelsFileName, err)
	}
	if err := writeFileAtomic(path, append(data, '\n'), 0o600); err != nil {
		return "", nil, fmt.Errorf("write %s: %w", modelsFileName, err)
	}
	return path, providerNames, nil
}

func readAndValidateCustomModelsFile(path string) ([]byte, []string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read --models-file %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, nil, fmt.Errorf("stat --models-file %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("--models-file %q is not a regular file", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxLocalModelsFileBytes+1))
	if err != nil {
		return nil, nil, fmt.Errorf("read --models-file %q: %w", path, err)
	}
	if len(data) > maxLocalModelsFileBytes {
		return nil, nil, fmt.Errorf("--models-file %q exceeds %d bytes", path, maxLocalModelsFileBytes)
	}
	var parsed struct {
		Models    []json.RawMessage `json:"models"`
		Providers []struct {
			Name string `json:"name"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, nil, fmt.Errorf("decode --models-file %q: %w", path, err)
	}
	if len(parsed.Models) == 0 && len(parsed.Providers) == 0 {
		return nil, nil, fmt.Errorf("--models-file %q declares neither models nor providers", path)
	}
	names := make([]string, 0, len(parsed.Providers))
	seen := make(map[string]bool, len(parsed.Providers))
	for _, p := range parsed.Providers {
		if p.Name != "" && !seen[p.Name] {
			seen[p.Name] = true
			names = append(names, p.Name)
		}
	}
	return data, names, nil
}
