package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
)

// admissionTestResolver never runs: these tests exercise the admission action
// loop directly and only declare request actions.
type admissionTestResolver struct{}

func (admissionTestResolver) Resolve(context.Context, ResolveRequest) (ResolvedImage, error) {
	return ResolvedImage{}, fault.New(fault.InternalFailure, "unexpected image resolution")
}

// TestAdmitActionsRefusesEmptyRequestDeclaration covers the defensive admission
// guards directly. Authoring already refuses an empty assignment and no subject,
// so a stored or inline YAML definition cannot reach this state today; the loop
// must still refuse a request declaration constructed without authoring.
func TestAdmitActionsRefusesEmptyRequestDeclaration(t *testing.T) {
	service := &AdmissionService{resolver: admissionTestResolver{}}
	subject := definition.NewSubject(map[string]definition.SubjectValue{
		"operation": {Kind: definition.SubjectLiteral, Literal: "git.ref.update"},
	})
	refusals := []struct {
		name    string
		request definition.Request
		message string
	}{
		{
			name:    "empty assignment",
			request: definition.Request{Assignment: "", Subject: subject},
			message: "request assignment is empty",
		},
		{
			name:    "no subject",
			request: definition.Request{Assignment: "role/reviewer", Subject: definition.NewSubject(nil)},
			message: "has no subject",
		},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			_, _, _, err := service.admitActions(context.Background(), definition.Pipeline{
				Actions: []definition.Action{{
					Name:    id.ActionName("review"),
					Request: &refusal.request,
				}},
			}, 5*time.Minute)
			requireFaultClassInternal(t, err, fault.InvalidInput)
			message, ok := fault.MessageOf(err)
			if !ok || !strings.Contains(message, `"review"`) || !strings.Contains(message, refusal.message) {
				t.Fatalf("message = %q; want it to name action %q and contain %q", message, "review", refusal.message)
			}
		})
	}
}

func requireFaultClassInternal(t *testing.T, err error, want fault.Class) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s fault, got nil", want)
	}
	got, ok := fault.ClassOf(err)
	if !ok {
		t.Fatalf("error %v is not a fault; want class %s", err, want)
	}
	if got != want {
		t.Fatalf("fault class = %s; want %s (%v)", got, want, err)
	}
}
