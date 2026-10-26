package app

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/artifacts"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/httpapi"
	"github.com/optimiware/kodama/internal/identity"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/secrets"
	"github.com/optimiware/kodama/internal/workspace"
)

// fakeAuthority answers identity questions from a function and records them.
type fakeAuthority struct {
	fn    func(actor identity.Actor, target identity.Target, perm identity.Permission) error
	asked []identity.Permission
}

func (f *fakeAuthority) Authorize(_ context.Context, actor identity.Actor, target identity.Target, perm identity.Permission) error {
	f.asked = append(f.asked, perm)
	if f.fn == nil {
		return nil
	}
	return f.fn(actor, target, perm)
}

var scopeA = domain.Scope{TenantID: "t_acme", ProjectID: "p_inv"}

func TestPipelineAuthorityMapsDefinitionPermissionsToRoles(t *testing.T) {
	ctx := context.Background()
	for perm, want := range map[pipeline.Permission]identity.Permission{
		pipeline.PermList: identity.PermRunRead, pipeline.PermRead: identity.PermRunRead, pipeline.PermRegister: identity.PermRunManage,
	} {
		f := &fakeAuthority{}
		a := pipelineAuthority{authority: f}
		if err := a.Authorize(ctx, pipeline.Actor{ID: "u_1", Type: pipeline.ActorUser}, scopeA, perm); err != nil {
			t.Fatal(err)
		}
		if len(f.asked) != 1 || f.asked[0] != want {
			t.Errorf("%s asked %v, want %s", perm, f.asked, want)
		}
	}
	f := &fakeAuthority{}
	if err := (pipelineAuthority{authority: f}).Authorize(ctx, pipeline.Actor{ID: "u", Type: pipeline.ActorUser}, scopeA, "pipeline_definition.delete"); !errors.Is(err, pipeline.ErrDenied) || len(f.asked) != 0 {
		t.Fatalf("an unknown permission must deny without asking: %v %v", err, f.asked)
	}
	for name, tc := range map[string]struct {
		in   error
		want error
	}{
		"hidden": {identity.ErrHidden, pipeline.ErrHidden}, "denied": {identity.ErrDenied, pipeline.ErrDenied}, "outage": {errors.New("db down"), pipeline.ErrUnavailable},
	} {
		a := pipelineAuthority{authority: &fakeAuthority{fn: func(identity.Actor, identity.Target, identity.Permission) error { return tc.in }}}
		if err := a.Authorize(ctx, pipeline.Actor{ID: "u", Type: pipeline.ActorUser}, scopeA, pipeline.PermRegister); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

type fakeCatalog struct{ err error }

func (c fakeCatalog) CurrentVersion(context.Context, domain.Scope, string) (int64, error) {
	return 3, c.err
}

func TestPipelineReferencesAdmitDeclaredFormsAndExistingSecrets(t *testing.T) {
	ctx := context.Background()
	actor := pipeline.Actor{ID: "u_1", Type: pipeline.ActorUser}
	ok := pipelineReferences{authority: &fakeAuthority{}, catalog: fakeCatalog{}}
	if err := ok.CheckSecret(ctx, scopeA, actor, "token", pipeline.SecretUseFile); err != nil {
		t.Fatal(err)
	}
	if err := ok.CheckSecret(ctx, scopeA, actor, "token", pipeline.SecretUseEnv); err != nil {
		t.Fatalf("an ENV binding is a declared form of delivery: %v", err)
	}
	if err := ok.CheckSecret(ctx, scopeA, actor, "token", pipeline.SecretUse("argv")); !errors.Is(err, pipeline.ErrReferenceDenied) {
		t.Fatalf("an unknown form of delivery must be refused: %v", err)
	}
	for name, tc := range map[string]struct {
		r    pipelineReferences
		want error
	}{
		"no credential:read": {pipelineReferences{authority: &fakeAuthority{fn: func(identity.Actor, identity.Target, identity.Permission) error { return identity.ErrDenied }}, catalog: fakeCatalog{}}, pipeline.ErrReferenceNotFound},
		"hidden project":     {pipelineReferences{authority: &fakeAuthority{fn: func(identity.Actor, identity.Target, identity.Permission) error { return identity.ErrHidden }}, catalog: fakeCatalog{}}, pipeline.ErrReferenceNotFound},
		"authority outage":   {pipelineReferences{authority: &fakeAuthority{fn: func(identity.Actor, identity.Target, identity.Permission) error { return errors.New("x") }}, catalog: fakeCatalog{}}, pipeline.ErrUnavailable},
		"absent secret":      {pipelineReferences{authority: &fakeAuthority{}, catalog: fakeCatalog{err: secrets.ErrNotFound}}, pipeline.ErrReferenceNotFound},
		"deleted secret":     {pipelineReferences{authority: &fakeAuthority{}, catalog: fakeCatalog{err: secrets.ErrDeleted}}, pipeline.ErrReferenceNotFound},
		"revoked secret":     {pipelineReferences{authority: &fakeAuthority{}, catalog: fakeCatalog{err: secrets.ErrNoActiveVersion}}, pipeline.ErrReferenceDenied},
		"catalog outage":     {pipelineReferences{authority: &fakeAuthority{}, catalog: fakeCatalog{err: errors.New("x")}}, pipeline.ErrUnavailable},
	} {
		if err := tc.r.CheckSecret(ctx, scopeA, actor, "token", pipeline.SecretUseFile); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := ok.CheckEnvironment(ctx, scopeA, actor, "env_prod"); !errors.Is(err, pipeline.ErrReferenceNotFound) {
		t.Fatalf("there are no environments in the core slice: %v", err)
	}
	if _, err := (noProfiles{}).LookupProfile(ctx, scopeA, "coding-v1"); !errors.Is(err, pipeline.ErrReferenceNotFound) {
		t.Fatal("no profile may resolve")
	}
}

func TestWorkspaceAuthorizer(t *testing.T) {
	ctx := context.Background()
	user := workspace.Actor{Type: workspace.ActorUser, ID: "u_1"}
	res := workspace.Resource{Kind: "workspace", ID: "ws_1"}
	f := &fakeAuthority{}
	a := workspaceAuthorizer{authority: f}
	for action, want := range map[workspace.Action]identity.Permission{
		workspace.ActionRead: identity.PermSourceRead, workspace.ActionProvision: identity.PermWorkspaceManage, workspace.ActionSync: identity.PermWorkspaceManage,
		workspace.ActionArchive: identity.PermWorkspaceManage, workspace.ActionRestore: identity.PermWorkspaceManage,
	} {
		f.asked = nil
		if err := a.Authorize(ctx, user, action, scopeA, res); err != nil || len(f.asked) != 1 || f.asked[0] != want {
			t.Errorf("%s: %v asked %v, want %s", action, err, f.asked, want)
		}
	}
	// Pinning, removal, fencing and writer admission are never human actions.
	for _, action := range []workspace.Action{workspace.ActionPin, workspace.ActionRemove, workspace.ActionFence, workspace.ActionAdmit, "workspace.unknown"} {
		f.asked = nil
		if err := a.Authorize(ctx, user, action, scopeA, res); !errors.Is(err, workspace.ErrDenied) || len(f.asked) != 0 {
			t.Errorf("a user performing %s: %v %v", action, err, f.asked)
		}
	}
	// Internal service actors hold exactly their documented actions and are never asked of identity.
	svc := func(id string) workspace.Actor { return workspace.Actor{Type: workspace.ActorService, ID: id} }
	for _, tc := range []struct {
		actor  workspace.Actor
		action workspace.Action
		ok     bool
	}{
		{svc("execution"), workspace.ActionSync, true}, {svc("execution"), workspace.ActionPin, true}, {svc("execution"), workspace.ActionAdmit, true},
		{svc("execution"), workspace.ActionProvision, false}, {svc("execution"), workspace.ActionArchive, false}, {svc("execution"), workspace.ActionRemove, false},
		{svc("workspace-reconciler"), workspace.ActionRemove, true}, {svc("workspace-reconciler"), workspace.ActionProvision, false},
		{svc("other"), workspace.ActionSync, false},
	} {
		err := a.Authorize(ctx, tc.actor, tc.action, scopeA, res)
		if (err == nil) != tc.ok {
			t.Errorf("%s %s: %v", tc.actor.ID, tc.action, err)
		}
	}
	for name, tc := range map[string]struct {
		in   error
		want error
	}{
		"hidden": {identity.ErrHidden, workspace.ErrNotFound}, "denied": {identity.ErrDenied, workspace.ErrDenied}, "outage": {errors.New("x"), workspace.ErrUnavailable},
	} {
		b := workspaceAuthorizer{authority: &fakeAuthority{fn: func(identity.Actor, identity.Target, identity.Permission) error { return tc.in }}}
		if err := b.Authorize(ctx, user, workspace.ActionProvision, scopeA, res); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestGitCredentialPrefixBoundary(t *testing.T) {
	for name, tc := range map[string]struct {
		prefix, url string
		ok          bool
	}{
		"host without slash permits subpath":   {"https://git.example", "https://git.example/acme/app.git", true},
		"host with slash permits subpath":      {"https://git.example/", "https://git.example/acme/app.git", true},
		"lookalike foreign host":               {"https://git.example", "https://git.example.evil/repo.git", false},
		"lookalike foreign host with slash":    {"https://git.example/", "https://git.example.evil/repo.git", false},
		"host suffix lookalike":                {"https://git.example", "https://evilgit.example/repo.git", false},
		"different scheme":                     {"https://git.example", "http://git.example/repo.git", false},
		"default port is not another port":     {"https://git.example", "https://git.example:8443/repo.git", false},
		"configured port exact":                {"https://git.example:8443", "https://git.example:8443/repo.git", true},
		"configured port not default":          {"https://git.example:8443", "https://git.example/repo.git", false},
		"configured port lookalike":            {"https://git.example:8443", "https://git.example:84430/repo.git", false},
		"path exact":                           {"https://git.example/repo", "https://git.example/repo", true},
		"path subpath":                         {"https://git.example/repo", "https://git.example/repo/x.git", true},
		"path string-prefix lookalike":         {"https://git.example/repo", "https://git.example/repository/x.git", false},
		"path with slash lookalike":            {"https://git.example/repo/", "https://git.example/repository", false},
		"userinfo in repository":               {"https://git.example", "https://user@git.example/repo.git", false},
		"userinfo trick":                       {"https://git.example", "https://git.example@git.example.evil/repo.git", false},
		"userinfo in prefix":                   {"https://u@git.example", "https://git.example/repo.git", false},
		"query in repository":                  {"https://git.example", "https://git.example/repo.git?x=1", false},
		"non-https prefix":                     {"http://git.example", "http://git.example/repo.git", false},
		"invalid repository":                   {"https://git.example", "https://git.example/%zz", false},
		"invalid prefix":                       {"https://git.exa mple", "https://git.example/repo.git", false},
		"empty":                                {"", "", false},
		"host case is insignificant":           {"https://Git.Example", "https://git.example/repo.git", true},
		"encoded separator does not escape":    {"https://git.example/repo", "https://git.example/repo%2F..%2Fother", false},
		"encoded lookalike":                    {"https://git.example/repo", "https://git.example/repo%2Fx", false},
		"dot segment":                          {"https://git.example/repo", "https://git.example/repo/../other", false},
		"missing host":                         {"https://git.example", "https:///repo.git", false},
		"scheme-relative":                      {"https://git.example", "//git.example/repo.git", false},
		"opaque":                               {"https://git.example", "https:git.example/repo.git", false},
		"same host different prefix path case": {"https://git.example/Repo", "https://git.example/repo/x.git", false},
	} {
		if got := urlWithinPrefix(tc.url, tc.prefix); got != tc.ok {
			t.Errorf("%s: urlWithinPrefix(%q, %q) = %v", name, tc.url, tc.prefix, got)
		}
	}
}

func TestGitCredentialsAreBoundToHostsAndTenants(t *testing.T) {
	creds := indexCredentials([]GitCredential{
		{Ref: "secret://installation/checkout", Username: "u", Password: "p", URLPrefix: "https://git.example/", Tenants: []string{"t_acme"}},
		{Ref: "secret://installation/shared", Username: "u", Password: "q", URLPrefix: "https://git.example/shared/", Tenants: []string{"*"}},
	})
	repos := projectRepositories{creds: creds}
	for name, tc := range map[string]struct {
		tenant domain.TenantID
		ref    string
		url    string
		ok     bool
	}{
		"own tenant and host":    {"t_acme", "secret://installation/checkout", "https://git.example/acme/app.git", true},
		"another tenant":         {"t_globex", "secret://installation/checkout", "https://git.example/acme/app.git", false},
		"another host":           {"t_acme", "secret://installation/checkout", "https://evil.example/acme/app.git", false},
		"prefix is not a host":   {"t_acme", "secret://installation/checkout", "https://git.example.evil/x.git", false},
		"unknown reference":      {"t_acme", "secret://installation/none", "https://git.example/x.git", false},
		"wildcard tenant":        {"t_globex", "secret://installation/shared", "https://git.example/shared/x.git", true},
		"wildcard on other path": {"t_globex", "secret://installation/shared", "https://git.example/private/x.git", false},
		"shared path itself":     {"t_globex", "secret://installation/shared", "https://git.example/shared", true},
		"path prefix lookalike":  {"t_globex", "secret://installation/shared", "https://git.example/shared-evil/x.git", false},
		"different scheme":       {"t_acme", "secret://installation/checkout", "http://git.example/x.git", false},
		"different port":         {"t_acme", "secret://installation/checkout", "https://git.example:8443/x.git", false},
		"userinfo":               {"t_acme", "secret://installation/checkout", "https://git.example@evil.example/x.git", false},
		"userinfo same host":     {"t_acme", "secret://installation/checkout", "https://u:p@git.example/x.git", false},
		"dot segments":           {"t_globex", "secret://installation/shared", "https://git.example/shared/../private/x.git", false},
		"invalid url":            {"t_acme", "secret://installation/checkout", "https://git.example/%zz", false},
		"not a url":              {"t_acme", "secret://installation/checkout", "::", false},
	} {
		if got := repos.permits(tc.tenant, tc.ref, tc.url); got != tc.ok {
			t.Errorf("%s: %v", name, got)
		}
	}
	r := installationCredentials{creds: creds}
	if c, err := r.Resolve(context.Background(), domain.Scope{TenantID: "t_acme", ProjectID: "p_a"}, "secret://installation/checkout"); err != nil || c.Password != "p" {
		t.Fatalf("%v %v", c, err)
	}
	if _, err := r.Resolve(context.Background(), domain.Scope{TenantID: "t_globex", ProjectID: "p_a"}, "secret://installation/checkout"); err == nil {
		t.Fatal("another tenant resolved the credential")
	}
	if _, err := r.Resolve(context.Background(), scopeA, "secret://project/x"); err == nil {
		t.Fatal("an unconfigured reference resolved")
	}
}

func TestArtifactReadAuthority(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		in   error
		want error
	}{
		"allowed": {nil, nil}, "hidden": {identity.ErrHidden, artifacts.ErrNotFound}, "denied": {identity.ErrDenied, artifacts.ErrForbidden},
	} {
		a := artifactReadAuthority{authority: &fakeAuthority{fn: func(_ identity.Actor, _ identity.Target, perm identity.Permission) error {
			if perm != identity.PermEvidenceRead {
				t.Errorf("%s asked %s", name, perm)
			}
			return tc.in
		}}}
		err := a.AuthorizeRead(ctx, artifacts.Caller{Subject: "u_1"}, scopeA, artifacts.ReadSubject{})
		if !errors.Is(err, tc.want) && !(tc.want == nil && err == nil) {
			t.Errorf("%s: %v", name, err)
		}
	}
	out := errors.New("outage")
	if err := (artifactReadAuthority{authority: &fakeAuthority{fn: func(identity.Actor, identity.Target, identity.Permission) error { return out }}}).AuthorizeRead(ctx, artifacts.Caller{Subject: "u"}, scopeA, artifacts.ReadSubject{}); !errors.Is(err, out) {
		t.Fatalf("an outage must be returned, never access: %v", err)
	}
}

func TestUnboundSeamsFailClosed(t *testing.T) {
	ctx := context.Background()
	if _, err := (deniedRegistry{}).Verify(ctx, artifacts.OCIRequest{}); !errors.Is(err, artifacts.ErrRegistryDenied) {
		t.Fatalf("no registry content may verify: %v", err)
	}
	if _, err := (attemptNeedles{}).Needles(ctx, scopeA, "att_1"); !errors.Is(err, workspace.ErrUnavailable) {
		t.Fatalf("source scanning without an execution store must fail closed: %v", err)
	}
}

func TestCoreRulesCoverEveryRegisteredAction(t *testing.T) {
	rules := coreRules()
	for _, a := range []string{
		"identity.me", "membership.create", "project.create", "work_item.create", "variable.create", "secret.create",
		"run.create", "run.list", "run.read", "run.steps", "run.cancel",
		"pipeline_definition.list", "pipeline_definition.register", "pipeline_definition.read",
		"workspace.provision", "workspace.read", "workspace.sync", "workspace.archive", "workspace.restore",
		"artifact.read", "observation.project.events", "observation.run.events",
		// T-072: profiles, gates, environments and deployment records.
		"agent_profile.list", "agent_profile.register", "gate.list", "gate.read", "gate.decide",
		"environment.list", "environment.read", "environment.create", "environment.update",
		"deployment.list", "deployment.read", "deployment.create", "deployment.cleanup",
		// T-039: project model settings and MCP registrations.
		"model_settings.read", "model_settings.update", "mcp_server.list", "mcp_server.mutate",
	} {
		if _, ok := rules[httpapiAction(a)]; !ok {
			t.Errorf("action %s has no rule: its route would deny every request", a)
		}
	}
	// No rule exists for capabilities that are not routed through the public
	// router: Git webhooks (signature-authenticated, edge-throttled), ChatOps and
	// the runtime deployment callbacks (they are served on the runtime API and
	// decided by their own service).
	for _, a := range []string{"log.read", "agent_profile.create",
		"deployment.runtime.register", "deployment.runtime.outcome", "git_hook.receive"} {
		if _, ok := rules[httpapiAction(a)]; ok {
			t.Errorf("action %s has a rule though its module is not part of the assembly", a)
		}
	}
}

func httpapiAction(s string) httpapi.Action { return httpapi.Action(s) }
