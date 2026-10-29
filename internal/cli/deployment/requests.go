package deployment

import (
	"net/http"
	"net/url"
	"regexp"

	"github.com/optimiware/kodama/internal/cli/client"
	"github.com/optimiware/kodama/internal/cli/root"
	"github.com/optimiware/kodama/internal/domain"
)

var (
	idPattern     = regexp.MustCompile(`^dep_[a-f0-9]{32}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	commitPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	repoPattern   = regexp.MustCompile(`^https://[^\s@/?#]+(/[^\s@?#]*)?$`)
	keyPattern    = regexp.MustCompile(`^[A-Za-z0-9._~:-]{1,128}$`)
)

// sourceBody, imageBody and createBody are the Source, Image and
// DeploymentCreate of api/delivery.yaml and nothing else: the environment, its
// configuration pin, the run, step and action key are derived by the server
// from the attempt and are never sent.
type sourceBody struct {
	RepositoryURL string `json:"repositoryUrl"`
	Commit        string `json:"commit"`
}

type imageBody struct {
	ArtifactID string `json:"artifactId"`
	Digest     string `json:"digest"`
}

type artifactBody struct {
	Kind       string `json:"kind"`
	ArtifactID string `json:"artifactId"`
	Digest     string `json:"digest"`
	MediaType  string `json:"mediaType,omitempty"`
}

type createBody struct {
	AttemptID string        `json:"attemptId"`
	Source    sourceBody    `json:"source"`
	Image     imageBody     `json:"image"`
	Artifact  *artifactBody `json:"artifact,omitempty"`
}

func listRequest(scope client.Scope, q url.Values) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"deployments"}, Query: q}
}

func getRequest(scope client.Scope, id string) client.Request {
	return client.Request{Method: http.MethodGet, Scope: scope, Path: []string{"deployments", id}}
}

func createRequest(scope client.Scope, key string, body createBody) client.Request {
	return client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"deployments"}, IdempotencyKey: key, Body: body}
}

// cleanupRequest has no body: the request names the recorded deployment and the
// version the caller observed, never objects, namespaces or environments.
func cleanupRequest(scope client.Scope, id string, v domain.Version) client.Request {
	return client.Request{Method: http.MethodPost, Scope: scope, Path: []string{"deployments", id, "cleanup"}, IfVersion: v}
}

// createInputs checks that every explicit input is present and well formed
// before any request. It enforces wire syntax only; nothing is defaulted,
// fetched or recomputed, so the values sent are exactly the ones supplied.
func createInputs(set map[string]bool, attempt, repo, commit, artifact, digest, key, kind, mediaType string) (createBody, string, error) {
	var zero createBody
	for _, name := range []string{"attempt-id", "repository-url", "commit", "image-artifact-id", "image-digest", "idempotency-key"} {
		if !set[name] {
			return zero, "", root.Usagef("--%s is required", name)
		}
	}
	if !keyPattern.MatchString(key) {
		return zero, "", root.Usagef("--idempotency-key must be 1-128 characters of letters, digits and . _ ~ : -")
	}
	if _, err := requireText(set, "attempt-id", attempt); err != nil {
		return zero, "", err
	}
	if _, err := requireText(set, "image-artifact-id", artifact); err != nil {
		return zero, "", err
	}
	if !repoPattern.MatchString(repo) {
		return zero, "", root.Usagef("--repository-url must be an https URL without credentials")
	}
	if !commitPattern.MatchString(commit) {
		return zero, "", root.Usagef("--commit must be 40 or 64 lowercase hex digits")
	}
	if !digestPattern.MatchString(digest) {
		return zero, "", root.Usagef("--image-digest must be sha256: followed by 64 lowercase hex digits")
	}
	body := createBody{
		AttemptID: attempt,
		Source:    sourceBody{RepositoryURL: repo, Commit: commit},
		Image:     imageBody{ArtifactID: artifact, Digest: digest},
	}
	switch kind {
	case "", "ociImage":
	case "bundle":
		if mediaType == "" {
			return zero, "", root.Usagef("--artifact-media-type is required for a bundle, for example application/vnd.kodama.bundle.v1+tar+zstd")
		}
		body.Artifact = &artifactBody{Kind: kind, ArtifactID: artifact, Digest: digest, MediaType: mediaType}
	default:
		return zero, "", root.Usagef("--artifact-kind must be ociImage or bundle")
	}
	return body, key, nil
}
