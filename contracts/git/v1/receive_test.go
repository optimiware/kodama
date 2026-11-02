package gitv1

import (
	"strings"
	"testing"
)

// A receive ID carries the grant it was made under, so a receive whose outcome
// was lost can be attributed from the worker's journal alone.
func Test_unit_git_contract_receive_id_carries_grant(t *testing.T) {
	for _, grant := range []string{"gg_01", "gg_0123456789abcdef01234567", "g"} {
		id := NewReceiveID(grant)
		if err := checkID("receiveId", id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
		got, ok := GrantOfReceive(id)
		if !ok || got != grant {
			t.Errorf("%q: grant %q, %v", id, got, ok)
		}
		if other := NewReceiveID(grant); other == id {
			t.Errorf("two receives share the ID %q", id)
		}
	}
	// A grant that is not an ID, or too long for the identifier bound, names no grant.
	for name, grant := range map[string]string{"empty": "", "separator": "gg/1", "too long": strings.Repeat("g", 120)} {
		id := NewReceiveID(grant)
		if checkID("receiveId", id) != nil {
			t.Errorf("%s: %q is not an ID", name, id)
		}
		if g, ok := GrantOfReceive(id); ok {
			t.Errorf("%s: a receive ID carries grant %q", name, g)
		}
	}
	for name, id := range map[string]string{
		"no prefix":    "x_00000000000000000000000000000000_gg_1",
		"short random": "rcv_0000_gg_1",
		"upper hex":    "rcv_0000000000000000000000000000000A_gg_1",
		"no grant":     "rcv_00000000000000000000000000000000_",
		"bad grant":    "rcv_00000000000000000000000000000000_-x",
		"plain":        "rcv_8f2c",
	} {
		if g, ok := GrantOfReceive(id); ok {
			t.Errorf("%s: %q carries grant %q", name, id, g)
		}
	}
}

func Test_unit_git_contract_outcome_sequence_and_assignment_ack(t *testing.T) {
	seq := func(n int64) *int64 { return &n }
	for name, c := range map[string]struct {
		req OutcomeRequest
		ok  bool
	}{
		"none":      {OutcomeRequest{ReceiveID: "rcv_1", Applied: []RefUpdate{}}, true},
		"one":       {OutcomeRequest{ReceiveID: "rcv_1", Applied: []RefUpdate{}, JournalSequence: seq(1)}, true},
		"zero":      {OutcomeRequest{ReceiveID: "rcv_1", Applied: []RefUpdate{}, JournalSequence: seq(0)}, false},
		"too large": {OutcomeRequest{ReceiveID: "rcv_1", Applied: []RefUpdate{}, JournalSequence: seq(MaxSafeInteger + 1)}, false},
	} {
		if err := c.req.Validate(); (err == nil) != c.ok {
			t.Errorf("outcome %s: %v", name, err)
		}
	}
	for name, c := range map[string]struct {
		a  Assignment
		ok bool
	}{
		"without":  {Assignment{Kind: AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main"}, true},
		"zero":     {Assignment{Kind: AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main", JournalAck: seq(0)}, true},
		"positive": {Assignment{Kind: AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main", JournalAck: seq(41)}, true},
		"negative": {Assignment{Kind: AssignReconcile, RepositoryID: "repo_1", DefaultBranch: "main", JournalAck: seq(-1)}, false},
	} {
		if err := c.a.Validate(); (err == nil) != c.ok {
			t.Errorf("assignment %s: %v", name, err)
		}
	}
	if !IsPushRefusalReason(ReasonRunInactive) || !IsRefusedEntryReason(ReasonRunInactive) {
		t.Error("run_inactive is a push refusal reason")
	}
}
