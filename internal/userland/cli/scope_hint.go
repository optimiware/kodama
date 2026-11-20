package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/optimiware/kodema/api"
	"github.com/optimiware/kodema/client"
)

// hintedError attaches a client-side stderr hint to an error while preserving
// its error text and unwrap chain for fault/api classification.
type hintedError struct {
	err  error
	hint string
}

func (e *hintedError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *hintedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

// errorHint returns the client-side hint attached to err, if any.
func errorHint(err error) string {
	if hinted, ok := errors.AsType[*hintedError](err); ok {
		return hinted.hint
	}
	return ""
}

// projectScopeHintTransport wraps a client.Transport to attach an actionable
// client-side hint when a project-scoped JWT credential hits a uniform
// "not_found: project is not visible" error for a different target project.
// The hint is derived exclusively from local JWT claims and request inputs and
// never reveals server-side existence of foreign projects.
type projectScopeHintTransport struct {
	inner  client.Transport
	config Config
	ports  Ports
	deps   CommandDeps
}

var _ client.EventStreamer = projectScopeHintTransport{}

func (t projectScopeHintTransport) Invoke(ctx context.Context, invocation api.Invocation) (api.Result, error) {
	result, err := t.inner.Invoke(ctx, invocation)
	if err == nil {
		return result, nil
	}
	return result, annotateProjectScopeHint(ctx, err, invocation, t.config, t.ports, t.deps)
}

func (t projectScopeHintTransport) StreamEvents(ctx context.Context, request client.EventStreamRequest) (client.EventStream, error) {
	streamer, ok := t.inner.(client.EventStreamer)
	if !ok {
		return nil, fmt.Errorf("the configured client does not support live event streaming")
	}
	stream, err := streamer.StreamEvents(ctx, request)
	if err == nil {
		return stream, nil
	}
	input, _ := json.Marshal(map[string]string{"project_id": request.ProjectID})
	synthetic := api.Invocation{
		Operation: "event.subscribe",
		Request:   api.Request{Input: input},
	}
	return nil, annotateProjectScopeHint(ctx, err, synthetic, t.config, t.ports, t.deps)
}

func annotateProjectScopeHint(ctx context.Context, err error, invocation api.Invocation, config Config, ports Ports, deps CommandDeps) error {
	if !isProjectNotVisibleError(err) {
		return err
	}
	token := resolvePresentedToken(ctx, config, ports, deps)
	if token == "" {
		return err
	}
	tokenProject, ok := parseJWTProjectID(token)
	if !ok {
		return err
	}
	targetProject := invocationTargetProject(invocation, config.Project)
	if targetProject != "" && tokenProject == targetProject {
		return err
	}
	return &hintedError{
		err:  err,
		hint: projectScopeMismatchHint(tokenProject, targetProject),
	}
}

func isProjectNotVisibleError(err error) bool {
	if err == nil || !isNotFoundError(err) {
		return false
	}
	return strings.Contains(strings.ToLower(publicMessage(err)), "project is not visible")
}

// resolvePresentedToken returns the bearer token from the active local
// credential source without logging or exposing it.
func resolvePresentedToken(ctx context.Context, config Config, ports Ports, deps CommandDeps) string {
	if ports.Credential != nil {
		if cred, err := ports.Credential.Credential(ctx); err == nil {
			if token := strings.TrimSpace(cred.Token); token != "" {
				return token
			}
		}
	}
	if config.CredentialFile != "" {
		if ports.Files != nil {
			if data, err := ports.Files.ReadFile(config.CredentialFile); err == nil {
				if token := strings.TrimSpace(string(data)); token != "" {
					return token
				}
			}
		}
		// Never fall through to a saved human session when an explicit
		// credential file was configured.
		return ""
	}
	if deps.Login.Store != nil {
		if record, err := deps.Login.Store.LoadRecord(ctx); err == nil {
			if token := strings.TrimSpace(record.Token); token != "" {
				return token
			}
		}
	}
	if ports.Session != nil {
		if cred, err := ports.Session.Load(ctx); err == nil {
			if token := strings.TrimSpace(cred.Token); token != "" {
				return token
			}
		}
	}
	return ""
}

// parseJWTProjectID extracts a non-empty string project_id claim from the
// payload of a compact JWT without verifying signatures. Non-JWT tokens and
// JWTs without a string project_id claim return ("", false).
func parseJWTProjectID(token string) (string, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return "", false
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", false
	}
	projectID, ok := claims["project_id"].(string)
	if !ok {
		return "", false
	}
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return "", false
	}
	return projectID, true
}

func invocationTargetProject(invocation api.Invocation, fallbackConfigProject string) string {
	if len(invocation.Request.Input) > 0 {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(invocation.Request.Input, &fields); err == nil {
			if raw, ok := fields["project_id"]; ok {
				var projectID string
				if err := json.Unmarshal(raw, &projectID); err == nil {
					if trimmed := strings.TrimSpace(projectID); trimmed != "" {
						return trimmed
					}
				}
			}
		}
	}
	return strings.TrimSpace(fallbackConfigProject)
}

func projectScopeMismatchHint(tokenProject, targetProject string) string {
	if targetProject != "" {
		return fmt.Sprintf(
			"the active credential is scoped to project %q (request targeted %q); use a credential for that project or a tenant-scoped credential (--credential-file, KODEMA_CREDENTIAL_FILE, or KODEMA_TENANT_ADMIN_CREDENTIAL_FILE) or run 'kodema login'",
			tokenProject, targetProject,
		)
	}
	return fmt.Sprintf(
		"the active credential is scoped to project %q; use a credential for the target project or a tenant-scoped credential (--credential-file, KODEMA_CREDENTIAL_FILE, or KODEMA_TENANT_ADMIN_CREDENTIAL_FILE) or run 'kodema login'",
		tokenProject,
	)
}
