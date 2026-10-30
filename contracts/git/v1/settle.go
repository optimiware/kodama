package gitv1

// Settlement is the decision table of git-repositories.md, "Push journal and
// events", as a pure function. The reconciler (T-130) gathers the evidence from
// the worker (receive liveness, the ref's journal, the ref's current value under
// the worker's mutation lock) and applies the decision to the entry in SQL; this
// function never reads anything itself and never decides from ref values alone.

// Resolution is the outcome of one settlement decision.
type Resolution string

// Resolutions.
const (
	ResolveApplied Resolution = "applied" // the entry's update committed
	ResolveRefused Resolution = "refused" // the entry's update never changed its ref
	ResolveUnknown Resolution = "unknown" // the evidence cannot decide; an operator resolves it
	ResolveWait    Resolution = "wait"    // not yet decidable; retry later
)

// Liveness is what the control plane established about the entry's receive.
type Liveness string

// Liveness values.
const (
	// LivenessRunning: the worker reports the receive still running. The worker
	// enforces the receive deadline by terminating overrunning receives, so the
	// receive ends.
	LivenessRunning Liveness = "running"
	// LivenessEnded: the worker reports the receive not running (finished,
	// terminated or never heard of by a worker instance that wrote its journal).
	LivenessEnded Liveness = "ended"
	// LivenessWorkerStopped: the worker instance that ran the receive is verified
	// stopped (for example the Pod is gone and the volume is attached to nothing).
	LivenessWorkerStopped Liveness = "worker_stopped"
	// LivenessUnavailable: neither could be established now (worker unreachable).
	LivenessUnavailable Liveness = "unavailable"
)

// CurrentRef is a ref's current value, read under the worker's per-repository
// mutation lock. Quiescent is true only when no receive or server operation was
// in progress at the read (ReadRefValuesResponse.Quiescent). Commit is the
// all-zero ID when the ref is absent.
type CurrentRef struct {
	Commit    string `json:"commit"`
	Quiescent bool   `json:"quiescent"`
}

// SettleEntry is the push-record entry being settled and its receive.
type SettleEntry struct {
	ReceiveID string     `json:"receiveId"`
	Ref       string     `json:"ref"`
	Old       string     `json:"old"`
	New       string     `json:"new"`
	State     EntryState `json:"state"`
}

// SettleEvidence is everything the decision may use.
//
//   - Liveness of the entry's receive (committing entries only).
//   - JournalReadable is false when the worker's journal could not be read.
//   - Journal holds every journal entry of the entry's ref, whatever its
//     attribution (a receive or a server operation), complete from the entry's
//     own prepared entry on; entries of other refs are ignored. JournalHead is
//     the journal's highest sequence at the read.
//   - KnownHead is the highest journal head the control plane saw before for this
//     repository (0 if never): a journal whose head is lower than that came from
//     an older snapshot.
//   - Current is the ref's value read under the lock with no receive in progress;
//     nil when it was not read.
type SettleEvidence struct {
	Liveness        Liveness
	JournalReadable bool
	JournalHead     int64
	KnownHead       int64
	Journal         []JournalEntry
	Current         *CurrentRef
}

// Decision is a resolution with its stable reason: the refusal, unknown or wait
// reason of codes.go, empty for an applied entry and for an entry that was
// already final.
type Decision struct {
	Resolution Resolution
	Reason     string
}

func decide(r Resolution, reason string) (Decision, error) {
	return Decision{Resolution: r, Reason: reason}, nil
}

// Settle decides what the evidence proves about one entry:
//
//   - applied, refused: already final, returned unchanged.
//   - unknown: stays unknown (operator_required) until an operator resolves it.
//   - authorized (no commit intent): refused. A late prepared hook is then
//     refused commit intent by the control plane and aborts, so this is safe at
//     any time.
//   - committing with the receive still running: wait.
//   - committing with the receive ended or its worker verified stopped:
//     once the evidence passes the guards below, the transaction's own committed
//     entry: applied; its own aborted entry: refused. With only its own prepared
//     entry, the ref's next value decides:
//     the observed value of the next prepared entry of that ref (by sequence), or
//     if there is none the current value read under the lock with no receive in
//     progress; equal to new: applied; equal to the value observed before the
//     transaction (old): refused; anything else: unknown.
//   - guards, evaluated before any outcome entry or value is accepted: an
//     unreadable journal, a missing, duplicated, contradictory or inconsistent
//     entry, a journal whose head is below one already seen (a restore: even its
//     committed or aborted entries prove nothing), and an observed value that
//     contradicts old: unknown, with a reason. A value that is neither old nor
//     new is unknown as well.
//
// Outcome entries (committed, aborted) are positive evidence for their own
// transaction only: Git releases ref locks before those hooks run, so they can be
// appended late and out of order, and they never order or settle another
// transaction. Attribution comes from the receive ID, never from matching
// (ref, old, new) values, so identical tuples of two receives are not confused.
//
// The error is non-nil only for malformed input (an invalid entry or current
// value), never for weak evidence.
func Settle(entry SettleEntry, ev SettleEvidence) (Decision, error) {
	if err := firstError(checkID("receiveId", entry.ReceiveID), (RefUpdate{Ref: entry.Ref, Old: entry.Old, New: entry.New}).Validate()); err != nil {
		return Decision{}, err
	}
	if !entry.State.valid() {
		return Decision{}, fail("state", "%q is not a push entry state", truncate(string(entry.State), 32))
	}
	switch entry.State {
	case EntryApplied:
		return decide(ResolveApplied, "")
	case EntryRefused:
		return decide(ResolveRefused, "")
	case EntryUnknown:
		return decide(ResolveUnknown, ReasonOperatorRequired)
	case EntryAuthorized:
		return decide(ResolveRefused, ReasonNoCommitIntent)
	}

	switch ev.Liveness {
	case LivenessRunning:
		return decide(ResolveWait, ReasonReceiveRunning)
	case LivenessUnavailable:
		return decide(ResolveWait, ReasonLivenessUnavailable)
	case LivenessEnded, LivenessWorkerStopped:
	default:
		return Decision{}, fail("liveness", "%q is not a liveness value", truncate(string(ev.Liveness), 32))
	}
	if ev.Current != nil {
		if err := checkObjectID("current.commit", ev.Current.Commit); err != nil {
			return Decision{}, err
		}
		if len(ev.Current.Commit) != len(entry.New) {
			return Decision{}, fail("current.commit", "must have the object ID length of the update")
		}
	}
	if !ev.JournalReadable {
		return decide(ResolveUnknown, ReasonJournalUnreadable)
	}

	var own struct{ prepared, committed, aborted []JournalEntry }
	var prepared []JournalEntry // every prepared entry of the ref, own included
	seq := map[int64]bool{}
	for _, j := range ev.Journal {
		if j.Validate() != nil || seq[j.Sequence] || j.Sequence > ev.JournalHead {
			return decide(ResolveUnknown, ReasonConflictingEvidence)
		}
		seq[j.Sequence] = true
		if j.Ref != entry.Ref {
			continue
		}
		if j.State == JournalPrepared {
			prepared = append(prepared, j)
		}
		if j.ReceiveID == nil || *j.ReceiveID != entry.ReceiveID {
			continue
		}
		if j.Old != entry.Old || j.New != entry.New {
			return decide(ResolveUnknown, ReasonConflictingEvidence)
		}
		switch j.State {
		case JournalPrepared:
			own.prepared = append(own.prepared, j)
		case JournalCommitted:
			own.committed = append(own.committed, j)
		case JournalAborted:
			own.aborted = append(own.aborted, j)
		}
	}
	if len(own.prepared) > 1 || len(own.committed) > 1 || len(own.aborted) > 1 || (len(own.committed) == 1 && len(own.aborted) == 1) {
		return decide(ResolveUnknown, ReasonConflictingEvidence)
	}
	if len(own.prepared) == 0 && len(own.committed)+len(own.aborted) > 0 {
		// An outcome without its prepared entry: the journal is inconsistent.
		return decide(ResolveUnknown, ReasonConflictingEvidence)
	}
	for _, o := range append(append([]JournalEntry{}, own.committed...), own.aborted...) {
		if o.Sequence < own.prepared[0].Sequence {
			return decide(ResolveUnknown, ReasonConflictingEvidence)
		}
	}
	// A journal older than one already seen cannot prove anything about the
	// transaction, whichever entries it holds: a restored volume may lack later
	// transactions on the ref, and its outcome entries are not trustworthy
	// either. This guard comes before every positive or negative conclusion.
	if ev.KnownHead > ev.JournalHead {
		return decide(ResolveUnknown, ReasonJournalRegressed)
	}
	if len(own.prepared) == 0 {
		return decide(ResolveUnknown, ReasonEvidenceMissing)
	}
	p := own.prepared[0]
	before := *p.Observed
	if (!IsZeroObjectID(entry.Old) && before != entry.Old) || before == entry.New {
		// Git verified old under the lock, so a different observed value (or one
		// that already equals the new value) contradicts the entry, and with it
		// any outcome entry of the same transaction.
		return decide(ResolveUnknown, ReasonConflictingEvidence)
	}
	if len(own.committed) == 1 {
		return decide(ResolveApplied, "")
	}
	if len(own.aborted) == 1 {
		return decide(ResolveRefused, ReasonReceiveAborted)
	}

	// Only the prepared entry: the next value of the ref decides.
	var value string
	var next *JournalEntry
	for i := range prepared {
		if prepared[i].Sequence > p.Sequence && (next == nil || prepared[i].Sequence < next.Sequence) {
			next = &prepared[i]
		}
	}
	switch {
	case next != nil:
		value = *next.Observed
	case ev.Current != nil && ev.Current.Quiescent:
		value = ev.Current.Commit
	default:
		return decide(ResolveWait, ReasonRefNotQuiescent)
	}
	switch value {
	case entry.New:
		return decide(ResolveApplied, "")
	case before:
		return decide(ResolveRefused, ReasonRefUnchanged)
	}
	return decide(ResolveUnknown, ReasonRefDiverged)
}
