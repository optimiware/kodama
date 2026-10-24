package metricsauth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/metrics"
	metricsauth "github.com/optimiware/kodama/internal/metrics/auth"
	"github.com/optimiware/kodama/internal/work"
)

type authority struct {
	resolveErr, authErr error
	actor               work.Actor
	asked               []work.Permission
}

func (a *authority) ResolveActor(context.Context, domain.Scope, string) (work.Actor, error) {
	return a.actor, a.resolveErr
}

func (a *authority) Authorize(_ context.Context, _ work.Actor, _ domain.Scope, p work.Permission) error {
	a.asked = append(a.asked, p)
	return a.authErr
}

var scope = domain.Scope{TenantID: "t_a", ProjectID: "p_a"}

func Test_work_read_authorizer(t *testing.T) {
	user := work.Actor{ID: "u_1", Type: work.ActorUser}

	a := &authority{actor: user}
	auth, err := metricsauth.NewWorkReadAuthorizer(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.AuthorizeRead(t.Context(), scope, "u_1"); err != nil {
		t.Fatal(err)
	}
	if len(a.asked) != 1 || a.asked[0] != work.PermRead {
		t.Fatalf("asked %v, want exactly work_item:read", a.asked)
	}

	for name, c := range map[string]struct {
		a    *authority
		want error
	}{
		"denied":            {&authority{actor: user, authErr: work.ErrDenied}, metrics.ErrDenied},
		"hidden":            {&authority{actor: user, authErr: work.ErrHidden}, metrics.ErrHidden},
		"unknown principal": {&authority{resolveErr: work.ErrHidden}, metrics.ErrHidden},
		"outage":            {&authority{actor: user, authErr: errors.New("down")}, metrics.ErrUnavailable},
		"resolution outage": {&authority{resolveErr: errors.New("down")}, metrics.ErrUnavailable},
		"unusable actor":    {&authority{actor: work.Actor{ID: "u_1", Type: "robot"}}, metrics.ErrUnavailable},
	} {
		au, _ := metricsauth.NewWorkReadAuthorizer(c.a)
		if err := au.AuthorizeRead(t.Context(), scope, "u_1"); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}

	if _, err := metricsauth.NewWorkReadAuthorizer(nil); err == nil {
		t.Error("an authorizer without an authority was built")
	}
	if err := (metricsauth.WorkReadAuthorizer{}).AuthorizeRead(t.Context(), scope, "u_1"); !errors.Is(err, metrics.ErrUnavailable) {
		t.Errorf("zero authorizer: %v", err)
	}
}
