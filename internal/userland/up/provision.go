package up

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
	"github.com/optimiware/kodema/internal/fault"
	"github.com/optimiware/kodema/internal/userland/principal"
)

const (
	gitServiceID = "git"
	// gitCredentialTTL is the longest lifetime a service credential may have.
	gitCredentialTTL = 8 * time.Hour
)

// provisionConfig names what a bootstrapped tenant still needs before it can
// run work: its project, the project roles of the built-in personas and the
// Git service account.
type provisionConfig struct {
	PublicURL string
	RootCAs   *x509.CertPool
	Issuer    string
	TenantID  string
	ProjectID string
	// TenantToken administers the tenant (project, memberships); ProjectToken
	// administers the project (services, bindings).
	TenantToken  string
	ProjectToken string
	// GitOrigins are the origins clients and workloads reach Git on.
	GitOrigins []string
}

type bearerToken string

func (token bearerToken) Credential(context.Context) (client.Credential, error) {
	return client.Credential{Kind: client.KindSession, Token: string(token), ExpiresAt: time.Now().Add(time.Hour)}, nil
}

// provision waits for the server, then creates what is missing. It is safe to
// repeat: what already exists is kept, and the Git service credential is
// renewed on every call. It returns that credential.
func provision(ctx context.Context, cfg provisionConfig) (string, error) {
	roundTripper := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: cfg.RootCAs, MinVersion: tls.VersionTLS12}}
	defer roundTripper.CloseIdleConnections()
	call := func(token, operation, key string, expected uint64, input any) (json.RawMessage, error) {
		encoded, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		request := api.Request{Input: encoded, IdempotencyKey: key}
		if expected > 0 {
			request.ExpectedVersion = &expected
		}
		transport := &client.HTTPTransport{
			BaseURL:      cfg.PublicURL,
			RoundTripper: roundTripper,
			Source:       bearerToken(token),
			Retry:        client.RetryPolicy{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: time.Second},
		}
		result, err := transport.Invoke(ctx, api.Invocation{Operation: operation, Request: request})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", operation, err)
		}
		return result.Body, nil
	}
	ready := false
	for deadline := time.Now().Add(90 * time.Second); !ready && time.Now().Before(deadline) && ctx.Err() == nil; {
		probe, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.PublicURL+"/readyz", nil)
		if err != nil {
			return "", err
		}
		if response, err := (&http.Client{Transport: roundTripper, Timeout: 2 * time.Second}).Do(probe); err == nil {
			_ = response.Body.Close()
			ready = response.StatusCode == http.StatusOK
		}
		if !ready {
			time.Sleep(500 * time.Millisecond)
		}
	}
	if !ready {
		return "", fmt.Errorf("the server was not ready within 90s")
	}

	// ensure runs a create that a repeated `kodema up` finds already done.
	ensure := func(token, operation, key string, input any) error {
		_, err := call(token, operation, key, 0, input)
		if class, _ := fault.ClassOf(err); class == fault.VersionConflict {
			return nil
		}
		return err
	}

	if err := ensure(cfg.TenantToken, "project.create", provisionIdempotencyKey("kodema-up-project", cfg.ProjectID), api.ProjectCreateRequest{
		TenantID: cfg.TenantID, ProjectID: cfg.ProjectID, Name: cfg.ProjectID,
	}); err != nil {
		return "", err
	}
	for persona, roles := range map[string][]string{
		"admin":    {"engineer", "reviewer", "operator"},
		"engineer": {"engineer"},
		"reviewer": {"reviewer"},
		"operator": {"operator"},
	} {
		principalID, err := principal.ForOIDC(cfg.Issuer, persona)
		if err != nil {
			return "", err
		}
		if err := ensure(cfg.TenantToken, "membership.grant", provisionIdempotencyKey("kodema-up-member-"+persona, cfg.ProjectID), api.MembershipGrantRequest{
			ProjectID: cfg.ProjectID, PrincipalID: string(principalID), Roles: roles,
		}); err != nil {
			return "", err
		}
	}

	if err := ensure(cfg.ProjectToken, "service.create", provisionIdempotencyKey("kodema-up-service-git", cfg.ProjectID), api.ServiceCreateRequest{
		ProjectID: cfg.ProjectID, ServiceID: gitServiceID, Roles: []string{"viewer"},
	}); err != nil {
		return "", err
	}
	if err := ensure(cfg.ProjectToken, "resource.binding.register", provisionIdempotencyKey("kodema-up-binding-git-admin", cfg.ProjectID), api.ResourceBindingRegisterRequest{
		ProjectID:        cfg.ProjectID,
		BindingID:        "git-admin",
		ResourceID:       "git-admin",
		EnforcingService: gitServiceID,
		TrustedOrigins:   cfg.GitOrigins,
		Operations:       []string{"git.read", "git.ref.update"},
		ReportableEvents: []string{"git.ref.updated"},
	}); err != nil {
		return "", err
	}

	listed, err := call(cfg.ProjectToken, "service.list", "", 0, api.ServiceListRequest{ProjectID: cfg.ProjectID})
	if err != nil {
		return "", err
	}
	var services api.ServiceListResult
	if err := json.Unmarshal(listed, &services); err != nil {
		return "", fmt.Errorf("service.list: %w", err)
	}
	var version uint64
	for _, service := range services.Services {
		if service.ServiceID == gitServiceID {
			version = service.Version
		}
	}
	// A service has a credential to rotate on every run but the first.
	credential := api.ServiceCredentialRotateRequest{ServiceID: gitServiceID, TTLSeconds: int(gitCredentialTTL / time.Second)}
	key := fmt.Sprintf("kodema-up-git-credential-%d", time.Now().UnixNano())
	body, err := call(cfg.ProjectToken, "service.credential.rotate", key, version, credential)
	if err != nil {
		body, err = call(cfg.ProjectToken, "service.credential.issue", key+"-issue", version, credential)
	}
	if err != nil {
		return "", err
	}
	var issued api.ServiceCredentialIssueResult
	if err := json.Unmarshal(body, &issued); err != nil || issued.Token == nil {
		return "", fmt.Errorf("the Git service credential was not returned")
	}
	return *issued.Token, nil
}

// provisionIdempotencyKey scopes one bootstrap idempotency key to projectID
// while keeping the key within api.MaxIdempotencyKeyBytes.
func provisionIdempotencyKey(prefix, projectID string) string {
	key := prefix + "-" + projectID
	if len(key) <= api.MaxIdempotencyKeyBytes {
		return key
	}
	sum := sha256.Sum256([]byte(projectID))
	return prefix + "-" + hex.EncodeToString(sum[:16])
}
