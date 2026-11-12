package nats_test

import (
	"strings"
	"testing"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/integration/nats"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestSubjectScopeCommandAndWildcardRefusal pins the scope-qualified subject
// grammar: exactly one scope per subject, no wildcard, no command-style suffix,
// and an injective project encoding.
func TestSubjectScopeCommandAndWildcardRefusal(t *testing.T) {
	project := testScope()
	tenant := tenancy.Scope{TenantID: project.TenantID}

	projectEvents, err := nats.EventSubject(project)
	if err != nil {
		t.Fatalf("EventSubject(project): %v", err)
	}
	if got, want := string(projectEvents), "kodama.v1.tenant-a.project-1.events"; got != want {
		t.Fatalf("project event subject = %q; want %q", got, want)
	}
	tenantEvents, err := nats.EventSubject(tenant)
	if err != nil {
		t.Fatalf("EventSubject(tenant): %v", err)
	}
	if got, want := string(tenantEvents), "kodama.v1.tenant-a._.events"; got != want {
		t.Fatalf("tenant event subject = %q; want %q", got, want)
	}
	if projectEvents == tenantEvents {
		t.Fatal("project and tenant-wide event subjects collide")
	}

	// The tenant-wide placeholder is not an encodable project id.
	placeholder := tenancy.Scope{TenantID: project.TenantID, ProjectID: nats.TenantWideProject}
	builders := []struct {
		name  string
		build func() (nats.Subject, error)
	}{
		{"event", func() (nats.Subject, error) { return nats.EventSubject(placeholder) }},
		{"invalidation", func() (nats.Subject, error) { return nats.InvalidationSubject(placeholder) }},
		{"log", func() (nats.Subject, error) {
			return nats.LogSubject(placeholder, id.ExecutionID("exec-1"))
		}},
		{"ingress", func() (nats.Subject, error) { return nats.IngressSubject(placeholder) }},
	}
	for _, builder := range builders {
		if _, err := builder.build(); err == nil {
			t.Errorf("%s builder accepted project id %q; want refusal so the encoding stays injective", builder.name, nats.TenantWideProject)
		}
	}

	// Command-style, wildcard, empty and extra tokens are not subjects.
	invalid := []nats.Subject{
		"",
		"kodama.v1.tenant-a.project-1.commands",
		"kodama.v1.tenant-a.project-1.events.start",
		"kodama.v1.tenant-a.project-1.events.>",
		"kodama.v1.tenant-a.project-1.events.*",
		"kodama.v1.tenant-a.*.events",
		"kodama.v1.tenant-a.>.events",
		"kodama.v1.tenant-a..events",
		"kodama.v1.tenant-a.project-1.invalidations.extra",
		"kodama.v1.tenant-a.project-1.ingress.start",
		"kodama.v2.tenant-a.project-1.events",
	}
	for _, subject := range invalid {
		if err := subject.Validate(); err == nil {
			t.Errorf("subject %q accepted; want refusal", subject)
		}
	}

	// A valid subject never contains a wildcard token.
	valid := []nats.Subject{projectEvents, tenantEvents}
	invalidation, err := nats.InvalidationSubject(project)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	liveLog, err := nats.LogSubject(project, id.ExecutionID("exec-1"))
	if err != nil {
		t.Fatalf("LogSubject: %v", err)
	}
	ingress, err := nats.IngressSubject(project)
	if err != nil {
		t.Fatalf("IngressSubject: %v", err)
	}
	valid = append(valid, invalidation, liveLog, ingress)
	for _, subject := range valid {
		if strings.ContainsAny(string(subject), "*>") {
			t.Errorf("valid subject %q contains a wildcard", subject)
		}
	}

	// ParseScope round-trips exactly one scope and never widens a project.
	parsed, err := nats.ParseScope(projectEvents)
	if err != nil {
		t.Fatalf("ParseScope(project): %v", err)
	}
	if parsed != project {
		t.Fatalf("ParseScope(project) = %+v; want %+v", parsed, project)
	}
	parsed, err = nats.ParseScope(tenantEvents)
	if err != nil {
		t.Fatalf("ParseScope(tenant): %v", err)
	}
	if parsed != tenant {
		t.Fatalf("ParseScope(tenant) = %+v; want %+v", parsed, tenant)
	}
	if parsed = mustParseScope(t, ingress); parsed != project {
		t.Fatalf("ParseScope(ingress) = %+v; want %+v", parsed, project)
	}
}

func mustParseScope(t *testing.T, subject nats.Subject) tenancy.Scope {
	t.Helper()
	scope, err := nats.ParseScope(subject)
	if err != nil {
		t.Fatalf("ParseScope(%q): %v", subject, err)
	}
	return scope
}

// TestIngressSubjectIsSeparateFromCommittedEventSubjects pins the dedicated
// authenticated observation-ingress subject: it is scope-qualified, validates,
// and never collides with the committed-event or invalidation subjects.
func TestIngressSubjectIsSeparateFromCommittedEventSubjects(t *testing.T) {
	scope := testScope()
	ingress, err := nats.IngressSubject(scope)
	if err != nil {
		t.Fatalf("IngressSubject: %v", err)
	}
	if got, want := string(ingress), "kodama.v1.tenant-a.project-1.ingress"; got != want {
		t.Fatalf("ingress subject = %q; want %q", got, want)
	}
	if err := ingress.Validate(); err != nil {
		t.Fatalf("ingress subject refused by Validate: %v", err)
	}
	events, err := nats.EventSubject(scope)
	if err != nil {
		t.Fatalf("EventSubject: %v", err)
	}
	invalidations, err := nats.InvalidationSubject(scope)
	if err != nil {
		t.Fatalf("InvalidationSubject: %v", err)
	}
	if ingress == events || ingress == invalidations {
		t.Fatalf("ingress subject %q collides with a committed-event subject", ingress)
	}

	tenantIngress, err := nats.IngressSubject(tenancy.Scope{TenantID: scope.TenantID})
	if err != nil {
		t.Fatalf("tenant IngressSubject: %v", err)
	}
	if got, want := string(tenantIngress), "kodama.v1.tenant-a._.ingress"; got != want {
		t.Fatalf("tenant ingress subject = %q; want %q", got, want)
	}
	if tenantIngress == ingress {
		t.Fatal("tenant-wide and project ingress subjects collide")
	}
	otherIngress, err := nats.IngressSubject(otherScope())
	if err != nil {
		t.Fatalf("other IngressSubject: %v", err)
	}
	if otherIngress == ingress {
		t.Fatal("ingress subjects from different scopes collide")
	}
}
