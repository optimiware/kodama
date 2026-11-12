package suite

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// maxDescriptorBytes bounds the installation descriptor read.
const maxDescriptorBytes = 1 << 20

// descriptor is the part of a schema version 1 installation descriptor the
// stage plan and the token refresh need.
type descriptor struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      string `json:"revision"`
	TenantID      string `json:"tenant_id"`
	ProjectID     string `json:"project_id"`
	API           struct {
		URL           string `json:"url"`
		CAFile        string `json:"ca_file"`
		TLSServerName string `json:"tls_server_name"`
	} `json:"api"`
	Images struct {
		Tools string `json:"tools"`
	} `json:"images"`
	OAuth struct {
		Issuer         string `json:"issuer"`
		CAFile         string `json:"ca_file"`
		ClientID       string `json:"client_id"`
		RedirectURI    string `json:"redirect_uri"`
		Resolve        string `json:"resolve"`
		AccessCodeFile string `json:"access_code_file"`
	} `json:"oauthsonas"`
	Credentials struct {
		AdminFile string `json:"admin_file"`
	} `json:"credentials"`
	Git struct {
		Repositories []struct {
			Name string `json:"name"`
		} `json:"repositories"`
	} `json:"git"`
	SmokeRepository *struct {
		Name string `json:"name"`
	} `json:"smoke_repository"`
}

func readDescriptor(path string) (descriptor, error) {
	file, err := os.Open(path)
	if err != nil {
		return descriptor{}, fmt.Errorf("read the installation descriptor: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxDescriptorBytes+1))
	if err != nil {
		return descriptor{}, fmt.Errorf("read the installation descriptor: %w", err)
	}
	if len(data) > maxDescriptorBytes {
		return descriptor{}, fmt.Errorf("the installation descriptor %s is larger than %d bytes", path, maxDescriptorBytes)
	}
	var document descriptor
	if err := json.Unmarshal(data, &document); err != nil {
		return descriptor{}, fmt.Errorf("decode the installation descriptor: %w", err)
	}
	if document.SchemaVersion != 1 {
		return descriptor{}, fmt.Errorf("installation descriptor schema_version = %d; want 1", document.SchemaVersion)
	}
	return document, nil
}

// ordinaryRepository returns the repository the Git probes use: the smoke
// repository when the descriptor selects one, otherwise the first that is not
// the tenant's admin repository.
func (document descriptor) ordinaryRepository() string {
	if document.SmokeRepository != nil && document.SmokeRepository.Name != "" {
		return document.SmokeRepository.Name
	}
	for _, repository := range document.Git.Repositories {
		if repository.Name != "" && repository.Name != "admin" {
			return repository.Name
		}
	}
	return ""
}

// stageEnv is the environment the stage lanes add to the inherited one.
type stageEnv struct {
	e2e      []string
	probe    []string
	revision string
	evidence func(lane string) string
}

// probes returns the production-probe environment of one lane.
func (environment stageEnv) probes(lane string) []string {
	return append(append([]string(nil), environment.probe...), setting("Q02_EVIDENCE_DIR", environment.evidence(lane)))
}

// stageEnvironment derives both stage dialects from the one installation
// descriptor: the Q01_* inputs of test/e2e and the Q02_* inputs of
// test/qualification/production. A Q02 input the caller exported is left alone,
// so the second tenant, the Git services and any other explicit setting keep
// their value.
func stageEnvironment(options Options, getenv func(string) string) (stageEnv, error) {
	document, err := readDescriptor(options.Installation)
	if err != nil {
		return stageEnv{}, err
	}
	isSet := func(name string) bool { return strings.TrimSpace(getenv(name)) != "" }
	first := func(values ...string) string {
		for _, value := range values {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
		return ""
	}

	kubeContext := first(getenv("Q02_KUBE_CONTEXT"), getenv("Q01_KUBE_CONTEXT"))
	if kubeContext == "" {
		return stageEnv{}, errors.New("the stage target needs the cluster context (Q01_KUBE_CONTEXT)")
	}
	tenant := first(getenv("Q02_TENANT_ID"), document.TenantID, document.ProjectID)

	environment := stageEnv{
		e2e: []string{
			"KODAMA_REQUIRE_INFRA=1",
			"Q01_REQUIRE_STAGE=1",
			"Q01_REQUIRE_INSTALLATION=1",
			"Q01_SMOKE_GIT=1",
			setting("Q01_INSTALLATION_FILE", options.Installation),
			setting("Q01_EVIDENCE_DIR", options.evidence("stage-e2e")),
		},
		probe:    []string{"KODAMA_REQUIRE_INFRA=1"},
		revision: first(getenv("Q01_DEPLOYED_REVISION"), document.Revision, getenv("Q01_HARNESS_REVISION")),
		evidence: options.evidence,
	}
	derive := func(name, value string) {
		if !isSet(name) && value != "" {
			environment.probe = append(environment.probe, setting(name, value))
		}
	}
	// A probe with a missing input fails instead of skipping, so a run that
	// passes ran every selected probe.
	derive("Q02_REQUIRE_STAGE", "1")
	derive("Q02_API_URL", document.API.URL)
	derive("Q02_CA_PEM", document.API.CAFile)
	derive("Q02_TLS_SERVER_NAME", document.API.TLSServerName)
	derive("Q02_PROJECT_ID", document.ProjectID)
	derive("Q02_TENANT_ID", tenant)
	derive("Q02_IMAGE", document.Images.Tools)
	derive("Q02_KUBE_CONTEXT", kubeContext)
	derive("Q02_DEPLOYED_REVISION", first(getenv("Q01_DEPLOYED_REVISION"), document.Revision))
	derive("Q02_HARNESS_REVISION", getenv("Q01_HARNESS_REVISION"))
	derive("Q02_GIT_REPOSITORY_A", document.ordinaryRepository())
	if tenant != "" {
		derive("Q02_TENANT_NAMESPACE", "kodama-tenant-"+tenant)
		derive("Q02_GIT_SERVICE_A", "kodama-git-"+tenant)
	}
	// The probes read their credentials from the same private files as the e2e
	// proofs. A caller that exported a token value keeps it.
	if !isSet("Q02_ADMIN_TOKEN") {
		derive("Q02_ADMIN_TOKEN_FILE", first(getenv("Q01_ADMIN_TOKEN_FILE"), document.Credentials.AdminFile))
	}
	if !isSet("Q02_SECOND_ADMIN_TOKEN") {
		derive("Q02_SECOND_ADMIN_TOKEN_FILE", options.SecondAdminTokenFile)
	}
	return environment, nil
}
