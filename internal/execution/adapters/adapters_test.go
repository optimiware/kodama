package adapters

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

func goodLimit() execution.Limit {
	return execution.Limit{TenantConcurrency: 4, ProjectConcurrency: 2, StartupBudget: time.Minute, MaxOutputBytes: 1, MaxOutputFiles: 1, MaxDiagnosticBytes: 1,
		DefaultTimeout: time.Minute, MaxTimeout: time.Hour}
}

// Static limits are explicit and validated: an unconfigured or invalid scope is
// unavailable, never an implicit default.
func Test_static_limits(t *testing.T) {
	scope := domain.Scope{TenantID: "t_a", ProjectID: "p_a"}
	big := goodLimit()
	big.TenantConcurrency = 9
	l := StaticLimits{Default: goodLimit(), PerTenant: map[domain.TenantID]execution.Limit{"t_big": big}}
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	if got, err := l.Limits(context.Background(), scope); err != nil || got.TenantConcurrency != 4 {
		t.Fatalf("%v %v", got, err)
	}
	if got, _ := l.Limits(context.Background(), domain.Scope{TenantID: "t_big", ProjectID: "p"}); got.TenantConcurrency != 9 {
		t.Fatalf("override %d", got.TenantConcurrency)
	}
	for name, mutate := range map[string]func(*execution.Limit){
		"zero tenant concurrency": func(l *execution.Limit) { l.TenantConcurrency = 0 },
		"zero project":            func(l *execution.Limit) { l.ProjectConcurrency = 0 },
		"tiny startup budget":     func(l *execution.Limit) { l.StartupBudget = time.Millisecond },
		"default over max":        func(l *execution.Limit) { l.DefaultTimeout = 2 * time.Hour },
		"no output bound":         func(l *execution.Limit) { l.MaxOutputBytes = 0 },
	} {
		bad := goodLimit()
		mutate(&bad)
		if (StaticLimits{Default: bad}).Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := (StaticLimits{Default: bad}).Limits(context.Background(), scope); !errors.Is(err, execution.ErrUnavailable) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := (StaticLimits{}).Limits(context.Background(), scope); !errors.Is(err, execution.ErrUnavailable) {
		t.Errorf("the zero value must deny dispatch: %v", err)
	}
}

func Test_adapter_constructors_fail_closed(t *testing.T) {
	if _, err := NewAuthority(nil, nil); err == nil {
		t.Error("authority without collaborators")
	}
	if _, err := NewDefinitions(nil); err == nil {
		t.Error("definitions without a repository")
	}
	if _, err := NewConfiguration(nil); err == nil {
		t.Error("configuration without a resolver")
	}
	if _, err := NewWorkContent(nil); err == nil {
		t.Error("work content without a repository")
	}
	if _, err := NewCancelledWork(nil); err == nil {
		t.Error("cancelled work without a source")
	}
	if _, err := NewCancellationRequester(nil); err == nil {
		t.Error("requester without a service")
	}
	if _, err := NewSources(nil, nil, nil); err == nil {
		t.Error("sources without collaborators")
	}
}
