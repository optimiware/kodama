package task_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/shell/identity"
	"github.com/optimiware/kodama/internal/shell/task"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// testEnv wires one Service to deterministic fakes. Tests mutate the fakes
// before calling the Service; nothing here reaches a clock, network or store.
type testEnv struct {
	service    *task.Service
	config     task.ServiceConfig
	ids        *sequentialIDs
	authorizer *fakeAuthorizer
	authority  *fakeAuthority
	principals *fakePrincipals
	pipelines  *fakePipelineReader
	reader     *fakeReader
	executions *fakeExecutionReader
	aggregates *fakeExecutionReader
	admission  *fakeAdmission
	ports      *fakePorts
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	env := &testEnv{
		ids:        &sequentialIDs{},
		authorizer: &fakeAuthorizer{},
		authority:  &fakeAuthority{},
		principals: &fakePrincipals{},
		pipelines:  &fakePipelineReader{},
		reader:     &fakeReader{},
		executions: &fakeExecutionReader{},
		admission:  &fakeAdmission{},
		ports:      &fakePorts{},
	}
	// The aggregate reader and the predecessor view reader share one fake so a
	// fixture seeds both from one execution.
	env.aggregates = env.executions
	env.principals.put(identity.Principal{
		ID:      engineerPrincipal,
		Kind:    identity.KindHuman,
		Scope:   projectScope(),
		Roles:   []tenancy.Role{tenancy.RoleEngineer},
		Enabled: true,
		Version: 1,
	})
	env.principals.put(identity.Principal{
		ID:      reviewerPrincipal,
		Kind:    identity.KindHuman,
		Scope:   projectScope(),
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	})
	env.principals.put(identity.Principal{
		ID:      operatorPrincipal,
		Kind:    identity.KindHuman,
		Scope:   projectScope(),
		Roles:   []tenancy.Role{tenancy.RoleOperator},
		Enabled: true,
		Version: 1,
	})
	env.principals.put(identity.Principal{
		ID:      viewerPrincipal,
		Kind:    identity.KindHuman,
		Scope:   projectScope(),
		Roles:   []tenancy.Role{tenancy.RoleViewer},
		Enabled: true,
		Version: 1,
	})
	env.principals.put(identity.Principal{
		ID:      serviceAPrincipal,
		Kind:    identity.KindService,
		Scope:   projectScope(),
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	})
	env.principals.put(identity.Principal{
		ID:      serviceBPrincipal,
		Kind:    identity.KindService,
		Scope:   projectScope(),
		Roles:   []tenancy.Role{tenancy.RoleReviewer},
		Enabled: true,
		Version: 1,
	})
	env.config = task.ServiceConfig{
		Clock:          fixedClock{now: testNow},
		IDs:            env.ids,
		Authorizer:     env.authorizer,
		Authority:      env.authority,
		Principals:     env.principals,
		Pipelines:      env.pipelines,
		Reader:         env.reader,
		Executions:     env.executions,
		Aggregates:     env.aggregates,
		Admission:      env.admission,
		Creates:        env.ports,
		Edits:          env.ports,
		Starts:         env.ports,
		Asks:           env.ports,
		Replies:        env.ports,
		Cancels:        env.ports,
		Progress:       env.ports,
		DefaultTimeout: 5 * time.Minute,
	}
	service, err := task.NewService(env.config)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	env.service = service
	return env
}

func TestNewServiceRefusesMissingDependency(t *testing.T) {
	base := task.ServiceConfig{
		Clock:          fixedClock{now: testNow},
		IDs:            &sequentialIDs{},
		Authorizer:     &fakeAuthorizer{},
		Authority:      &fakeAuthority{},
		Principals:     &fakePrincipals{},
		Pipelines:      &fakePipelineReader{},
		Reader:         &fakeReader{},
		Executions:     &fakeExecutionReader{},
		Aggregates:     &fakeExecutionReader{},
		Admission:      &fakeAdmission{},
		Creates:        &fakePorts{},
		Edits:          &fakePorts{},
		Starts:         &fakePorts{},
		Asks:           &fakePorts{},
		Replies:        &fakePorts{},
		Cancels:        &fakePorts{},
		Progress:       &fakePorts{},
		DefaultTimeout: 5 * time.Minute,
	}
	if _, err := task.NewService(base); err != nil {
		t.Fatalf("NewService complete config: %v", err)
	}
	cases := map[string]func(*task.ServiceConfig){
		"clock":      func(config *task.ServiceConfig) { config.Clock = nil },
		"ids":        func(config *task.ServiceConfig) { config.IDs = nil },
		"authorizer": func(config *task.ServiceConfig) { config.Authorizer = nil },
		"authority":  func(config *task.ServiceConfig) { config.Authority = nil },
		"principals": func(config *task.ServiceConfig) { config.Principals = nil },
		"pipelines":  func(config *task.ServiceConfig) { config.Pipelines = nil },
		"reader":     func(config *task.ServiceConfig) { config.Reader = nil },
		"executions": func(config *task.ServiceConfig) { config.Executions = nil },
		"aggregates": func(config *task.ServiceConfig) { config.Aggregates = nil },
		"admission":  func(config *task.ServiceConfig) { config.Admission = nil },
		"creates":    func(config *task.ServiceConfig) { config.Creates = nil },
		"edits":      func(config *task.ServiceConfig) { config.Edits = nil },
		"starts":     func(config *task.ServiceConfig) { config.Starts = nil },
		"asks":       func(config *task.ServiceConfig) { config.Asks = nil },
		"replies":    func(config *task.ServiceConfig) { config.Replies = nil },
		"cancels":    func(config *task.ServiceConfig) { config.Cancels = nil },
		"progress":   func(config *task.ServiceConfig) { config.Progress = nil },
		"timeout":    func(config *task.ServiceConfig) { config.DefaultTimeout = 0 },
	}
	for name, mutate := range cases {
		config := base
		mutate(&config)
		if _, err := task.NewService(config); err == nil {
			t.Fatalf("NewService accepted a missing %s dependency", name)
		} else if class, ok := fault.ClassOf(err); !ok || class != fault.InternalFailure {
			t.Fatalf("NewService missing %s fault = %v; want internal_failure", name, err)
		}
	}
}
