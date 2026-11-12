//go:build e2e

package harness

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/optimiware/kodama/api"
)

const (
	// DefaultImage is the digest-pinned ordinary tools image. The e2e suite
	// runs its inline pipelines on it unless Q01_IMAGE or a descriptor
	// overrides it; a mutable stage tag is never a default.
	DefaultImage = "docker.io/library/buildpack-deps@sha256:712f969c9d81a35f9f00cce701faae521891d42a20b592b61bd0e6c5328fad7c"
	// DefaultExternalHarnessImage is the digest-pinned small external-harness
	// image.
	DefaultExternalHarnessImage = "docker.io/library/alpine@sha256:0f49f331d9212aff11b932863be4eb01cdf962969aa86dab7d1ee21ac6f5f52b"
)

// Input is one environment variable the e2e harness reads. Inputs come only
// from the environment or an installation descriptor; a missing required input
// is a named skip (or a failure in a required mode), never a pass.
type Input string

const (
	InputAPIURL               Input = "Q01_API_URL"
	InputCAPEM                Input = "Q01_CA_PEM"
	InputTLSServerName        Input = "Q01_TLS_SERVER_NAME"
	InputToken                Input = "Q01_TOKEN"
	InputProjectID            Input = "Q01_PROJECT_ID"
	InputKubeContext          Input = "Q01_KUBE_CONTEXT"
	InputImage                Input = "Q01_IMAGE"
	InputExternalHarnessImage Input = "Q01_EXTERNAL_HARNESS_IMAGE"
	InputWorkloadCAFile       Input = "Q01_WORKLOAD_CA_FILE"
	InputEvidenceDir          Input = "Q01_EVIDENCE_DIR"
	InputHarnessRevision      Input = "Q01_HARNESS_REVISION"
	InputDeployedRevision     Input = "Q01_DEPLOYED_REVISION"
	InputInstallationFile     Input = "Q01_INSTALLATION_FILE"
	InputSmokeGit             Input = "Q01_SMOKE_GIT"
	// InputNATSURL is the installation's broker endpoint for live event and log
	// subscription probes. It is optional outside a required run: a test that
	// needs it skips with this name rather than substituting a fake broker, and
	// Q01_REQUIRE_INSTALLATION=1 (or Q01_REQUIRE_STAGE=1) turns that skip into a
	// named failure.
	InputNATSURL Input = "Q01_NATS_URL"
	// InputNATSCAFile is the broker CA bundle for a verified-TLS live
	// subscription. It is optional for a plain local broker and required when
	// the broker URL uses tls://.
	InputNATSCAFile Input = "Q01_NATS_CA_FILE"
	// InputNATSTokenFile is a private 0600 file holding the current Kodama
	// bearer token a live probe presents as the broker auth token. The admin
	// token file is accepted instead of a raw environment token.
	InputNATSTokenFile Input = "Q01_NATS_TOKEN_FILE"
	// InputNATSTLSServerName overrides the verified broker TLS name. An empty
	// value uses the broker URL host.
	InputNATSTLSServerName Input = "Q01_NATS_TLS_SERVER_NAME"
	// InputNATSClientCertFile and InputNATSClientKeyFile are the client
	// certificate pair a broker that verifies client certificates (the stage
	// broker does) requires. Both are optional together and required together.
	InputNATSClientCertFile Input = "Q01_NATS_CLIENT_CERT_FILE"
	InputNATSClientKeyFile  Input = "Q01_NATS_CLIENT_KEY_FILE"

	InputAdminTokenFile    Input = "Q01_ADMIN_TOKEN_FILE"
	InputEngineerTokenFile Input = "Q01_ENGINEER_TOKEN_FILE"
	InputReviewerTokenFile Input = "Q01_REVIEWER_TOKEN_FILE"
	InputOperatorTokenFile Input = "Q01_OPERATOR_TOKEN_FILE"
)

// Persona is one configured installation human identity.
type Persona string

const (
	PersonaAdmin    Persona = "admin"
	PersonaEngineer Persona = "engineer"
	PersonaReviewer Persona = "reviewer"
	PersonaOperator Persona = "operator"
)

// Personas lists the four smoke personas in declaration order.
var Personas = []Persona{PersonaAdmin, PersonaEngineer, PersonaReviewer, PersonaOperator}

// TokenFileInput returns the environment input that names one persona's
// private credential file.
func (persona Persona) TokenFileInput() Input {
	switch persona {
	case PersonaAdmin:
		return InputAdminTokenFile
	case PersonaEngineer:
		return InputEngineerTokenFile
	case PersonaReviewer:
		return InputReviewerTokenFile
	case PersonaOperator:
		return InputOperatorTokenFile
	default:
		return ""
	}
}

// Inputs lists every environment input in declaration order.
var Inputs = []Input{
	InputAPIURL,
	InputCAPEM,
	InputTLSServerName,
	InputToken,
	InputProjectID,
	InputKubeContext,
	InputImage,
	InputExternalHarnessImage,
	InputWorkloadCAFile,
	InputEvidenceDir,
	InputHarnessRevision,
	InputDeployedRevision,
	InputInstallationFile,
	InputSmokeGit,
	InputNATSURL,
	InputNATSCAFile,
	InputNATSTokenFile,
	InputNATSTLSServerName,
	InputNATSClientCertFile,
	InputNATSClientKeyFile,
	InputAdminTokenFile,
	InputEngineerTokenFile,
	InputReviewerTokenFile,
	InputOperatorTokenFile,
}

// RequireStageEnv makes an unconfigured live test fail instead of skip. It is
// the e2e analogue of Q02_REQUIRE_STAGE.
const RequireStageEnv = "Q01_REQUIRE_STAGE"

// RequireStageEnvCompat is accepted alongside RequireStageEnv because the
// general qualification rule names Q02_REQUIRE_STAGE. Both mean the same
// thing: a missing input fails rather than skips.
const RequireStageEnvCompat = "Q02_REQUIRE_STAGE"

// RequireInstallationEnv makes missing installation inputs, persona
// credentials, image pins, Git endpoints and a repository hard failures.
const RequireInstallationEnv = "Q01_REQUIRE_INSTALLATION"

// RequireStage reports whether an unconfigured legacy live test must fail.
func RequireStage() bool {
	return os.Getenv(RequireStageEnv) == "1" || os.Getenv(RequireStageEnvCompat) == "1"
}

// RequireInstallation reports whether a release installation run must fail
// instead of skipping incomplete inputs.
func RequireInstallation() bool {
	return os.Getenv(RequireInstallationEnv) == "1"
}

// Secret is bearer credential material. It renders as redacted under every
// ordinary formatting verb; only Reveal returns the value.
type Secret string

// Reveal returns the credential value for the one call that must send it.
func (secret Secret) Reveal() string { return string(secret) }

// String redacts the credential.
func (secret Secret) String() string { return "<redacted>" }

// GoString redacts the credential under %#v.
func (secret Secret) GoString() string { return `harness.Secret("<redacted>")` }

// IsSet reports whether a credential value is configured.
func (secret Secret) IsSet() bool { return secret != "" }

// Installation is the normalized schema version 1 installation descriptor.
type Installation struct {
	ID                string
	Profile           string
	Runtime           string
	Revision          string
	ServerImageDigest string
	TenantID          string
	ProjectID         string

	API             InstallationAPI
	Images          InstallationImages
	OAuth           OAuthConfig
	Credentials     InstallationCredentials
	Git             GitConfig
	SmokeRepository *GitRepository
}

// InstallationCredentials are the configured private persona credential paths.
type InstallationCredentials struct {
	AdminFile    string
	EngineerFile string
	ReviewerFile string
	OperatorFile string
}

// InstallationAPI is the installation's public and workload-reachable API.
type InstallationAPI struct {
	URL            string
	CAFile         string
	TLSServerName  string
	WorkloadURL    string
	WorkloadCAFile string
}

// InstallationImages are the digest-pinned workload images.
type InstallationImages struct {
	Tools           string
	ExternalHarness string
}

// OAuthConfig is the installation's OAuthSonas configuration and personas.
type OAuthConfig struct {
	Issuer         string
	Audience       string
	CAFile         string
	ClientID       string
	RedirectURI    string
	Resolve        string
	AccessCodeFile string
	Personas       map[string]OAuthPersona
}

// OAuthPersona is one configured OAuthSonas persona.
type OAuthPersona struct {
	ID          string
	Subject     string
	PrincipalID string
}

// GitConfig is the installation's served Git metadata.
type GitConfig struct {
	URL              string
	WorkloadURL      string
	CAFile           string
	WorkloadCAFile   string
	ServiceID        string
	ServiceTokenFile string
	AdminBindingID   string
	AdminResourceID  string
	Repositories     []GitRepository
}

// GitRepository is one served repository and its resource binding.
type GitRepository struct {
	Name       string
	ResourceID string
	BindingID  string
}

// Config is the validated e2e configuration. Tokens are Secret values and never
// appear in String or GoString.
type Config struct {
	APIURL               string
	CAPEM                string
	TLSServerName        string
	Token                Secret
	ProjectID            string
	KubeContext          string
	Image                string
	ExternalHarnessImage string
	WorkloadCAFile       string
	EvidenceDir          string
	NATSURL              string
	NATSCAFile           string
	NATSTokenFile        string
	NATSTLSServerName    string
	NATSClientCertFile   string
	NATSClientKeyFile    string
	HarnessRevision      string
	DeployedRevision     string
	InstallationFile     string
	Installation         Installation

	// SmokeGitDisabled is set by Q01_SMOKE_GIT=0. The zero value keeps the
	// five-scenario behaviour, so an unconfigured run runs all five.
	SmokeGitDisabled bool

	// PersonaTokenFiles are configured private credential paths, keyed by
	// persona. personaTokens are the loaded values and are deliberately
	// unexported so a rendering cannot leak them.
	PersonaTokenFiles map[Persona]string
	personaTokens     map[Persona]Secret
}

// RequireInstallation reports this run's required mode.
func (config Config) RequireInstallation() bool { return RequireInstallation() }

// GitSmokeEnabled reports whether the two Git release-smoke scenarios run. The
// default is on; Q01_SMOKE_GIT=0 removes them for a non-Git run.
func (config Config) GitSmokeEnabled() bool { return !config.SmokeGitDisabled }

// PersonaToken returns one loaded persona credential.
func (config Config) PersonaToken(persona Persona) (Secret, bool) {
	secret, ok := config.personaTokens[persona]
	return secret, ok && secret.IsSet()
}

// brokerTokenFile returns the private file a live broker probe reads its auth
// token from. An explicit Q01_NATS_TOKEN_FILE wins; otherwise the existing
// admin token file is accepted, so a caller never has to export a raw token.
func (config Config) brokerTokenFile() string {
	if config.NATSTokenFile != "" {
		return config.NATSTokenFile
	}
	return config.PersonaTokenFiles[PersonaAdmin]
}

// PersonaSecrets returns every loaded persona credential in declaration order.
func (config Config) PersonaSecrets() []Secret {
	secrets := make([]Secret, 0, len(Personas))
	for _, persona := range Personas {
		if secret, ok := config.PersonaToken(persona); ok {
			secrets = append(secrets, secret)
		}
	}
	return secrets
}

// LoadConfig reads and validates the installation environment and optional
// descriptor. Descriptor values are applied first and explicit environment
// values override them. Absent values stay empty so a test can skip with their
// names; a present but malformed value is an error naming the input.
func LoadConfig(read func(string) string) (Config, error) {
	if read == nil {
		read = os.Getenv
	}
	config := Config{
		PersonaTokenFiles: map[Persona]string{},
		personaTokens:     map[Persona]Secret{},
	}

	if raw := strings.TrimSpace(read(string(InputInstallationFile))); raw != "" {
		path := filepath.Clean(raw)
		installation, err := loadInstallation(path)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputInstallationFile, err)
		}
		config.InstallationFile = path
		config.Installation = installation
		config.applyInstallation(installation)
	}

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
	if raw := strings.TrimSpace(read(string(InputProjectID))); raw != "" {
		if !identityPattern.MatchString(raw) {
			return Config{}, fmt.Errorf("%s: %q is not a bounded identity", InputProjectID, raw)
		}
		config.ProjectID = raw
	}
	if raw := strings.TrimSpace(read(string(InputKubeContext))); raw != "" {
		if err := validateKubeContext(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputKubeContext, err)
		}
		config.KubeContext = raw
	}
	if raw := strings.TrimSpace(read(string(InputImage))); raw != "" {
		if err := validateImageReference(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputImage, err)
		}
		config.Image = raw
	}
	if raw := strings.TrimSpace(read(string(InputExternalHarnessImage))); raw != "" {
		if err := validateImageReference(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputExternalHarnessImage, err)
		}
		config.ExternalHarnessImage = raw
	}
	if raw := strings.TrimSpace(read(string(InputWorkloadCAFile))); raw != "" {
		normalized, err := validateCAPEM(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputWorkloadCAFile, err)
		}
		config.WorkloadCAFile = normalized
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
	if raw := strings.TrimSpace(read(string(InputSmokeGit))); raw != "" {
		switch raw {
		case "0":
			config.SmokeGitDisabled = true
		case "1":
			config.SmokeGitDisabled = false
		default:
			return Config{}, fmt.Errorf("%s: %q is not 0 or 1", InputSmokeGit, raw)
		}
	}
	if raw := strings.TrimSpace(read(string(InputNATSURL))); raw != "" {
		normalized, err := validateNATSURL(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputNATSURL, err)
		}
		config.NATSURL = normalized
	}
	if raw := strings.TrimSpace(read(string(InputNATSCAFile))); raw != "" {
		normalized, err := validateCAPEM(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputNATSCAFile, err)
		}
		config.NATSCAFile = normalized
	}
	if raw := strings.TrimSpace(read(string(InputNATSTLSServerName))); raw != "" {
		if err := validateDNSName(raw); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputNATSTLSServerName, err)
		}
		config.NATSTLSServerName = raw
	}
	if raw := strings.TrimSpace(read(string(InputNATSClientCertFile))); raw != "" {
		normalized, err := validateReadableFile(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputNATSClientCertFile, err)
		}
		config.NATSClientCertFile = normalized
	}
	if raw := strings.TrimSpace(read(string(InputNATSClientKeyFile))); raw != "" {
		normalized, err := validateReadableFile(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputNATSClientKeyFile, err)
		}
		config.NATSClientKeyFile = normalized
	}
	if (config.NATSClientCertFile == "") != (config.NATSClientKeyFile == "") {
		return Config{}, fmt.Errorf("%s and %s must be supplied together", InputNATSClientCertFile, InputNATSClientKeyFile)
	}
	if raw := strings.TrimSpace(read(string(InputNATSTokenFile))); raw != "" {
		normalized, err := validateNATSTokenFile(raw)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputNATSTokenFile, err)
		}
		config.NATSTokenFile = normalized
	}

	for _, persona := range Personas {
		input := persona.TokenFileInput()
		if raw := strings.TrimSpace(read(string(input))); raw != "" {
			config.PersonaTokenFiles[persona] = filepath.Clean(raw)
		}
	}

	tokenValue := read(string(InputToken))
	if tokenValue != "" {
		if err := validateToken(tokenValue); err != nil {
			return Config{}, fmt.Errorf("%s: %w", InputToken, err)
		}
		if config.PersonaTokenFiles[PersonaAdmin] != "" {
			return Config{}, fmt.Errorf("%s and %s are ambiguous for the admin credential; configure exactly one", InputToken, InputAdminTokenFile)
		}
		config.Token = Secret(tokenValue)
	}

	for _, persona := range Personas {
		path := config.PersonaTokenFiles[persona]
		if path == "" {
			continue
		}
		secret, err := loadCredentialFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", persona.TokenFileInput(), err)
		}
		config.personaTokens[persona] = secret
		if persona == PersonaAdmin && !config.Token.IsSet() {
			config.Token = secret
		}
	}
	return config, nil
}

// applyInstallation seeds the legacy fields from a validated descriptor.
func (config *Config) applyInstallation(installation Installation) {
	config.APIURL = installation.API.URL
	config.CAPEM = installation.API.CAFile
	config.TLSServerName = installation.API.TLSServerName
	config.WorkloadCAFile = installation.API.WorkloadCAFile
	config.ProjectID = installation.ProjectID
	config.Image = installation.Images.Tools
	config.ExternalHarnessImage = installation.Images.ExternalHarness
	config.PersonaTokenFiles[PersonaAdmin] = installation.Credentials.AdminFile
	config.PersonaTokenFiles[PersonaEngineer] = installation.Credentials.EngineerFile
	config.PersonaTokenFiles[PersonaReviewer] = installation.Credentials.ReviewerFile
	config.PersonaTokenFiles[PersonaOperator] = installation.Credentials.OperatorFile
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
	return "missing e2e input(s): " + strings.Join(names, ", ")
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
	case InputToken:
		return string(config.Token)
	case InputProjectID:
		return config.ProjectID
	case InputKubeContext:
		return config.KubeContext
	case InputImage:
		return config.Image
	case InputExternalHarnessImage:
		return config.ExternalHarnessImage
	case InputWorkloadCAFile:
		return config.WorkloadCAFile
	case InputEvidenceDir:
		return config.EvidenceDir
	case InputNATSURL:
		return config.NATSURL
	case InputNATSCAFile:
		return config.NATSCAFile
	case InputNATSTokenFile:
		return config.brokerTokenFile()
	case InputNATSTLSServerName:
		return config.NATSTLSServerName
	case InputNATSClientCertFile:
		return config.NATSClientCertFile
	case InputNATSClientKeyFile:
		return config.NATSClientKeyFile
	case InputHarnessRevision:
		return config.HarnessRevision
	case InputDeployedRevision:
		return config.DeployedRevision
	case InputInstallationFile:
		return config.InstallationFile
	case InputSmokeGit:
		if config.GitSmokeEnabled() {
			return "1"
		}
		return "0"
	case InputAdminTokenFile:
		return config.PersonaTokenFiles[PersonaAdmin]
	case InputEngineerTokenFile:
		return config.PersonaTokenFiles[PersonaEngineer]
	case InputReviewerTokenFile:
		return config.PersonaTokenFiles[PersonaReviewer]
	case InputOperatorTokenFile:
		return config.PersonaTokenFiles[PersonaOperator]
	default:
		return ""
	}
}

// InstallationMissing returns every input the installation-access preflight
// requires, including digest-pinned images, the Git endpoint and a repository.
// It is a named list, never a silent skip.
func (config Config) InstallationMissing() MissingInputs {
	return config.installationMissing(true)
}

// InstallationMissingNonGit returns the release-smoke input set for a
// non-Git run: the API, CA, project, four personas and both digest pins,
// with no Git endpoint or repository. It is a named list, never a silent skip.
func (config Config) InstallationMissingNonGit() MissingInputs {
	return config.installationMissing(false)
}

func (config Config) installationMissing(includeGit bool) MissingInputs {
	missing := config.Missing(
		InputAPIURL,
		InputCAPEM,
		InputProjectID,
		InputAdminTokenFile,
		InputEngineerTokenFile,
		InputReviewerTokenFile,
		InputOperatorTokenFile,
		InputWorkloadCAFile,
		InputExternalHarnessImage,
	)
	if config.Image == "" || config.ImageDigest() == "" {
		missing = append(missing, Input("image digest (Q01_IMAGE or images.tools)"))
	}
	if config.ExternalHarnessImage == "" || config.ExternalHarnessDigest() == "" {
		missing = append(missing, Input("external harness image digest (Q01_EXTERNAL_HARNESS_IMAGE or images.external_harness)"))
	}
	if !includeGit {
		return missing
	}
	if config.GitURL() == "" {
		missing = append(missing, Input("git.url"))
	}
	if _, ok := config.Repository(); !ok {
		missing = append(missing, Input("git.repositories"))
	}
	return missing
}

// ImageReference returns the image the live suite runs: the configured image,
// the descriptor tools image, or the digest-pinned default. A reference that
// also carries a tag is normalized to repository@digest: the digest remains the
// immutable pin, while the admission resolver refuses the combined form.
func (config Config) ImageReference() string {
	if config.Image != "" {
		return canonicalImageReference(config.Image)
	}
	if config.Installation.Images.Tools != "" {
		return canonicalImageReference(config.Installation.Images.Tools)
	}
	return DefaultImage
}

// ExternalHarnessReference returns the configured small external-harness image.
func (config Config) ExternalHarnessReference() string {
	if config.ExternalHarnessImage != "" {
		return canonicalImageReference(config.ExternalHarnessImage)
	}
	if config.Installation.Images.ExternalHarness != "" {
		return canonicalImageReference(config.Installation.Images.ExternalHarness)
	}
	return DefaultExternalHarnessImage
}

// canonicalImageReference drops a redundant tag from a digest-pinned reference.
func canonicalImageReference(image string) string {
	separator := strings.LastIndex(image, "@")
	if separator < 0 || separator == len(image)-1 {
		return image
	}
	repository := image[:separator]
	digest := image[separator+1:]
	lastColon := strings.LastIndex(repository, ":")
	lastSlash := strings.LastIndex(repository, "/")
	if lastColon > lastSlash {
		repository = repository[:lastColon]
	}
	return repository + "@" + digest
}

// ImageDigest returns the digest of the configured pinned image, or empty when
// the image is unconfigured or not digest-pinned.
func (config Config) ImageDigest() string { return digestOf(config.ImageReference()) }

// ExternalHarnessDigest returns the external-harness image digest, or empty.
func (config Config) ExternalHarnessDigest() string {
	return digestOf(config.ExternalHarnessReference())
}

// digestOf returns the trailing digest of a reference.
func digestOf(image string) string {
	index := strings.LastIndex(image, "@")
	if index < 0 || index == len(image)-1 {
		return ""
	}
	return image[index+1:]
}

// WorkloadAPIURL returns the API URL workloads use.
func (config Config) WorkloadAPIURL() string { return config.Installation.API.WorkloadURL }

// GitURL returns the host-reachable served Git endpoint.
func (config Config) GitURL() string { return config.Installation.Git.URL }

// GitWorkloadURL returns the workload-reachable served Git endpoint.
func (config Config) GitWorkloadURL() string { return config.Installation.Git.WorkloadURL }

// GitCAFile returns the host Git trust root, falling back to the API CA.
func (config Config) GitCAFile() string {
	if config.Installation.Git.CAFile != "" {
		return config.Installation.Git.CAFile
	}
	return config.CAPEM
}

// GitWorkloadCAFile returns the workload Git trust root, falling back to the
// explicit Q01_WORKLOAD_CA_FILE and then the host Git CA.
func (config Config) GitWorkloadCAFile() string {
	if config.WorkloadCAFile != "" {
		return config.WorkloadCAFile
	}
	if config.Installation.Git.WorkloadCAFile != "" {
		return config.Installation.Git.WorkloadCAFile
	}
	return config.GitCAFile()
}

// GitServiceID returns the enforcing Git service id.
func (config Config) GitServiceID() string { return config.Installation.Git.ServiceID }

// GitServiceTokenFile returns the Git service credential file.
func (config Config) GitServiceTokenFile() string { return config.Installation.Git.ServiceTokenFile }

// Repository returns the selected smoke repository, or the first configured
// installation repository.
func (config Config) Repository() (GitRepository, bool) {
	if config.Installation.SmokeRepository != nil {
		return *config.Installation.SmokeRepository, true
	}
	if len(config.Installation.Git.Repositories) > 0 {
		return config.Installation.Git.Repositories[0], true
	}
	return GitRepository{}, false
}

// OrdinaryRepositories returns the installation's repositories other than the
// tenant admin repository, the selected smoke repository first, each once.
func (config Config) OrdinaryRepositories() []GitRepository {
	git := config.Installation.Git
	candidates := git.Repositories
	if config.Installation.SmokeRepository != nil {
		candidates = append([]GitRepository{*config.Installation.SmokeRepository}, candidates...)
	}
	var ordinary []GitRepository
	seen := map[string]bool{}
	for _, repository := range candidates {
		admin := repository.Name == "admin" || (git.AdminResourceID != "" && repository.ResourceID == git.AdminResourceID)
		if repository.Name == "" || repository.ResourceID == "" || admin || seen[repository.ResourceID] {
			continue
		}
		seen[repository.ResourceID] = true
		ordinary = append(ordinary, repository)
	}
	return ordinary
}

// RepositoryForSlot spreads the Git scenarios over the ordinary repositories:
// a slot always maps to the same repository, and slots beyond the last
// repository wrap around. An installation with one repository maps every slot
// to it, exactly like Repository.
func (config Config) RepositoryForSlot(slot int) (GitRepository, bool) {
	ordinary := config.OrdinaryRepositories()
	if len(ordinary) == 0 || slot < 0 {
		return config.Repository()
	}
	return ordinary[slot%len(ordinary)], true
}

// RepositoryName returns the selected repository name or empty.
func (config Config) RepositoryName() string {
	repository, ok := config.Repository()
	if !ok {
		return ""
	}
	return repository.Name
}

// String renders a configuration summary with the token redacted.
func (config Config) String() string {
	return fmt.Sprintf(
		"Config{APIURL:%s, CAPEM:%s, TLSServerName:%s, Token:<redacted>, ProjectID:%s, KubeContext:%s, Image:%s, ExternalHarnessImage:%s, WorkloadCAFile:%s, EvidenceDir:%s, NATSURL:%s, NATSCAFile:%s, NATSTokenFile:%s, NATSTLSServerName:%s, HarnessRevision:%s, DeployedRevision:%s, InstallationFile:%s, InstallationID:%s, GitSmoke:%t}",
		config.APIURL, config.CAPEM, config.TLSServerName, config.ProjectID,
		config.KubeContext, config.Image, config.ExternalHarnessImage, config.WorkloadCAFile,
		config.EvidenceDir, config.NATSURL, config.NATSCAFile, config.brokerTokenFile(),
		config.NATSTLSServerName, config.HarnessRevision, config.DeployedRevision,
		config.InstallationFile, config.Installation.ID, config.GitSmokeEnabled(),
	)
}

// GoString renders a %#v configuration summary with the token redacted.
func (config Config) GoString() string { return config.String() }

var (
	identityPattern      = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	labelPattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	hostnameLabelPattern = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9]{0,61}[A-Za-z0-9])?$`)
	imagePattern         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]{0,511}$`)
	revisionPattern      = regexp.MustCompile(`^[a-f0-9]{7,64}$`)
	sha256DigestPattern  = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	principalIDPattern   = regexp.MustCompile(`^oidc-[a-f0-9]{32}$`)
	repositoryPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)
	resolvePattern       = regexp.MustCompile(`^[^:\s]+:[0-9]{1,5}:[^:\s]+$`)
)

// validateNATSURL accepts a broker URL for live subscription probes. Only the
// plain and verified-TLS nats schemes are accepted, and userinfo is refused so
// a credential is never embedded in a URL that could be rendered in evidence.
func validateNATSURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("is not a valid URL: %v", err)
	}
	switch parsed.Scheme {
	case "nats", "tls":
	default:
		return "", fmt.Errorf("must use nats:// or tls://")
	}
	if parsed.Opaque != "" {
		return "", fmt.Errorf("must not be opaque")
	}
	if parsed.Host == "" || parsed.Port() == "" {
		return "", fmt.Errorf("must be an absolute host:port endpoint")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("must not carry user information")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.Path != "" {
		return "", fmt.Errorf("must not carry a path, query or fragment")
	}
	if strings.ContainsAny(raw, "\r\n\t") {
		return "", fmt.Errorf("must not contain control characters")
	}
	return raw, nil
}

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

// validateRedirectURI accepts an http(s) absolute redirect URI. A loopback
// callback is plain http by design.
func validateRedirectURI(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("is not a valid URL: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("must use http or https")
	}
	if parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("must be an absolute URI without user information or fragment")
	}
	return nil
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

// validateReadableFile requires an existing readable regular file and returns
// its cleaned path. It reads no bytes.
func validateReadableFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the file")
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("is not a regular file")
	}
	return filepath.Clean(path), nil
}

// validateNATSTokenFile requires a private token file a live broker probe can
// read without ever rendering its contents: an existing regular file with mode
// 0600, owned by the calling user. It reads no bytes, so a malformed path fails
// before any credential is touched.
func validateNATSTokenFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the token file")
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the token file is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return "", fmt.Errorf("the token file must be a 0600 file owned by the caller")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("the token file must be a 0600 file owned by the caller")
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("the token file is empty")
	}
	if info.Size() > int64(api.MaxCredentialBytes)+1 {
		return "", fmt.Errorf("the token file is larger than %d bytes", api.MaxCredentialBytes)
	}
	handle, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the token file")
	}
	if err := handle.Close(); err != nil {
		return "", fmt.Errorf("cannot read the token file")
	}
	return filepath.Clean(path), nil
}

// loadNATSTokenFile validates and reads one private broker token file. The
// returned Secret redacts itself under every ordinary formatting verb.
func loadNATSTokenFile(path string) (Secret, error) {
	normalized, err := validateNATSTokenFile(path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(normalized)
	if err != nil {
		return "", fmt.Errorf("cannot read the token file")
	}
	token := strings.TrimSpace(string(data))
	if err := validateToken(token); err != nil {
		return "", fmt.Errorf("the token file: %w", err)
	}
	return Secret(token), nil
}

// validateImageReference accepts a bounded ordinary image reference. A tag is
// allowed for an explicit non-release override; release evidence requires a
// digest.
func validateImageReference(image string) error {
	if !imagePattern.MatchString(image) {
		return fmt.Errorf("%q is not a bounded image reference", image)
	}
	if strings.ContainsAny(image, " \t\r\n") {
		return fmt.Errorf("must not contain whitespace")
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

// loadCredentialFile reads one private token file without echoing its value.
func loadCredentialFile(path string) (Secret, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the credential file: %v", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the credential path is not a regular file")
	}
	if info.Size() > int64(api.MaxCredentialBytes)+1 {
		return "", fmt.Errorf("the credential file is larger than %d bytes", api.MaxCredentialBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot read the credential file")
	}
	token := strings.TrimSpace(string(data))
	if err := validateToken(token); err != nil {
		return "", fmt.Errorf("the credential file: %w", err)
	}
	return Secret(token), nil
}

// descriptor schema version 1. The nested shape is exactly the documented
// installation descriptor; unknown members are refused so a typo cannot be
// silently ignored.
type installationDescriptor struct {
	SchemaVersion     int                   `json:"schema_version"`
	InstallationID    string                `json:"installation_id"`
	Profile           string                `json:"profile"`
	Runtime           string                `json:"runtime"`
	Revision          string                `json:"revision"`
	ServerImageDigest string                `json:"server_image_digest"`
	TenantID          string                `json:"tenant_id"`
	ProjectID         string                `json:"project_id"`
	API               descriptorAPI         `json:"api"`
	Images            descriptorImages      `json:"images"`
	OAuth             descriptorOAuth       `json:"oauthsonas"`
	Credentials       descriptorCredentials `json:"credentials"`
	Git               descriptorGit         `json:"git"`
	SmokeRepository   *descriptorRepository `json:"smoke_repository,omitempty"`
}

type descriptorAPI struct {
	URL            string `json:"url"`
	CAFile         string `json:"ca_file"`
	TLSServerName  string `json:"tls_server_name"`
	WorkloadURL    string `json:"workload_url"`
	WorkloadCAFile string `json:"workload_ca_file"`
}

type descriptorImages struct {
	Tools           string `json:"tools"`
	ExternalHarness string `json:"external_harness"`
}

type descriptorOAuth struct {
	Issuer         string                       `json:"issuer"`
	Audience       string                       `json:"audience"`
	CAFile         string                       `json:"ca_file"`
	ClientID       string                       `json:"client_id"`
	RedirectURI    string                       `json:"redirect_uri"`
	Resolve        string                       `json:"resolve"`
	AccessCodeFile string                       `json:"access_code_file"`
	Personas       map[string]descriptorPersona `json:"personas"`
}

type descriptorPersona struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	PrincipalID string `json:"principal_id"`
}

type descriptorCredentials struct {
	AdminFile    string `json:"admin_file"`
	EngineerFile string `json:"engineer_file"`
	ReviewerFile string `json:"reviewer_file"`
	OperatorFile string `json:"operator_file"`
}

type descriptorGit struct {
	URL              string                 `json:"url"`
	WorkloadURL      string                 `json:"workload_url"`
	CAFile           string                 `json:"ca_file"`
	WorkloadCAFile   string                 `json:"workload_ca_file"`
	ServiceID        string                 `json:"service_id"`
	ServiceTokenFile string                 `json:"service_token_file"`
	AdminBindingID   string                 `json:"admin_binding_id"`
	AdminResourceID  string                 `json:"admin_resource_id"`
	Repositories     []descriptorRepository `json:"repositories"`
}

type descriptorRepository struct {
	Name       string `json:"name"`
	ResourceID string `json:"resource_id"`
	BindingID  string `json:"binding_id"`
}

// maxDescriptorBytes bounds one descriptor read.
const maxDescriptorBytes = 1 << 20

var (
	descriptorRootKeys       = []string{"schema_version", "installation_id", "profile", "runtime", "revision", "server_image_digest", "tenant_id", "project_id", "api", "images", "oauthsonas", "credentials", "git", "smoke_repository"}
	descriptorAPIKeys        = []string{"url", "ca_file", "tls_server_name", "workload_url", "workload_ca_file"}
	descriptorImagesKeys     = []string{"tools", "external_harness"}
	descriptorOAuthKeys      = []string{"issuer", "audience", "ca_file", "client_id", "redirect_uri", "resolve", "access_code_file", "personas"}
	descriptorPersonaKeys    = []string{"id", "subject", "principal_id"}
	descriptorCredentialKeys = []string{"admin_file", "engineer_file", "reviewer_file", "operator_file"}
	descriptorGitKeys        = []string{"url", "workload_url", "ca_file", "workload_ca_file", "service_id", "service_token_file", "admin_binding_id", "admin_resource_id", "repositories"}
	descriptorRepoKeys       = []string{"name", "resource_id", "binding_id"}
)

// rejectCaseVariantKeys refuses a descriptor whose member is spelled in another
// case. encoding/json matches object members case-insensitively, so without
// this check "API" or "Subject" would silently configure the field.
func rejectCaseVariantKeys(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil // the typed decoder reports the malformed document
	}
	if err := checkObjectKeys("", root, descriptorRootKeys); err != nil {
		return err
	}
	if err := checkObjectKeys("api", root["api"], descriptorAPIKeys); err != nil {
		return err
	}
	if err := checkObjectKeys("images", root["images"], descriptorImagesKeys); err != nil {
		return err
	}
	if err := checkObjectKeys("oauthsonas", root["oauthsonas"], descriptorOAuthKeys); err != nil {
		return err
	}
	if err := checkObjectKeys("credentials", root["credentials"], descriptorCredentialKeys); err != nil {
		return err
	}
	if err := checkObjectKeys("git", root["git"], descriptorGitKeys); err != nil {
		return err
	}
	if err := checkObjectKeys("smoke_repository", root["smoke_repository"], descriptorRepoKeys); err != nil {
		return err
	}
	var personas map[string]json.RawMessage
	if len(root["oauthsonas"]) > 0 {
		var oauth map[string]json.RawMessage
		if err := json.Unmarshal(root["oauthsonas"], &oauth); err == nil {
			if err := json.Unmarshal(oauth["personas"], &personas); err == nil {
				for name, raw := range personas {
					if err := checkObjectKeys("oauthsonas.personas."+name, raw, descriptorPersonaKeys); err != nil {
						return err
					}
				}
			}
		}
	}
	var repositories []json.RawMessage
	if len(root["git"]) > 0 {
		var git map[string]json.RawMessage
		if err := json.Unmarshal(root["git"], &git); err == nil {
			if err := json.Unmarshal(git["repositories"], &repositories); err == nil {
				for index, raw := range repositories {
					if err := checkObjectKeys(fmt.Sprintf("git.repositories[%d]", index), raw, descriptorRepoKeys); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// checkObjectKeys refuses a case-variant spelling of an allowed member.
func checkObjectKeys(prefix string, value any, allowed []string) error {
	object, ok := value.(map[string]json.RawMessage)
	if !ok {
		raw, ok := value.(json.RawMessage)
		if !ok || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil // a non-object is reported by the typed decoder
		}
	}
	for key := range object {
		known := slices.Contains(allowed, key)
		if known {
			continue
		}
		for _, wanted := range allowed {
			if strings.EqualFold(key, wanted) {
				where := prefix
				if where == "" {
					where = "descriptor"
				}
				return fmt.Errorf("%s key %q must be spelled %q", where, key, wanted)
			}
		}
	}
	return nil
}

// loadInstallation reads and validates one schema version 1 descriptor.
func loadInstallation(path string) (Installation, error) {
	info, err := os.Stat(path)
	if err != nil {
		return Installation{}, fmt.Errorf("cannot read the descriptor: %v", err)
	}
	if !info.Mode().IsRegular() {
		return Installation{}, fmt.Errorf("the descriptor path is not a regular file")
	}
	if info.Size() > maxDescriptorBytes {
		return Installation{}, fmt.Errorf("the descriptor exceeds %d bytes", maxDescriptorBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Installation{}, fmt.Errorf("cannot read the descriptor")
	}
	if err := rejectCaseVariantKeys(data); err != nil {
		return Installation{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var descriptor installationDescriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return Installation{}, fmt.Errorf("the descriptor is not valid schema version 1 JSON: %v", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return Installation{}, fmt.Errorf("the descriptor must contain exactly one JSON document")
	}
	if descriptor.SchemaVersion != 1 {
		return Installation{}, fmt.Errorf("schema_version must be 1")
	}
	installation := Installation{
		ID:                descriptor.InstallationID,
		Profile:           descriptor.Profile,
		Runtime:           descriptor.Runtime,
		Revision:          descriptor.Revision,
		ServerImageDigest: descriptor.ServerImageDigest,
		TenantID:          descriptor.TenantID,
		ProjectID:         descriptor.ProjectID,
		API: InstallationAPI{
			URL:            descriptor.API.URL,
			CAFile:         descriptor.API.CAFile,
			TLSServerName:  descriptor.API.TLSServerName,
			WorkloadURL:    descriptor.API.WorkloadURL,
			WorkloadCAFile: descriptor.API.WorkloadCAFile,
		},
		Images: InstallationImages{
			Tools:           descriptor.Images.Tools,
			ExternalHarness: descriptor.Images.ExternalHarness,
		},
		Credentials: InstallationCredentials{
			AdminFile:    descriptor.Credentials.AdminFile,
			EngineerFile: descriptor.Credentials.EngineerFile,
			ReviewerFile: descriptor.Credentials.ReviewerFile,
			OperatorFile: descriptor.Credentials.OperatorFile,
		},
		OAuth: OAuthConfig{
			Issuer:         descriptor.OAuth.Issuer,
			Audience:       descriptor.OAuth.Audience,
			CAFile:         descriptor.OAuth.CAFile,
			ClientID:       descriptor.OAuth.ClientID,
			RedirectURI:    descriptor.OAuth.RedirectURI,
			Resolve:        descriptor.OAuth.Resolve,
			AccessCodeFile: descriptor.OAuth.AccessCodeFile,
			Personas:       map[string]OAuthPersona{},
		},
		Git: GitConfig{
			URL:              descriptor.Git.URL,
			WorkloadURL:      descriptor.Git.WorkloadURL,
			CAFile:           descriptor.Git.CAFile,
			WorkloadCAFile:   descriptor.Git.WorkloadCAFile,
			ServiceID:        descriptor.Git.ServiceID,
			ServiceTokenFile: descriptor.Git.ServiceTokenFile,
			AdminBindingID:   descriptor.Git.AdminBindingID,
			AdminResourceID:  descriptor.Git.AdminResourceID,
		},
	}
	for name, persona := range descriptor.OAuth.Personas {
		installation.OAuth.Personas[name] = OAuthPersona(persona)
	}
	for _, repository := range descriptor.Git.Repositories {
		installation.Git.Repositories = append(installation.Git.Repositories, GitRepository(repository))
	}
	if descriptor.SmokeRepository != nil {
		smoke := GitRepository(*descriptor.SmokeRepository)
		installation.SmokeRepository = &smoke
	}
	if err := validateDescriptor(&installation); err != nil {
		return Installation{}, err
	}
	return installation, nil
}

// validateDescriptor validates every present descriptor field and normalizes
// the derived persona identities.
func validateDescriptor(installation *Installation) error {
	checks := []struct {
		path  string
		value string
		check func(string) error
	}{
		{path: "installation_id", value: installation.ID, check: validateIdentity},
		{path: "profile", value: installation.Profile, check: validateLabel},
		{path: "runtime", value: installation.Runtime, check: validateLabel},
		{path: "revision", value: installation.Revision, check: validateRevision},
		{path: "server_image_digest", value: installation.ServerImageDigest, check: validateSHA256},
		{path: "tenant_id", value: installation.TenantID, check: validateIdentity},
		{path: "project_id", value: installation.ProjectID, check: validateIdentity},
		{path: "api.url", value: installation.API.URL, check: validateHTTPSURL},
		{path: "api.ca_file", value: installation.API.CAFile, check: validateCAFile},
		{path: "api.tls_server_name", value: installation.API.TLSServerName, check: validateDNSName},
		{path: "api.workload_url", value: installation.API.WorkloadURL, check: validateHTTPSURL},
		{path: "api.workload_ca_file", value: installation.API.WorkloadCAFile, check: validateCAFile},
		{path: "images.tools", value: installation.Images.Tools, check: validateImageReference},
		{path: "images.external_harness", value: installation.Images.ExternalHarness, check: validateImageReference},
		{path: "oauthsonas.issuer", value: installation.OAuth.Issuer, check: validateHTTPSURL},
		{path: "oauthsonas.audience", value: installation.OAuth.Audience, check: validateHTTPSURL},
		{path: "oauthsonas.ca_file", value: installation.OAuth.CAFile, check: validateCAFile},
		{path: "oauthsonas.client_id", value: installation.OAuth.ClientID, check: validateIdentity},
		{path: "oauthsonas.redirect_uri", value: installation.OAuth.RedirectURI, check: validateRedirectURI},
		{path: "oauthsonas.resolve", value: installation.OAuth.Resolve, check: validateResolve},
		{path: "oauthsonas.access_code_file", value: installation.OAuth.AccessCodeFile, check: validateRegularFile},
		{path: "credentials.admin_file", value: installation.Credentials.AdminFile, check: validateRegularFile},
		{path: "credentials.engineer_file", value: installation.Credentials.EngineerFile, check: validateRegularFile},
		{path: "credentials.reviewer_file", value: installation.Credentials.ReviewerFile, check: validateRegularFile},
		{path: "credentials.operator_file", value: installation.Credentials.OperatorFile, check: validateRegularFile},
		{path: "git.url", value: installation.Git.URL, check: validateHTTPSURL},
		{path: "git.workload_url", value: installation.Git.WorkloadURL, check: validateHTTPSURL},
		{path: "git.ca_file", value: installation.Git.CAFile, check: validateCAFile},
		{path: "git.workload_ca_file", value: installation.Git.WorkloadCAFile, check: validateCAFile},
		{path: "git.service_id", value: installation.Git.ServiceID, check: validateIdentity},
		{path: "git.service_token_file", value: installation.Git.ServiceTokenFile, check: validateRegularFile},
		{path: "git.admin_binding_id", value: installation.Git.AdminBindingID, check: validateIdentity},
		{path: "git.admin_resource_id", value: installation.Git.AdminResourceID, check: validateIdentity},
	}
	for _, check := range checks {
		if check.value == "" {
			continue
		}
		if err := check.check(check.value); err != nil {
			return fmt.Errorf("%s %w", check.path, err)
		}
	}
	for name, persona := range installation.OAuth.Personas {
		if err := validateDescriptorPersona(installation.OAuth.Issuer, name, persona); err != nil {
			return fmt.Errorf("oauthsonas.personas.%s %w", name, err)
		}
		normalized := persona
		if normalized.ID == "" {
			normalized.ID = name
		}
		if normalized.PrincipalID == "" && installation.OAuth.Issuer != "" && normalized.Subject != "" {
			normalized.PrincipalID = derivePrincipalID(installation.OAuth.Issuer, normalized.Subject)
		}
		installation.OAuth.Personas[name] = normalized
	}
	for index, repository := range installation.Git.Repositories {
		if err := validateRepository(repository); err != nil {
			return fmt.Errorf("git.repositories[%d] %w", index, err)
		}
	}
	if installation.SmokeRepository != nil {
		if err := validateRepository(*installation.SmokeRepository); err != nil {
			return fmt.Errorf("smoke_repository %w", err)
		}
	}
	return nil
}

func validateDescriptorPersona(issuer, name string, persona OAuthPersona) error {
	if !identityPattern.MatchString(name) {
		return fmt.Errorf("is not a bounded persona id")
	}
	if persona.ID != "" && persona.ID != name {
		return fmt.Errorf("id %q does not match its persona key", persona.ID)
	}
	if persona.Subject != "" && !identityPattern.MatchString(persona.Subject) {
		return fmt.Errorf("subject is not a bounded identity")
	}
	if persona.PrincipalID != "" {
		if !principalIDPattern.MatchString(persona.PrincipalID) {
			return fmt.Errorf("principal_id is not a derived oidc- identity")
		}
		if issuer != "" && persona.Subject != "" {
			if want := derivePrincipalID(issuer, persona.Subject); persona.PrincipalID != want {
				return fmt.Errorf("principal_id does not match the configured issuer and subject")
			}
		}
	}
	return nil
}

func validateRepository(repository GitRepository) error {
	if !repositoryPattern.MatchString(repository.Name) || strings.Contains(repository.Name, "..") {
		return fmt.Errorf("name %q is not a bounded repository name", repository.Name)
	}
	if !identityPattern.MatchString(repository.ResourceID) || !identityPattern.MatchString(repository.BindingID) {
		return fmt.Errorf("resource_id and binding_id must be bounded identities")
	}
	return nil
}

func validateIdentity(value string) error {
	if !identityPattern.MatchString(value) {
		return fmt.Errorf("%q is not a bounded identity", value)
	}
	return nil
}

func validateLabel(value string) error {
	if !labelPattern.MatchString(value) {
		return fmt.Errorf("%q is not a bounded label", value)
	}
	return nil
}

func validateRevision(value string) error {
	if !revisionPattern.MatchString(value) {
		return fmt.Errorf("%q is not a git revision", value)
	}
	return nil
}

func validateSHA256(value string) error {
	if !sha256DigestPattern.MatchString(value) {
		return fmt.Errorf("%q is not a sha256 digest", value)
	}
	return nil
}

func validateHTTPSURL(value string) error {
	_, err := validateAPIURL(value)
	return err
}

func validateCAFile(path string) error {
	_, err := validateCAPEM(path)
	return err
}

func validateRegularFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("cannot read the file: %v", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("is not a regular file")
	}
	return nil
}

func validateResolve(value string) error {
	if !resolvePattern.MatchString(value) {
		return fmt.Errorf("is not a host:port:address resolve value")
	}
	return nil
}

// derivePrincipalID mirrors the product's verified human principal derivation.
func derivePrincipalID(issuer, subject string) string {
	sum := sha256.Sum256([]byte(issuer + "\x00" + subject))
	return "oidc-" + hex.EncodeToString(sum[:16])
}
