package delivery

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	repoRE   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[a-z0-9]+([._-][a-z0-9]+)*)+$`)
)

// ValidDigest reports whether s is an OCI sha256 digest.
func ValidDigest(s string) bool { return digestRE.MatchString(s) }

// ValidRepository reports whether s is a registry repository (host[:port]/path), without tag or digest.
func ValidRepository(s string) bool { return repoRE.MatchString(s) }

// Registry checks images in an OCI registry over its distribution API.
type Registry struct {
	HTTP *http.Client
	// Insecure selects http. Loopback hosts and explicit configuration only.
	Insecure bool
}

var manifestTypes = strings.Join([]string{
	"application/vnd.oci.image.manifest.v1+json", "application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json", "application/vnd.docker.distribution.manifest.list.v2+json",
}, ", ")

// ErrImageMissing means the registry does not hold the digest.
var ErrImageMissing = errors.New("delivery: the registry does not hold this image digest")

// Digest returns the manifest digest of repo at reference (a tag or digest).
func (r *Registry) Digest(ctx context.Context, repo, reference string) (string, error) {
	if !ValidRepository(repo) {
		return "", fmt.Errorf("delivery: %q is not a registry repository", repo)
	}
	host, path, _ := strings.Cut(repo, "/")
	scheme := "https"
	if r.Insecure {
		scheme = "http"
	}
	u := url.URL{Scheme: scheme, Host: host, Path: "/v2/" + path + "/manifests/" + reference}
	client := r.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", manifestTypes)
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: registry unreachable", ErrUnavailable)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return "", ErrImageMissing
	case resp.StatusCode != http.StatusOK:
		return "", fmt.Errorf("%w: registry answered HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	d := resp.Header.Get("Docker-Content-Digest")
	if !digestRE.MatchString(d) {
		return "", errors.New("delivery: the registry returned no manifest digest")
	}
	if strings.HasPrefix(reference, "sha256:") && d != reference {
		return "", fmt.Errorf("delivery: the registry returned digest %s for %s", d, reference)
	}
	return d, nil
}
