package app

// Trusted-file configuration of the optional Shell gateways.
//
// `kodama server` reads the model gateway and the external MCP tool proxy from
// operator-owned files named by KODAMA_MODELS_FILE and KODAMA_TOOL_PROXY_FILE.
// The files are trusted configuration: they carry project scopes, provider or
// upstream endpoints, secret names and bounds, never a secret value, a
// permission or a transport. An unnamed surface stays disabled with the
// not_found refusal; a named-but-unreadable, malformed, unknown-member
// or semantically incomplete file is a startup refusal that names the setting.
// No entry ever falls back to an implicit configuration.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/gateway"
	"github.com/optimiware/kodama/internal/shell/secret"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

const (
	// ModelsFileSetting names the trusted model-gateway configuration file.
	ModelsFileSetting = "KODAMA_MODELS_FILE"
	// ToolProxyFileSetting names the trusted external MCP tool-proxy
	// configuration file.
	ToolProxyFileSetting = "KODAMA_TOOL_PROXY_FILE"

	// ToolArgumentValidatorJSONObject is the configured argument validator that
	// admits exactly one JSON object. It is the only validator a file may name;
	// an unknown or empty name is refused rather than defaulted to a no-op.
	ToolArgumentValidatorJSONObject = "json_object"

	// maxTrustedConfigFileBytes bounds one gateway configuration file. The two
	// files describe an installation's allow-lists, so a file above this bound
	// is a configuration failure, never a partially read policy.
	maxTrustedConfigFileBytes = 1 << 20
	// maxTrustedConfigDepth bounds JSON nesting. The declared file shapes nest
	// only a handful of levels; an explicit bound keeps a hostile document from
	// spending unbounded parser stack before the strict member pass finishes.
	maxTrustedConfigDepth = 32
)

// errTrustedConfigTrailing marks content after the one trusted document. It is
// a sentinel so the caller can keep the dedicated trailing-content refusal.
var errTrustedConfigTrailing = errors.New("trailing content after the document")

// modelsFile is the strict JSON shape of KODAMA_MODELS_FILE.
type modelsFile struct {
	Models      []modelFile         `json:"models"`
	Providers   []modelProviderFile `json:"providers"`
	SecretRules []secretRuleFile    `json:"secret_rules"`
}

// modelFile is one configured model entry.
type modelFile struct {
	TenantID            string `json:"tenant_id"`
	ProjectID           string `json:"project_id"`
	Model               string `json:"model"`
	UpstreamModel       string `json:"upstream_model"`
	Endpoint            string `json:"endpoint"`
	SecretName          string `json:"secret_name"`
	MaxRequestBytes     int    `json:"max_request_bytes"`
	MaxResponseBytes    int    `json:"max_response_bytes"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
	Timeout             string `json:"timeout"`
	RecordContent       bool   `json:"record_content"`
	ContentRetention    string `json:"content_retention"`
}

// modelProviderFile is one configured streaming LLM proxy provider entry.
type modelProviderFile struct {
	TenantID         string `json:"tenant_id"`
	ProjectID        string `json:"project_id"`
	Name             string `json:"name"`
	BaseURL          string `json:"base_url"`
	Endpoint         string `json:"endpoint"`
	SecretName       string `json:"secret_name"`
	AuthHeader       string `json:"auth_header"`
	AnthropicVersion string `json:"anthropic_version"`
	MaxRequestBytes  int    `json:"max_request_bytes"`
	Timeout          string `json:"timeout"`
}

// toolProxyFile is the strict JSON shape of KODAMA_TOOL_PROXY_FILE.
type toolProxyFile struct {
	Projects    []toolProjectFile `json:"projects"`
	SecretRules []secretRuleFile  `json:"secret_rules"`
	Timeout     string            `json:"timeout"`
}

// toolProjectFile is one project-scoped proxy surface.
type toolProjectFile struct {
	TenantID  string           `json:"tenant_id"`
	ProjectID string           `json:"project_id"`
	Servers   []toolServerFile `json:"servers"`
}

// toolServerFile is one trusted upstream MCP server.
type toolServerFile struct {
	Name        string               `json:"name"`
	Endpoint    string               `json:"endpoint"`
	Secrets     []string             `json:"secrets"`
	Credentials []toolCredentialFile `json:"credentials"`
	Tools       []toolFile           `json:"tools"`
}

// toolCredentialFile is one explicit secret-to-header mapping.
type toolCredentialFile struct {
	Secret string `json:"secret"`
	Header string `json:"header"`
	Scheme string `json:"scheme"`
}

// toolFile is one public alias of one upstream tool.
type toolFile struct {
	Alias       string `json:"alias"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Validator   string `json:"validator"`
}

// secretRuleFile is one configured secret-name allow-list rule.
type secretRuleFile struct {
	TenantID  string   `json:"tenant_id"`
	ProjectID string   `json:"project_id"`
	Names     []string `json:"names"`
}

// LoadModelsConfigFile reads and validates the trusted model-gateway
// configuration at path for one composition profile. The returned value is
// complete: an enabled configuration with no models, a duplicate model, a
// malformed bound or a plain-http provider in production is refused. A nil
// value and an error are always returned together.
func LoadModelsConfigFile(path string, profile Profile) (*ModelsConfig, error) {
	var file modelsFile
	if err := decodeTrustedConfigFile(path, ModelsFileSetting, &file); err != nil {
		return nil, err
	}
	config := &ModelsConfig{}
	for index, entry := range file.Models {
		timeout, err := trustedDuration(entry.Timeout)
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s model %d timeout is invalid: %v", ModelsFileSetting, index+1, err), err)
		}
		retention, err := trustedDuration(entry.ContentRetention)
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s model %d content retention is invalid: %v", ModelsFileSetting, index+1, err), err)
		}
		if err := trustedModelsFileEndpoint(entry.Endpoint, index); err != nil {
			return nil, err
		}
		config.Models = append(config.Models, gateway.ModelConfig{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(entry.TenantID),
				ProjectID: id.ProjectID(entry.ProjectID),
			},
			Model:               entry.Model,
			UpstreamModel:       entry.UpstreamModel,
			Endpoint:            entry.Endpoint,
			SecretName:          id.SecretName(entry.SecretName),
			MaxRequestBytes:     entry.MaxRequestBytes,
			MaxResponseBytes:    entry.MaxResponseBytes,
			MaxCompletionTokens: entry.MaxCompletionTokens,
			Timeout:             timeout,
			RecordContent:       entry.RecordContent,
			ContentRetention:    retention,
		})
	}
	for index, entry := range file.Providers {
		timeout, err := trustedDuration(entry.Timeout)
		if err != nil {
			return nil, fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s provider %d timeout is invalid: %v", ModelsFileSetting, index+1, err), err)
		}
		config.Providers = append(config.Providers, ModelProviderConfig{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(entry.TenantID),
				ProjectID: id.ProjectID(entry.ProjectID),
			},
			Name:             entry.Name,
			BaseURL:          entry.BaseURL,
			Endpoint:         entry.Endpoint,
			SecretName:       id.SecretName(entry.SecretName),
			AuthHeader:       entry.AuthHeader,
			AnthropicVersion: entry.AnthropicVersion,
			MaxRequestBytes:  entry.MaxRequestBytes,
			Timeout:          timeout,
		})
	}
	config.SecretRules = trustedSecretRules(file.SecretRules)
	if err := config.validate(profile); err != nil {
		return nil, fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s configuration is invalid: %v", ModelsFileSetting, err), err)
	}
	return config, nil
}

// LoadToolProxyConfigFile reads and validates the trusted external MCP
// tool-proxy configuration at path. The returned value is complete: an enabled
// configuration with no project, an undeclared or dropped credential mapping,
// an unknown argument validator or an ambiguous alias is refused. A nil value
// and an error are always returned together.
func LoadToolProxyConfigFile(path string) (*ToolProxyConfig, error) {
	var file toolProxyFile
	if err := decodeTrustedConfigFile(path, ToolProxyFileSetting, &file); err != nil {
		return nil, err
	}
	config := &ToolProxyConfig{}
	timeout, err := trustedDuration(file.Timeout)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s timeout is invalid: %v", ToolProxyFileSetting, err), err)
	}
	config.Timeout = timeout
	config.SecretRules = trustedSecretRules(file.SecretRules)
	config.Projects = make([]ProjectToolProxy, 0, len(file.Projects))
	for projectIndex, project := range file.Projects {
		entry := ProjectToolProxy{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(project.TenantID),
				ProjectID: id.ProjectID(project.ProjectID),
			},
			Servers: make([]ToolServer, 0, len(project.Servers)),
		}
		for serverIndex, server := range project.Servers {
			configured, err := trustedToolServer(projectIndex, serverIndex, server)
			if err != nil {
				return nil, err
			}
			entry.Servers = append(entry.Servers, configured)
		}
		config.Projects = append(config.Projects, entry)
	}
	if err := config.validate(); err != nil {
		return nil, fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s configuration is invalid: %v", ToolProxyFileSetting, err), err)
	}
	return config, nil
}

// trustedToolServer maps one configured upstream and its closed validator
// vocabulary.
func trustedToolServer(projectIndex, serverIndex int, server toolServerFile) (ToolServer, error) {
	position := fmt.Sprintf("%s project %d server %d", ToolProxyFileSetting, projectIndex+1, serverIndex+1)
	configured := ToolServer{
		Name:        server.Name,
		Endpoint:    server.Endpoint,
		Secrets:     make([]id.SecretName, 0, len(server.Secrets)),
		Credentials: make([]ToolCredential, 0, len(server.Credentials)),
		Tools:       make([]ToolDefinition, 0, len(server.Tools)),
	}
	for _, name := range server.Secrets {
		configured.Secrets = append(configured.Secrets, id.SecretName(name))
	}
	for _, credential := range server.Credentials {
		configured.Credentials = append(configured.Credentials, ToolCredential{
			Secret: id.SecretName(credential.Secret),
			Header: credential.Header,
			Scheme: credential.Scheme,
		})
	}
	for toolIndex, tool := range server.Tools {
		validate, err := toolArgumentValidator(tool.Validator)
		if err != nil {
			return ToolServer{}, fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s tool %d validator is invalid: %v", position, toolIndex+1, err), err)
		}
		configured.Tools = append(configured.Tools, ToolDefinition{
			Alias:       tool.Alias,
			Name:        tool.Name,
			Description: tool.Description,
			Validate:    validate,
		})
	}
	return configured, nil
}

// toolArgumentValidator resolves the closed validator vocabulary. An empty
// name returns nil so the construction check refuses the tool instead of
// silently admitting every argument; an unknown name is a refusal here.
func toolArgumentValidator(name string) (func(json.RawMessage) error, error) {
	switch name {
	case "":
		return nil, nil
	case ToolArgumentValidatorJSONObject:
		return validateJSONObjectArguments, nil
	default:
		return nil, fmt.Errorf("%q is not one of the configured validators", name)
	}
}

// validateJSONObjectArguments admits exactly one JSON object.
func validateJSONObjectArguments(arguments json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &object); err != nil || object == nil {
		return fmt.Errorf("arguments are not a JSON object")
	}
	return nil
}

// trustedSecretRules maps the configured rules; an absent list stays nil.
func trustedSecretRules(rules []secretRuleFile) []secret.NameRule {
	if len(rules) == 0 {
		return nil
	}
	configured := make([]secret.NameRule, 0, len(rules))
	for _, rule := range rules {
		names := make([]id.SecretName, 0, len(rule.Names))
		for _, name := range rule.Names {
			names = append(names, id.SecretName(name))
		}
		configured = append(configured, secret.NameRule{
			Scope: tenancy.Scope{
				TenantID:  id.TenantID(rule.TenantID),
				ProjectID: id.ProjectID(rule.ProjectID),
			},
			Names: names,
		})
	}
	return configured
}

// trustedModelsFileEndpoint refuses a configured model endpoint that carries a
// query or fragment. The models file never holds a secret value, so a query or
// fragment could only be smuggled credential material or a reader-side
// selector; the gateway's own endpoint validation stays untouched for
// non-file configurations.
func trustedModelsFileEndpoint(endpoint string, index int) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s model %d endpoint is invalid: %v", ModelsFileSetting, index+1, err), err)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return fault.New(fault.InvalidInput,
			fmt.Sprintf("%s model %d endpoint must not carry a query or fragment; the provider secret is resolved per call", ModelsFileSetting, index+1))
	}
	return nil
}

// trustedDuration parses one optional configured duration. An absent value is
// zero; a malformed one is a refusal rather than a silent default.
func trustedDuration(value string) (time.Duration, error) {
	if strings.TrimSpace(value) == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("must be a Go duration such as 250ms or 5s")
	}
	return parsed, nil
}

// decodeTrustedConfigFile strictly decodes exactly one bounded JSON document.
// Unknown members, a repeated member in one object, a member whose name is not
// an exact declared name, trailing content, an empty file and an unreadable or
// non-regular path are all refusals; the diagnostic names the setting and the
// path, never a value.
func decodeTrustedConfigFile(path, setting string, target any) error {
	data, err := readTrustedConfigFile(path, setting)
	if err != nil {
		return err
	}
	if err := validateTrustedConfigDocument(data, target); err != nil {
		switch {
		case err == io.EOF:
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s file %q is empty", setting, path))
		case errors.Is(err, errTrustedConfigTrailing):
			return fault.New(fault.InvalidInput,
				fmt.Sprintf("%s file %q has trailing content after its document", setting, path))
		default:
			return fault.Wrap(fault.InvalidInput,
				fmt.Sprintf("%s file %q is malformed: %v", setting, path, err), err)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if err == io.EOF {
			return fault.New(fault.InvalidInput, fmt.Sprintf("%s file %q is empty", setting, path))
		}
		return fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s file %q is malformed: %v", setting, path, err), err)
	}
	var trailing json.RawMessage
	switch err := decoder.Decode(&trailing); {
	case err == io.EOF:
		return nil
	case err == nil:
		return fault.New(fault.InvalidInput,
			fmt.Sprintf("%s file %q has trailing content after its document", setting, path))
	default:
		return fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s file %q is malformed: %v", setting, path, err), err)
	}
}

// readTrustedConfigFile reads one bounded regular file. The file is opened and
// checked before any byte is read, and the read stops one byte past the bound so
// an oversized file is refused instead of materialized.
func readTrustedConfigFile(path, setting string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s file %q is unreadable: %v", setting, path, err), err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s file %q is unreadable: %v", setting, path, err), err)
	}
	if !info.Mode().IsRegular() {
		return nil, fault.New(fault.InvalidInput,
			fmt.Sprintf("%s file %q is not a regular file", setting, path))
	}
	data, err := io.ReadAll(io.LimitReader(file, maxTrustedConfigFileBytes+1))
	if err != nil {
		return nil, fault.Wrap(fault.InvalidInput,
			fmt.Sprintf("%s file %q is unreadable: %v", setting, path, err), err)
	}
	if len(data) > maxTrustedConfigFileBytes {
		return nil, fault.New(fault.InvalidInput,
			fmt.Sprintf("%s file %q exceeds the %d-byte bound", setting, path, maxTrustedConfigFileBytes))
	}
	return data, nil
}

// validateTrustedConfigDocument walks the JSON token stream along the declared
// Go shape before the decoder binds any member. It refuses a repeated member in
// one object, any member name that is not an exact declared name (so a
// case-variant such as Endpoint can never bind to endpoint), and nesting past
// maxTrustedConfigDepth.
func validateTrustedConfigDocument(data []byte, target any) error {
	shape := reflect.TypeOf(target)
	for shape != nil && shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	if shape == nil || shape.Kind() != reflect.Struct {
		return fmt.Errorf("trusted configuration target is not a struct")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if err := walkTrustedConfigToken(decoder, token, shape, 1); err != nil {
		return err
	}
	if _, err := decoder.Token(); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errTrustedConfigTrailing
}

// walkTrustedConfigToken validates one token and everything it opens against
// the declared shape. Scalars carry no member names, so only structure is
// checked here; the strict decoder still binds and types them.
func walkTrustedConfigToken(decoder *json.Decoder, token json.Token, shape reflect.Type, depth int) error {
	if depth > maxTrustedConfigDepth {
		return fmt.Errorf("document nesting exceeds the %d-level depth bound", maxTrustedConfigDepth)
	}
	for shape != nil && shape.Kind() == reflect.Pointer {
		shape = shape.Elem()
	}
	if shape == nil {
		return fmt.Errorf("the declared shape is missing")
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		if shape.Kind() != reflect.Struct {
			return fmt.Errorf("an object appears where the declared shape is not an object")
		}
		declared := make(map[string]reflect.Type, shape.NumField())
		for field := range shape.Fields() {
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue
			}
			declared[name] = field.Type
		}
		seen := make(map[string]bool, len(declared))
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("an object member name is not a string")
			}
			if seen[key] {
				return fmt.Errorf("duplicate object member %q", key)
			}
			seen[key] = true
			member, ok := declared[key]
			if !ok {
				return fmt.Errorf("object member %q does not exactly match a declared member", key)
			}
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkTrustedConfigToken(decoder, valueToken, member, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("an object is not closed")
		}
		return nil
	case '[':
		if shape.Kind() != reflect.Slice && shape.Kind() != reflect.Array {
			return fmt.Errorf("an array appears where the declared shape is not an array")
		}
		element := shape.Elem()
		for decoder.More() {
			valueToken, err := decoder.Token()
			if err != nil {
				return err
			}
			if err := walkTrustedConfigToken(decoder, valueToken, element, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("an array is not closed")
		}
		return nil
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
}
