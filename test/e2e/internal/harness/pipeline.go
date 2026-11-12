//go:build e2e

package harness

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/optimiware/kodama/internal/shell/pipeline"
)

// Document is the e2e-authored inline pipeline document. It mirrors the
// supported S03 authoring fields this suite uses; it is test data, never a
// second production definition model.
type Document struct {
	Repository string            `yaml:"repository,omitempty"`
	Service    string            `yaml:"service,omitempty"`
	On         *On               `yaml:"on,omitempty"`
	Inputs     []string          `yaml:"inputs,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
	Actions    []Action          `yaml:"actions"`
}

// On is one supported pipeline trigger declaration.
type On struct {
	Push []string `yaml:"push,omitempty"`
}

// Action is one ordered container or request action. Exactly one of Image or
// Request is set.
type Action struct {
	Name     string            `yaml:"name"`
	Image    string            `yaml:"image,omitempty"`
	Commands []string          `yaml:"commands,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
	Request  string            `yaml:"request,omitempty"`
	Subject  map[string]any    `yaml:"subject,omitempty"`
	Outputs  []string          `yaml:"outputs,omitempty"`
	Secrets  []string          `yaml:"secrets,omitempty"`
	Timeout  string            `yaml:"timeout,omitempty"`
}

var declaredKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Build renders the exact pipeline YAML bytes and admits them through the real
// admission parser. The harness is never a second grammar: it accepts exactly
// the documents `pipeline.Decode` accepts and refuses exactly the documents it
// refuses.
func (document Document) Build() ([]byte, error) {
	encoded, err := yaml.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("marshal pipeline yaml: %v", err)
	}
	if _, err := pipeline.Decode(encoded, pipeline.DefaultLimits); err != nil {
		return nil, fmt.Errorf("the real pipeline decoder refused the built document: %w", err)
	}
	return encoded, nil
}

// DocumentDigest returns the SHA-256 identity of exact document bytes. The
// admission snapshot hashes the posted bytes, never a canonicalized form.
func DocumentDigest(document []byte) string {
	sum := sha256.Sum256(document)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// outputSubmissionTemplate is the small Python standard-library HTTPS client
// run inside a tools workload. It is embedded so the generated command and the
// tested program are the same bytes.
//
//go:embed testdata/output-submission.py
var outputSubmissionTemplate string

// OutputSubmissionScript builds one shell command that submits declared outputs
// through the workload's mounted execution credential. caFile is the declared
// CA secret mount (empty means the image's system roots). The script reads the
// current action id from its own inspection, re-reads the token for each call,
// bounds bodies, refuses redirects and retries only an explicit
// version_conflict with a fresh per-action key.
func OutputSubmissionScript(outputs map[string]string, caFile string) (string, error) {
	if len(outputs) == 0 || len(outputs) > 16 {
		return "", fmt.Errorf("output submission needs 1..16 outputs")
	}
	for key, value := range outputs {
		if !declaredKeyPattern.MatchString(key) {
			return "", fmt.Errorf("output %q is not a bounded lower-case key", key)
		}
		if len(value) > 256 || strings.ContainsAny(value, "\x00\n\r\t") {
			return "", fmt.Errorf("output %q value is not a bounded string", key)
		}
	}
	encoded, err := json.MarshalIndent(outputs, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode the output map")
	}
	caLiteral, err := json.Marshal(caFile)
	if err != nil {
		return "", fmt.Errorf("encode the CA path")
	}
	script := strings.ReplaceAll(outputSubmissionTemplate, "__KODAMA_CA_FILE__", string(caLiteral))
	script = strings.ReplaceAll(script, "__KODAMA_OUTPUTS__", string(encoded))
	const (
		delimiter  = "KODAMA_E2E_SUBMIT"
		submitPath = `"${TMPDIR:-/tmp}/kodama-e2e-submit.$$.py"`
	)
	return "trap 'rm -f " + submitPath + "' EXIT\ncat > " + submitPath + " <<'" + delimiter + "'\n" + script + delimiter + "\npython3 " + submitPath, nil
}
