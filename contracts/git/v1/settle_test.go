package gitv1

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"testing"
)

type settleCase struct {
	Name     string      `json:"name"`
	Note     string      `json:"note"`
	Entry    SettleEntry `json:"entry"`
	Evidence struct {
		Liveness        Liveness       `json:"liveness"`
		JournalReadable bool           `json:"journalReadable"`
		JournalHead     int64          `json:"journalHead"`
		KnownHead       int64          `json:"knownHead"`
		Journal         []JournalEntry `json:"journal"`
		Current         *CurrentRef    `json:"current"`
	} `json:"evidence"`
	Expect struct {
		Resolution Resolution `json:"resolution"`
		Reason     string     `json:"reason"`
	} `json:"expect"`
}

func (c settleCase) evidence() SettleEvidence {
	e := c.Evidence
	return SettleEvidence{Liveness: e.Liveness, JournalReadable: e.JournalReadable, JournalHead: e.JournalHead, KnownHead: e.KnownHead,
		Journal: append([]JournalEntry{}, e.Journal...), Current: e.Current}
}

func loadSettleCases(t *testing.T) []settleCase {
	t.Helper()
	var cases []settleCase
	if err := json.Unmarshal(mustRead(t, "fixtures/settlement.json"), &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 40 {
		t.Fatalf("only %d settlement cases", len(cases))
	}
	return cases
}

// Test_unit_git_contract_ref_journal (GR-09, GR-A13): the journal entry shape and
// the settlement decision table, including the ABA ordering case, identical
// tuples of two receives and a journal restored from an older snapshot.
func Test_unit_git_contract_ref_journal(t *testing.T) {
	runArea(t, "ref_journal")
	cases := loadSettleCases(t)

	t.Run("decision table", func(t *testing.T) {
		seen := map[string]bool{}
		for _, c := range cases {
			if seen[c.Name] {
				t.Fatalf("duplicate case %s", c.Name)
			}
			seen[c.Name] = true
			t.Run(c.Name, func(t *testing.T) {
				got, err := Settle(c.Entry, c.evidence())
				if err != nil {
					t.Fatal(err)
				}
				if got.Resolution != c.Expect.Resolution || got.Reason != c.Expect.Reason {
					t.Fatalf("%s/%q, want %s/%q (%s)", got.Resolution, got.Reason, c.Expect.Resolution, c.Expect.Reason, c.Note)
				}
				if got.Resolution == ResolveRefused || got.Resolution == ResolveUnknown || got.Resolution == ResolveWait {
					if got.Reason == "" && c.Entry.State != EntryRefused {
						t.Fatalf("a %s decision has no reason", got.Resolution)
					}
				}
				switch got.Resolution {
				case ResolveRefused:
					if got.Reason != "" && !IsRefusedEntryReason(got.Reason) {
						t.Errorf("refusal reason %q is not a refused-entry reason", got.Reason)
					}
				case ResolveUnknown:
					if !IsUnknownEntryReason(got.Reason) {
						t.Errorf("unknown reason %q is not an unknown-entry reason", got.Reason)
					}
				}
			})
		}
		for _, name := range []string{
			"aba_successor_prepared_then_predecessor_committed_late", "identical_tuples_crashed_receive_is_refused_not_confused_with_the_other",
			"restored_older_journal_is_unknown_never_refused", "authorized_is_refused", "committing_receive_still_running_waits",
		} {
			if !seen[name] {
				t.Errorf("the table lost the %s case", name)
			}
		}
	})

	t.Run("the decision does not depend on the order the journal is read in", func(t *testing.T) {
		rng := rand.New(rand.NewSource(7))
		for _, c := range cases {
			want, err := Settle(c.Entry, c.evidence())
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 10; i++ {
				ev := c.evidence()
				rng.Shuffle(len(ev.Journal), func(a, b int) { ev.Journal[a], ev.Journal[b] = ev.Journal[b], ev.Journal[a] })
				if got, _ := Settle(c.Entry, ev); got != want {
					t.Fatalf("%s: %v after shuffling, %v before", c.Name, got, want)
				}
			}
		}
	})

	t.Run("a late outcome entry of another receive never changes a decision", func(t *testing.T) {
		// Git releases ref locks before the committed and aborted hooks run, so
		// outcome entries of other receives arrive late and out of order. They are
		// positive evidence for their own transaction only.
		for _, c := range cases {
			if c.Entry.State != EntryCommitting {
				continue
			}
			want, _ := Settle(c.Entry, c.evidence())
			for _, state := range []JournalState{JournalCommitted, JournalAborted} {
				ev := c.evidence()
				for i := 0; i < 3; i++ {
					id := "rcv_late_" + strconv.Itoa(i)
					ev.JournalHead++
					if ev.KnownHead > 0 {
						ev.KnownHead++ // appended entries must not hide a regression
					}
					ev.Journal = append(ev.Journal, JournalEntry{ReceiveID: &id, Ref: c.Entry.Ref, Old: c.Entry.Old, New: c.Entry.New, State: state, Sequence: ev.JournalHead})
				}
				if got, _ := Settle(c.Entry, ev); got != want {
					t.Errorf("%s: late %s entries of other receives changed %v to %v", c.Name, state, want, got)
				}
			}
		}
	})

	t.Run("malformed input is an error, weak evidence is not", func(t *testing.T) {
		c := cases[0]
		good := c.Entry
		for name, mutate := range map[string]func(e *SettleEntry){
			"empty receive": func(e *SettleEntry) { e.ReceiveID = "" },
			"bad ref":       func(e *SettleEntry) { e.Ref = "refs/heads/a..b" },
			"same values":   func(e *SettleEntry) { e.New = e.Old },
			"bad state":     func(e *SettleEntry) { e.State = "done" },
		} {
			e := good
			mutate(&e)
			if _, err := Settle(e, c.evidence()); err == nil {
				t.Errorf("%s accepted", name)
			}
		}
		committing := good
		committing.State = EntryCommitting
		ev := c.evidence()
		ev.Liveness = "gone"
		if _, err := Settle(committing, ev); err == nil {
			t.Error("an unknown liveness value accepted")
		}
		ev = c.evidence()
		ev.Liveness, ev.JournalReadable, ev.Current = LivenessEnded, true, &CurrentRef{Commit: "xyz", Quiescent: true}
		if _, err := Settle(committing, ev); err == nil {
			t.Error("a malformed current value accepted")
		}
		ev.Current = &CurrentRef{Commit: c64, Quiescent: true}
		if _, err := Settle(committing, ev); err == nil {
			t.Error("a current value of another object ID length accepted")
		}
	})

	t.Run("every non-final decision carries a reason defined by the contract", func(t *testing.T) {
		for _, r := range []string{ReasonNoCommitIntent, ReasonReceiveAborted, ReasonRefUnchanged} {
			if !IsRefusedEntryReason(r) || IsPushRefusalReason(r) {
				t.Errorf("%s", r)
			}
		}
		for _, r := range []string{ReasonJournalUnreadable, ReasonJournalRegressed, ReasonEvidenceMissing, ReasonConflictingEvidence, ReasonRefDiverged, ReasonOperatorRequired} {
			if !IsUnknownEntryReason(r) || IsRefusedEntryReason(r) {
				t.Errorf("%s", r)
			}
		}
	})
}
