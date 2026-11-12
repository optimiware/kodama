//go:build qualification

package production

import (
	"crypto/x509"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/optimiware/kodama/api"
)

// Input is one environment variable the qualification harness reads. Inputs
// come only from the environment; a missing input is a named skip, never a pass.
type Input string

const (
	InputAPIURL           Input = "Q02_API_URL"
	InputCAPEM            Input = "Q02_CA_PEM"
	InputTLSServerName    Input = "Q02_TLS_SERVER_NAME"
	InputAdminToken       Input = "Q02_ADMIN_TOKEN"
	InputDeveloperToken   Input = "Q02_DEVELOPER_TOKEN"
	InputTenantID         Input = "Q02_TENANT_ID"
	InputProjectID        Input = "Q02_PROJECT_ID"
	InputTenantNamespace  Input = "Q02_TENANT_NAMESPACE"
	InputSecondTenantID   Input = "Q02_SECOND_TENANT_ID"
	InputSecondProjectID  Input = "Q02_SECOND_PROJECT_ID"
	InputSecondNamespace  Input = "Q02_SECOND_TENANT_NAMESPACE"
	InputSecondAdminToken Input = "Q02_SECOND_ADMIN_TOKEN"
	InputGitRepositoryA   Input = "Q02_GIT_REPOSITORY_A"
	InputGitRepositoryB   Input = "Q02_GIT_REPOSITORY_B"
	InputGitServiceA      Input = "Q02_GIT_SERVICE_A"
	InputGitServiceB      Input = "Q02_GIT_SERVICE_B"
	InputKubeContext      Input = "Q02_KUBE_CONTEXT"
	InputImage            Input = "Q02_IMAGE"
	InputNetworkImage     Input = "Q02_NETWORK_IMAGE"
	InputEvidenceDir      Input = "Q02_EVIDENCE_DIR"
	InputHarnessRevision  Input = "Q02_HARNESS_REVISION"
	InputDeployedRevision Input = "Q02_DEPLOYED_REVISION"
)

// The token-file inputs are an alternative source for the token inputs: a
// private file holding the credential. A long run refreshes the file in place,
// and each probe reads it when it starts, so a later probe never starts with a
// token an earlier one already saw expire. Setting a token and its file is
// ambiguous and refused.
const (
	InputAdminTokenFile       Input = "Q02_ADMIN_TOKEN_FILE"
	InputSecondAdminTokenFile Input = "Q02_SECOND_ADMIN_TOKEN_FILE"
)

// Inputs lists every environment input in declaration order.
var Inputs = []Input{
	InputAPIURL,
	InputCAPEM,
	InputTLSServerName,
	InputAdminToken,
	InputDeveloperToken,
	InputTenantID,
	InputProjectID,
	InputTenantNamespace,
	InputSecondTenantID,
	InputSecondProjectID,
	InputSecondNamespace,
	InputSecondAdminToken,
	InputGitRepositoryA,
	InputGitRepositoryB,
	InputGitServiceA,
	InputGitServiceB,
	InputKubeContext,
	InputImage,
	InputNetworkImage,
	InputEvidenceDir,
	InputHarnessRevision,
	InputDeployedRevision,
}

// Secret is bearer credential material. It renders as redacted under every
// ordinary formatting verb; only Reveal returns the value.
type Secret string

// Reveal returns the credential value for the one call that must send it.
func (secret Secret) Reveal() string { return string(secret) }

// String redacts the credential.
func (secret Secret) String() string { return "<redacted>" }

// GoString redacts the credential under %#v.
func (secret Secret) GoString() string { return `production.Secret("<redacted>")` }

// IsSet reports whether a credential value is configured.
func (secret Secret) IsSet() bool { return secret != "" }

// Config is the validated qualification environment. Tokens are Secret values
// and never appear in String or GoString.
type Config struct {
	APIURL           string
	CAPEM            string
	TLSServerName    string
	AdminToken       Secret
	DeveloperToken   Secret
	TenantID         string
	ProjectID        string
	TenantNamespace  string
	SecondTenantID   string
	SecondProjectID  string
	SecondNamespace  string
	SecondAdminToken Secret
	GitRepositoryA   string
	GitRepositoryB   string
	GitServiceA      string
	GitServiceB      string
	KubeContext      string
	Image            string
	NetworkImage     string
	EvidenceDir      string
	HarnessRevision  string
	DeployedRevision string
}

// LoadConfig reads and validates the qualification environment. Absent values
// are left empty so a test can skip with their names; a present but malformed
// value is an error naming the input.
func LoadConfig(read func(string) string) (Config, error) {
	if read == nil {
		read = os.Getenv
	}
	var config Config

	if raw := strings.TrimSpace(read(string(InputAPIURL))); raw != "" {
		normalized, err := validateAPIURL(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputAPIURL, err)
		}
		config.APIURL = normalized
	}
	if raw := strings.TrimSpace(read(string(InputCAPEM))); raw != "" {
		normalized, err := validateCAPEM(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputCAPEM, err)
		}
		config.CAPEM = normalized
	}
	if raw := strings.TrimSpace(read(string(InputTLSServerName))); raw != "" {
		if err := validateDNSName(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputTLSServerName, err)
		}
		config.TLSServerName = raw
	}
	adminToken, err := loadToken(read, InputAdminToken, InputAdminTokenFile)
	if err != nil {
		return Config{}, err
	}
	config.AdminToken = adminToken
	if raw := read(string(InputDeveloperToken)); raw != "" {
		if err := validateToken(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputDeveloperToken, err)
		}
		config.DeveloperToken = Secret(raw)
	}
	if raw := strings.TrimSpace(read(string(InputTenantID))); raw != "" {
		if !identityPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a bounded identity", InputTenantID, raw)
		}
		config.TenantID = raw
	}
	if raw := strings.TrimSpace(read(string(InputProjectID))); raw != "" {
		if !identityPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a bounded identity", InputProjectID, raw)
		}
		config.ProjectID = raw
	}
	if raw := strings.TrimSpace(read(string(InputTenantNamespace))); raw != "" {
		if systemNamespaces[raw] {
			return Config{}, fmt.Errorf("%s: is a system namespace", InputTenantNamespace)
		}
		if !dnsLabelPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a kubernetes namespace", InputTenantNamespace, raw)
		}
		config.TenantNamespace = raw
	}
	if raw := strings.TrimSpace(read(string(InputSecondTenantID))); raw != "" {
		if !identityPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a bounded identity", InputSecondTenantID, raw)
		}
		config.SecondTenantID = raw
	}
	if raw := strings.TrimSpace(read(string(InputSecondProjectID))); raw != "" {
		if !identityPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a bounded identity", InputSecondProjectID, raw)
		}
		config.SecondProjectID = raw
	}
	if raw := strings.TrimSpace(read(string(InputSecondNamespace))); raw != "" {
		if systemNamespaces[raw] {
			return Config{}, fmt.Errorf("%s: is a system namespace", InputSecondNamespace)
		}
		if !dnsLabelPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a kubernetes namespace", InputSecondNamespace, raw)
		}
		config.SecondNamespace = raw
	}
	secondAdminToken, err := loadToken(read, InputSecondAdminToken, InputSecondAdminTokenFile)
	if err != nil {
		return Config{}, err
	}
	config.SecondAdminToken = secondAdminToken
	if raw := strings.TrimSpace(read(string(InputGitRepositoryA))); raw != "" {
		if err := validateGitRepository(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputGitRepositoryA, err)
		}
		config.GitRepositoryA = raw
	}
	if raw := strings.TrimSpace(read(string(InputGitRepositoryB))); raw != "" {
		if err := validateGitRepository(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputGitRepositoryB, err)
		}
		config.GitRepositoryB = raw
	}
	if raw := strings.TrimSpace(read(string(InputGitServiceA))); raw != "" {
		if !dnsLabelPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a kubernetes service name", InputGitServiceA, raw)
		}
		config.GitServiceA = raw
	}
	if raw := strings.TrimSpace(read(string(InputGitServiceB))); raw != "" {
		if !dnsLabelPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a kubernetes service name", InputGitServiceB, raw)
		}
		config.GitServiceB = raw
	}
	if raw := strings.TrimSpace(read(string(InputKubeContext))); raw != "" {
		if err := validateKubeContext(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputKubeContext, err)
		}
		config.KubeContext = raw
	}
	if raw := strings.TrimSpace(read(string(InputImage))); raw != "" {
		if !pinnedImagePattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a digest-pinned image", InputImage, raw)
		}
		config.Image = raw
	}
	if raw := strings.TrimSpace(read(string(InputNetworkImage))); raw != "" {
		if !pinnedImagePattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a digest-pinned image", InputNetworkImage, raw)
		}
		config.NetworkImage = raw
	}
	// The network listener/client pair needs nc and curl; a stage may pin a
	// small tool image for it while the general workload image stays unchanged.
	if config.NetworkImage == "" {
		config.NetworkImage = config.Image
	}
	if raw := strings.TrimSpace(read(string(InputEvidenceDir))); raw != "" {
		normalized, err := validateEvidenceDir(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputEvidenceDir, err)
		}
		config.EvidenceDir = normalized
	}
	if raw := strings.TrimSpace(read(string(InputHarnessRevision))); raw != "" {
		if !revisionPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a git revision", InputHarnessRevision, raw)
		}
		config.HarnessRevision = raw
	}
	if raw := strings.TrimSpace(read(string(InputDeployedRevision))); raw != "" {
		if !revisionPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a git revision", InputDeployedRevision, raw)
		}
		config.DeployedRevision = raw
	}
	return config, nil
}

// MissingInputs names each required input that is not configured.
type MissingInputs []Input

// Empty reports whether every required input is configured.
func (missing MissingInputs) Empty() bool { return len(missing) == 0 }

// String renders the named skip reason.
func (missing MissingInputs) String() string {
	if len(missing) == 0 {
		return ""
	}
	names := make([]string, 0, len(missing))
	for _, input := range missing {
		names = append(names, string(input))
	}
	return "missing qualification input(s): " + strings.Join(names, ", ")
}

// Error renders the missing inputs as an error.
func (missing MissingInputs) Error() string { return missing.String() }

// Missing returns the required inputs that are not configured, in the caller's
// order.
func (config Config) Missing(required ...Input) MissingInputs {
	var missing MissingInputs
	for _, input := range required {
		if config.input(input) == "" {
			missing = append(missing, input)
		}
	}
	return missing
}

// input returns the configured value of one input.
func (config Config) input(input Input) string {
	switch input {
	case InputAPIURL:
		return config.APIURL
	case InputCAPEM:
		return config.CAPEM
	case InputTLSServerName:
		return config.TLSServerName
	case InputAdminToken:
		return string(config.AdminToken)
	case InputDeveloperToken:
		return string(config.DeveloperToken)
	case InputTenantID:
		return config.TenantID
	case InputProjectID:
		return config.ProjectID
	case InputTenantNamespace:
		return config.TenantNamespace
	case InputSecondTenantID:
		return config.SecondTenantID
	case InputSecondProjectID:
		return config.SecondProjectID
	case InputSecondNamespace:
		return config.SecondNamespace
	case InputSecondAdminToken:
		return string(config.SecondAdminToken)
	case InputGitRepositoryA:
		return config.GitRepositoryA
	case InputGitRepositoryB:
		return config.GitRepositoryB
	case InputGitServiceA:
		return config.GitServiceA
	case InputGitServiceB:
		return config.GitServiceB
	case InputKubeContext:
		return config.KubeContext
	case InputImage:
		return config.Image
	case InputNetworkImage:
		return config.NetworkImage
	case InputEvidenceDir:
		return config.EvidenceDir
	case InputHarnessRevision:
		return config.HarnessRevision
	case InputDeployedRevision:
		return config.DeployedRevision
	default:
		return ""
	}
}

// ImageDigest returns the digest of the configured pinned image, or empty when
// the image is unconfigured or not digest-pinned.
func (config Config) ImageDigest() string {
	index := strings.LastIndex(config.Image, "@")
	if index < 0 || index == len(config.Image)-1 {
		return ""
	}
	return config.Image[index+1:]
}

// tokens returns every configured qualification credential for evidence
// redaction. It never renders a value.
func (config Config) tokens() []Secret {
	return []Secret{config.AdminToken, config.DeveloperToken, config.SecondAdminToken}
}

// String renders a configuration summary with every token redacted.
func (config Config) String() string {
	return fmt.Sprintf(
		"Config{APIURL:%s, CAPEM:%s, TLSServerName:%s, AdminToken:<redacted>, DeveloperToken:<redacted>, SecondAdminToken:<redacted>, TenantID:%s, ProjectID:%s, TenantNamespace:%s, SecondTenantID:%s, SecondProjectID:%s, SecondNamespace:%s, GitRepositoryA:%s, GitRepositoryB:%s, GitServiceA:%s, GitServiceB:%s, KubeContext:%s, Image:%s, NetworkImage:%s, EvidenceDir:%s, HarnessRevision:%s, DeployedRevision:%s}",
		config.APIURL, config.CAPEM, config.TLSServerName, config.TenantID, config.ProjectID,
		config.TenantNamespace, config.SecondTenantID, config.SecondProjectID, config.SecondNamespace,
		config.GitRepositoryA, config.GitRepositoryB, config.GitServiceA, config.GitServiceB,
		config.KubeContext, config.Image, config.NetworkImage, config.EvidenceDir,
		config.HarnessRevision, config.DeployedRevision,
	)
}

// GoString renders a %#v configuration summary with every token redacted.
func (config Config) GoString() string { return config.String() }

var (
	identityPattern             = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	dnsLabelPattern             = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	pinnedImagePattern          = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*@sha256:[a-f0-9]{64}$`)
	revisionPattern             = regexp.MustCompile(`^[a-f0-9]{7,64}$`)
	hostnameLabelPattern        = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?$`)
	gitRepositorySegmentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// validateAPIURL requires an absolute https base URL with no userinfo, query,
// fragment or trailing slash ambiguity.
func validateAPIURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("is not a valid URL: %v", err)
	}
	if parsed.Scheme != "https" {
		return "", fmt.Errorf("must use https")
	}
	if parsed.Opaque != "" {
		return "", fmt.Errorf("must not be opaque")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("must be absolute")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("must not carry user information")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("must not carry a query or fragment")
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return "", fmt.Errorf("must not contain control characters")
	}
	return strings.TrimSuffix(raw, "/"), nil
}

// validateCAPEM requires a readable regular file holding at least one X.509
// certificate.
func validateCAPEM(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the CA file: %v", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the CA path is not a regular file")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the CA file: %v", err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(data) {
		return "", fmt.Errorf("the CA file contains no certificate")
	}
	return filepath.Clean(path), nil
}

// validateDNSName requires a plain DNS name, not a URL or a host:port pair.
func validateDNSName(name string) error {
	if name == "" || len(name) > 253 {
		return fmt.Errorf("is not a bounded DNS name")
	}
	if strings.ContainsAny(name, "/:@ \t\r\n") {
		return fmt.Errorf("must be a plain DNS name")
	}
	labels := strings.SplitSeq(name, ".")
	for label := range labels {
		if !hostnameLabelPattern.MatchString(label) {
			return fmt.Errorf("is not a DNS name")
		}
	}
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".") || strings.Contains(name, "..") {
		return fmt.Errorf("is not a DNS name")
	}
	return nil
}

// loadToken reads one credential from its value input or from the private file
// its file input names. A file may end with one line break; it is otherwise
// held to the same rules as a value.
func loadToken(read func(string) string, value, file Input) (Secret, error) {
	raw := read(string(value))
	path := strings.TrimSpace(read(string(file)))
	switch {
	case raw != "" && path != "":
		return "", fmt.Errorf("%s and %s are ambiguous; configure exactly one", value, file)
	case raw != "":
		if err := validateToken(raw); err != nil {
			return "", fmt.Errorf("%s: %w", value, err)
		}
		return Secret(raw), nil
	case path == "":
		return "", nil
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s: must be an absolute path", file)
	}
	handle, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the token file", file)
	}
	defer handle.Close()
	data, err := io.ReadAll(io.LimitReader(handle, api.MaxCredentialBytes+2))
	if err != nil {
		return "", fmt.Errorf("%s: cannot read the token file", file)
	}
	token := strings.TrimRight(string(data), "\r\n")
	if err := validateToken(token); err != nil {
		return "", fmt.Errorf("%s: %w", file, err)
	}
	return Secret(token), nil
}

// validateToken rejects an empty, oversized or whitespace-bearing bearer
// credential. Tokens are otherwise opaque.
func validateToken(token string) error {
	if token == "" {
		return fmt.Errorf("is empty")
	}
	if len(token) > api.MaxCredentialBytes {
		return fmt.Errorf("is %d bytes; at most %d are allowed", len(token), api.MaxCredentialBytes)
	}
	for index := 0; index < len(token); index++ {
		if token[index] <= 0x20 || token[index] == 0x7f {
			return fmt.Errorf("contains whitespace or a control character")
		}
	}
	return nil
}

// validateGitRepository requires a bounded repository path with plain segments
// and no traversal.
func validateGitRepository(repository string) error {
	if repository == "" || len(repository) > 128 || strings.HasPrefix(repository, "/") || strings.HasSuffix(repository, "/") {
		return fmt.Errorf("is not a bounded repository path")
	}
	if strings.Contains(repository, "//") || strings.ContainsAny(repository, "\\ \t\r\n") {
		return fmt.Errorf("is not a bounded repository path")
	}
	for segment := range strings.SplitSeq(repository, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return fmt.Errorf("contains an unsafe path segment")
		}
		if !gitRepositorySegmentPattern.MatchString(segment) {
			return fmt.Errorf("contains an unsupported character")
		}
	}
	return nil
}

// validateKubeContext requires a plausible, non-flag kubeconfig context name.
func validateKubeContext(context string) error {
	if context == "" || len(context) > 253 {
		return fmt.Errorf("is not a bounded context name")
	}
	if strings.HasPrefix(context, "-") {
		return fmt.Errorf("must not look like a kubectl flag")
	}
	for index := 0; index < len(context); index++ {
		character := context[index]
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-', character == '_', character == '.', character == '@', character == ':', character == '/':
		default:
			return fmt.Errorf("contains an unsupported character")
		}
	}
	return nil
}

// validateEvidenceDir requires an absolute directory that is not an existing
// non-directory. The directory is created by NewEvidenceWriter.
func validateEvidenceDir(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("must be an absolute path outside the repository by default")
	}
	cleaned := filepath.Clean(path)
	if cleaned == string(filepath.Separator) {
		return "", fmt.Errorf("must not be the filesystem root")
	}
	if info, err := os.Stat(cleaned); err == nil && !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", cleaned)
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("cannot inspect the directory: %v", err)
	}
	return cleaned, nil
}
