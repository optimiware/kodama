package authorize_test

import (
	"reflect"
	"testing"

	"github.com/optimiware/kodama/internal/fault"
	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/shell/authorize"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// commitReads is the authority-read set of one task.edit commit: a current
// membership fact and the expected task version.
func commitReads(t *testing.T) []authorize.AuthorityRead {
	t.Helper()
	required := operationNamed(t, "task.edit").Requires
	return []authorize.AuthorityRead{
		{
			Operation:   "task.edit",
			Required:    required,
			Check:       authorize.CheckMembership,
			Scope:       projectScope(),
			PrincipalID: testPrincipal,
		},
		{
			Operation:       "task.edit",
			Required:        required,
			Check:           authorize.CheckTaskVersion,
			Scope:           projectScope(),
			PrincipalID:     testPrincipal,
			TargetID:        testTask,
			ExpectedVersion: 5,
		},
	}
}

func commitFacts() []authorize.AuthorityFact {
	return []authorize.AuthorityFact{
		{
			Check:       authorize.CheckMembership,
			Scope:       projectScope(),
			PrincipalID: testPrincipal,
			Allowed:     true,
		},
		{
			Check:       authorize.CheckTaskVersion,
			Scope:       projectScope(),
			PrincipalID: testPrincipal,
			TargetID:    testTask,
			Version:     5,
			Allowed:     true,
		},
	}
}

// TestConfirmAcceptsCurrentAllowedFacts asserts one fact per read, in input
// order, with the expected version, is accepted.
func TestConfirmAcceptsCurrentAllowedFacts(t *testing.T) {
	if err := authorize.Confirm(commitReads(t), commitFacts()); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if err := authorize.Confirm(nil, nil); err != nil {
		t.Fatalf("Confirm with no reads: %v", err)
	}
}

// TestConfirmRefusesRevokedAuthority asserts a fact read as not allowed refuses
// the commit with the forbidden class even though the earlier grant allowed it.
func TestConfirmRefusesRevokedAuthority(t *testing.T) {
	facts := commitFacts()
	facts[0].Allowed = false
	err := authorize.Confirm(commitReads(t), facts)
	wantFaultClass(t, err, fault.Forbidden)

	facts = commitFacts()
	facts[1].Allowed = false
	err = authorize.Confirm(commitReads(t), facts)
	wantFaultClass(t, err, fault.Forbidden)
}

// TestConfirmRefusesStaleVersion asserts a versioned fact observed at a
// different version is a stable version conflict, not a silent allow.
func TestConfirmRefusesStaleVersion(t *testing.T) {
	for _, observed := range []uint64{4, 6, 0} {
		facts := commitFacts()
		facts[1].Version = observed
		err := authorize.Confirm(commitReads(t), facts)
		wantFaultClass(t, err, fault.VersionConflict)
	}
}

// TestConfirmRefusesFactsOutsideTheRead asserts the returned facts must answer
// exactly the requested reads: count, order, check, scope, principal and
// target all participate.
func TestConfirmRefusesFactsOutsideTheRead(t *testing.T) {
	cases := []struct {
		name   string
		mutate func([]authorize.AuthorityFact) []authorize.AuthorityFact
	}{
		{"missing fact", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact { return facts[:1] }},
		{"extra fact", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			return append(facts, facts[1])
		}},
		{"reordered facts", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			return []authorize.AuthorityFact{facts[1], facts[0]}
		}},
		{"wrong check", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			facts[0].Check = authorize.CheckTaskVersion
			return facts
		}},
		{"wrong scope", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			facts[0].Scope = otherTenantScope()
			return facts
		}},
		{"wrong project", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			facts[0].Scope = otherProjectScope()
			return facts
		}},
		{"wrong principal", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			facts[0].PrincipalID = id.PrincipalID("principal-2")
			return facts
		}},
		{"wrong target", func(facts []authorize.AuthorityFact) []authorize.AuthorityFact {
			facts[1].TargetID = "task-2"
			return facts
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := authorize.Confirm(commitReads(t), testCase.mutate(commitFacts()))
			wantFaultClass(t, err, fault.InternalFailure)
		})
	}
}

// TestConfirmPreservesReadContext asserts Confirm never mutates the caller's
// reads, so the commit unit keeps the operation/required context it was built
// with.
func TestConfirmPreservesReadContext(t *testing.T) {
	reads := commitReads(t)
	before := make([]authorize.AuthorityRead, len(reads))
	for index := range reads {
		before[index] = reads[index]
		before[index].Required.HumanRoles = append([]tenancy.Role(nil), reads[index].Required.HumanRoles...)
		before[index].Required.ServiceRoles = append([]tenancy.Role(nil), reads[index].Required.ServiceRoles...)
	}
	requireNoError(t, authorize.Confirm(reads, commitFacts()))
	if !reflect.DeepEqual(reads, before) {
		t.Fatalf("Confirm mutated the authority reads:\n got %#v\nwant %#v", reads, before)
	}
}
