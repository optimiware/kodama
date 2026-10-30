// Package spec holds the constants shared by the OpenCode wrapper
// (images/harness-opencode/wrapper), the image build and the acceptance
// composition in test/integration/harness-opencode. It carries no behaviour
// beyond rendering the static managed OpenCode configuration.
//
// Every value here is either chosen by this task (wrapper identity, loopback
// port, result file name) or measured against the pinned candidate (HarnessVersion);
// none is a production pin of a provider or model.
package spec

import (
	"encoding/json"
	"fmt"
)

const (
	// WrapperID and WrapperVersion identify the wrapper in an agent profile's
	// compatibility tuple.
	WrapperID      = "kodama-opencode"
	WrapperVersion = "0.1.0"
	// HarnessID and HarnessVersion are the pinned OpenCode candidate. The
	// wrapper refuses to run when `opencode --version` disagrees.
	HarnessID      = "opencode"
	HarnessVersion = "2.1.2"

	// WrapperPath and OpenCodePath are the image locations of the binaries.
	WrapperPath  = "/opt/kodama/bin/kodama-opencode-wrapper"
	OpenCodePath = "/opt/kodama/bin/opencode"
	// ManagedConfigPath is the system configuration directory file of OpenCode
	// on Linux; it has the highest configuration precedence.
	ManagedConfigPath = "/etc/opencode/opencode.json"

	// ProxyHost and ProxyPort are the fixed loopback address of the wrapper's
	// credential-injecting proxy. The managed configuration points the
	// provider at it, so a workspace configuration cannot redirect model
	// traffic.
	ProxyHost = "127.0.0.1"
	ProxyPort = 18941
	// ProviderID is the only OpenCode provider the managed policy allows.
	ProviderID = "kodama"
	// ModelPath and MCPPath are the proxy's route prefixes.
	ModelPath = "/model/v1"
	MCPPath   = "/mcp/"

	// ResultFile is the structured completion file the agent must write in the
	// workspace root. The wrapper removes it before anything is committed.
	ResultFile = ".kodama-result.json"
	// ModelKeyEnv carries a placeholder to OpenCode's provider; the proxy
	// replaces every inbound credential, so it is not a secret.
	ModelKeyEnv   = "KODAMA_MODEL_KEY"
	ModelKeyValue = "kodama-loopback-placeholder"

	// Environment the wrapper reads. The step entrypoint exports none of the
	// first three to its command, so a pipeline step supplies them as
	// non-secret `env` (see the README's prerequisites).
	EnvPlatformURL   = "KODAMA_PLATFORM_URL"
	EnvTokenFile     = "KODAMA_TOKEN_FILE"
	EnvCAFile        = "KODAMA_CA_FILE"
	EnvOpenCodeBin   = "KODAMA_OPENCODE_BIN"
	EnvIdleSeconds   = "KODAMA_OPENCODE_IDLE_SECONDS"
	EnvOutputTokens  = "KODAMA_OPENCODE_OUTPUT_TOKENS"
	EnvAttemptID     = "KODAMA_ATTEMPT_ID"
	EnvWorkspace     = "KODAMA_WORKSPACE"
	EnvOutputsDir    = "KODAMA_OUTPUTS_DIR"
	EnvResultsFile   = "KODAMA_RESULTS_FILE"
	DefaultTokenFile = "/kodama/identity/token"
	DefaultCAFile    = "/kodama/bootstrap/ca.crt"

	// ConformanceOutput and TranscriptOutput are the declared artifact output
	// names the wrapper fills.
	ConformanceOutput = "conformance"
	TranscriptOutput  = "transcript"
	// The files carry no extension on purpose: the entrypoint names a file
	// artifact by its basename while the artifact module accepts only the
	// declared output name, so the two agree only when basename == output name
	// (a recorded entrypoint/artifacts mismatch; see the README).
	ConformanceFile = "conformance"
	TranscriptFile  = "transcript"
)

// Reason codes the wrapper reports in a failed manifest. They match the
// contract's ReasonCode pattern; the entrypoint passes them through.
const (
	ReasonIncomplete       = "wrapper_incomplete"
	ReasonPayloadInvalid   = "payload_invalid"
	ReasonConfigInvalid    = "config_invalid"
	ReasonVersionMismatch  = "harness_version_mismatch"
	ReasonStartFailed      = "harness_start_failed"
	ReasonExitNonzero      = "harness_exit_nonzero"
	ReasonHarnessError     = "harness_error"
	ReasonIdle             = "harness_idle"
	ReasonResultMissing    = "result_missing"
	ReasonResultInvalid    = "result_invalid"
	ReasonContextMismatch  = "context_mismatch"
	ReasonOutputUndeclared = "output_undeclared"
	ReasonCommitFailed     = "commit_failed"
	// ReasonSourceInvalid: the source set of a v3 attempt cannot be worked in (the
	// attempt's Git grant could not publish the checkouts, or a checkout is not a
	// repository at a commit). Nothing was run.
	ReasonSourceInvalid     = "source_invalid"
	ReasonAgentReportedFail = "agent_reported_failure"
)

// ManagedConfig renders the static system configuration baked into the image.
// It is the highest-precedence configuration, so neither the workspace nor the
// wrapper-written user configuration can loosen it: one allowed provider whose
// endpoint is the fixed loopback proxy, hard-denied web, skill, subagent and
// question tools, and no plugins or skills.
func ManagedConfig() []byte {
	doc := map[string]any{
		"$schema":   "https://opencode.ai/config.json",
		"update":    "disable",
		"policies":  Policies(),
		"providers": map[string]any{ProviderID: ProviderDefinition()},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		panic(err) // static document
	}
	return append(b, '\n')
}

// Policies are the hard policies of the managed configuration. The wrapper
// repeats them in the user configuration it writes so a development run without
// the baked /etc file is still confined; the managed file remains the
// highest-precedence copy.
func Policies() []map[string]string {
	return []map[string]string{
		{"action": "provider.use", "resource": "*", "effect": "deny"},
		{"action": "provider.use", "resource": ProviderID, "effect": "allow"},
		{"action": "tool.use", "resource": "webfetch:*", "effect": "deny"},
		{"action": "tool.use", "resource": "websearch:*", "effect": "deny"},
		{"action": "tool.use", "resource": "skill:*", "effect": "deny"},
		{"action": "tool.use", "resource": "subagent:*", "effect": "deny"},
		{"action": "tool.use", "resource": "question:*", "effect": "deny"},
		{"action": "integration.use", "resource": "plugin:*", "effect": "deny"},
		{"action": "integration.use", "resource": "skill:*", "effect": "deny"},
	}
}

// ProviderDefinition is the only provider OpenCode may use: an OpenAI-compatible
// endpoint at the wrapper's loopback proxy.
func ProviderDefinition() map[string]any {
	return map[string]any{
		"name":     "Kodama gateway (loopback)",
		"env":      []string{ModelKeyEnv},
		"package":  "@opencode/ai/providers/openai-compatible",
		"settings": map[string]any{"baseURL": ModelBaseURL()},
	}
}

// ModelBaseURL is the provider endpoint OpenCode is configured with.
func ModelBaseURL() string { return fmt.Sprintf("http://%s:%d%s", ProxyHost, ProxyPort, ModelPath) }

// DeclaredOutputPath is the path a pipeline declares for a file output: the
// entrypoint maps /kodama/outputs/<file> onto its outputs directory.
func DeclaredOutputPath(file string) string { return "/kodama/outputs/" + file }
