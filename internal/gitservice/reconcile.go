package gitservice

import (
	"context"
	"fmt"
	"sort"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The control plane keeps no push-record table (O-36): a receive is one
// git.refs_updated row, and repositories.journal_ack is the highest journal
// sequence below which every receive was recorded. The worker's journal is the
// evidence, so the worker re-reports. This file holds the worker's half:
//
//   - ackState tracks, per repository, the acknowledged sequence the worker last
//     saw and the receives it has reported since;
//   - the live outcome carries a journal sequence only when every other receive
//     in the window after the acknowledgement is reported, aborted or absent
//     (windowSettled); otherwise it omits it and the sweep acknowledges later;
//   - the sweep (reconcile) re-reports every receive after the acknowledged
//     sequence that was not reported, in sequence order, and acknowledges up to
//     the journal head only when none is still running or failed.
//
// Every report is idempotent on (repository, receiveId) at the control plane, so
// repeating one changes nothing.

// sweepEntries bounds the journal entries one sweep page reads. A larger
// backlog is swept page by page: each pass settles the receives that begin in its
// page, acknowledges up to the page's last sequence and the next pass continues
// after it, so a backlog always makes progress. s.snapshotEntries bounds the
// complete evidence of the receives of one page; a page whose receives exceed it
// settles nothing and is reported as an error rather than reported truncated.
// They are Service fields (default below) so tests can shrink them per worker.
const (
	defaultSweepEntries    = 1 << 16
	defaultSnapshotEntries = 1 << 20
)

type ackState struct {
	known    int64
	reported map[string]bool
	// cursor is where the sweep reads from: the highest sequence the worker itself
	// settled (reported, aborted or an operation's), which may exceed the control
	// plane's acknowledgement while that is not yet carried by a report.
	cursor int64
}

// ackOf returns the repository's state; the caller holds s.mu.
func (s *Service) ackOf(repositoryID string) *ackState {
	a := s.acks[repositoryID]
	if a == nil {
		a = &ackState{reported: map[string]bool{}}
		s.acks[repositoryID] = a
	}
	return a
}

// ackAdvanced records the sequence the control plane acknowledged.
func (s *Service) ackAdvanced(repositoryID string, seq int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.ackOf(repositoryID)
	if seq > a.known {
		a.known = seq
	}
}

func (s *Service) markReported(repositoryID, receiveID string) {
	s.mu.Lock()
	s.ackOf(repositoryID).reported[receiveID] = true
	s.mu.Unlock()
}

func (s *Service) isReported(repositoryID, receiveID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ackOf(repositoryID).reported[receiveID]
}

// receiveGroup is the journal entries of one receive in a window.
type receiveGroup struct {
	id                          string
	first, last                 int64
	prepared, committed, aborts []gitv1.JournalEntry
}

// groupReceives groups the receive-attributed entries (operations have no event
// and never block an acknowledgement) by receive, ordered by first sequence.
func groupReceives(entries []gitv1.JournalEntry) []*receiveGroup {
	byID := map[string]*receiveGroup{}
	var order []*receiveGroup
	for _, e := range entries {
		if e.ReceiveID == nil {
			continue
		}
		g := byID[*e.ReceiveID]
		if g == nil {
			g = &receiveGroup{id: *e.ReceiveID, first: e.Sequence}
			byID[g.id] = g
			order = append(order, g)
		}
		g.last = e.Sequence
		switch e.State {
		case gitv1.JournalPrepared:
			g.prepared = append(g.prepared, e)
		case gitv1.JournalCommitted:
			g.committed = append(g.committed, e)
		case gitv1.JournalAborted:
			g.aborts = append(g.aborts, e)
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].first < order[j].first })
	return order
}

// windowSettled reports whether every receive other than self with entries after
// known is safe to acknowledge past: reported, or without any applied update.
func (s *Service) windowSettled(dir, repositoryID, self string, known int64) bool {
	res, err := journalAt(dir).readExisting(known, s.sweepEntries, nil)
	if err != nil || res.HasMore {
		return false
	}
	for _, g := range groupReceives(res.Entries) {
		if g.id == self || s.isReported(repositoryID, g.id) {
			continue
		}
		if s.receiveRunning(repositoryID, g.id) {
			return false
		}
		if len(g.committed) > 0 || len(g.prepared) > len(g.aborts)+len(g.committed) {
			return false // an unreported applied (or undecided) receive
		}
	}
	return true
}

// ackSequence is the journal sequence the live outcome of rc may carry: its own
// last entry, when everything else in the window is settled. Nil omits it.
func (s *Service) ackSequence(rc *receive) *int64 {
	s.mu.Lock()
	known := s.ackOf(rc.repositoryID).known
	s.mu.Unlock()
	res, err := journalAt(rc.dir).readExisting(known, s.sweepEntries, nil)
	if err != nil || res.HasMore {
		return nil
	}
	var last int64
	for _, e := range res.Entries {
		if e.ReceiveID != nil && *e.ReceiveID == rc.id && e.Sequence > last {
			last = e.Sequence
		}
	}
	if last == 0 || !s.windowSettled(rc.dir, rc.repositoryID, rc.id, known) {
		return nil
	}
	return &last
}

// receivePosition is the journal sequence of the receive's first entry: the
// receive's position in the order its updates were applied (its prepared entries
// are appended while it holds the locks of every ref it updates). It is nil when
// the journal cannot be read or holds no entry of the receive.
func receivePosition(dir, receiveID string) *int64 {
	res, err := journalAt(dir).readExisting(0, 1, func(e gitv1.JournalEntry) bool { return e.ReceiveID != nil && *e.ReceiveID == receiveID })
	if err != nil || len(res.Entries) == 0 {
		return nil
	}
	first := res.Entries[0].Sequence
	return &first
}

// Reconcile polls the control plane's assignments once and performs them,
// including the re-report of unacknowledged receives. The poll loop calls it on
// its interval; tests and an operator may call it directly.
func (s *Service) Reconcile(ctx context.Context) error { return s.processAssignments(ctx) }

// receiveEvidence is the outcome of one receive's journal entries.
type receiveEvidence int

const (
	evidenceSettled receiveEvidence = iota
	evidenceAmbiguous
)

// recoverApplied decides, from complete evidence of an ended receive, which of its
// updates were applied. A committed entry is applied and an aborted one is not.
// A prepared entry with no outcome (a worker killed between Git's commit and the
// hook's own append, or a ref a later transaction followed) is decided by the
// next value of the ref: the observed value of the next prepared entry of that
// ref by any transaction, or, when there is none, the ref's current value (an
// absent ref is the all-zero ID) with the journal unchanged around the read.
// Equal to the new value is applied, equal to the value before is not, anything
// else is ambiguous, and a receive with one ambiguous update is not reported at
// all: a partial event would win the deterministic event ID.
func (s *Service) recoverApplied(ctx context.Context, dir string, g *receiveGroup) ([]gitv1.RefUpdate, receiveEvidence, error) {
	applied := []gitv1.RefUpdate{}
	decided := map[string]bool{}
	for _, e := range g.committed {
		applied = append(applied, gitv1.RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New})
		decided[e.Ref] = true
	}
	for _, e := range g.aborts {
		decided[e.Ref] = true
	}
	for _, e := range g.prepared {
		if decided[e.Ref] {
			continue
		}
		verdict, err := s.refVerdict(ctx, dir, e)
		if err != nil {
			return nil, evidenceAmbiguous, err
		}
		switch verdict {
		case verdictApplied:
			applied = append(applied, gitv1.RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New})
		case verdictNotApplied:
		default:
			return nil, evidenceAmbiguous, nil
		}
	}
	return applied, evidenceSettled, nil
}

type refVerdictKind int

const (
	verdictAmbiguous refVerdictKind = iota
	verdictApplied
	verdictNotApplied
)

func (s *Service) refVerdict(ctx context.Context, dir string, e gitv1.JournalEntry) (refVerdictKind, error) {
	if e.Observed == nil {
		return verdictAmbiguous, nil
	}
	before := *e.Observed
	decide := func(value string) refVerdictKind {
		switch value {
		case e.New:
			return verdictApplied
		case before:
			return verdictNotApplied
		}
		return verdictAmbiguous
	}
	j := journalAt(dir)
	later, err := j.readExisting(e.Sequence, 1, func(x gitv1.JournalEntry) bool { return x.Ref == e.Ref && x.State == gitv1.JournalPrepared })
	if err != nil {
		return verdictAmbiguous, err
	}
	if len(later.Entries) > 0 && later.Entries[0].Observed != nil {
		return decide(*later.Entries[0].Observed), nil
	}
	current, err := s.readRef(ctx, dir, e.Ref)
	if err != nil {
		return verdictAmbiguous, err
	}
	if current == "" {
		current = gitv1.ZeroObjectID(len(e.New))
	}
	if head, err := j.head(); err != nil || head != later.Head {
		return verdictAmbiguous, err // a transaction began while the ref was read: decide next time
	}
	return decide(current), nil
}

// reconcileRepository re-reports the receives of the repository's journal after
// the acknowledged sequence and acknowledges the settled prefix.
//
// The order is what makes a report complete. The candidates are the receives that
// begin in one bounded page after the cursor. Liveness is established first, and
// only then is the complete evidence of the ended candidates read in one
// snapshot, so a receive that finished between the page read and the liveness
// check is read whole and a running one is never reported. Candidates are decided
// in sequence order; the sweep stops at the first receive that is still running
// or whose outcome is ambiguous and never acknowledges past it.
func (s *Service) reconcileRepository(ctx context.Context, a gitv1.Assignment) error {
	dir, err := s.repoDir(a.RepositoryID)
	if err != nil {
		return err
	}
	if !isRepository(dir) {
		return nil
	}
	var ack int64
	if a.JournalAck != nil {
		ack = *a.JournalAck
	}
	s.ackAdvanced(a.RepositoryID, ack)
	s.mu.Lock()
	start := max(ack, s.ackOf(a.RepositoryID).cursor)
	s.mu.Unlock()

	j := journalAt(dir)
	page, err := j.readExisting(start, s.sweepEntries, nil)
	if err != nil {
		return fmt.Errorf("reading the journal: %w", err)
	}
	if len(page.Entries) == 0 {
		return nil
	}
	pageLast := page.Head
	if page.HasMore {
		pageLast = page.Entries[len(page.Entries)-1].Sequence
	}
	candidates := groupReceives(page.Entries)
	if hook := s.sweepHook; hook != nil {
		hook(a.RepositoryID)
	}

	// Liveness first, evidence second.
	var ended []*receiveGroup
	var blocked int64
	for _, g := range candidates {
		if s.isReported(a.RepositoryID, g.id) {
			continue // settled already; re-reporting would change nothing
		}
		if s.receiveRunning(a.RepositoryID, g.id) {
			blocked = g.first
			break
		}
		ended = append(ended, g)
	}
	ids := make(map[string]bool, len(ended))
	for _, g := range ended {
		ids[g.id] = true
	}
	full := map[string]*receiveGroup{}
	if len(ids) > 0 {
		snap, err := j.readExisting(start, s.snapshotEntries, func(e gitv1.JournalEntry) bool { return e.ReceiveID != nil && ids[*e.ReceiveID] })
		if err != nil {
			return fmt.Errorf("reading the journal: %w", err)
		}
		if snap.HasMore {
			return fmt.Errorf("the evidence of %d receives exceeds %d journal entries; nothing is reported", len(ids), s.snapshotEntries)
		}
		for _, g := range groupReceives(snap.Entries) {
			full[g.id] = g
		}
	}

	type report struct {
		id      string
		applied []gitv1.RefUpdate
	}
	var reports []report
	for _, g := range ended {
		applied, verdict, err := s.recoverApplied(ctx, dir, full[g.id])
		if err != nil {
			return fmt.Errorf("receive %s: %w", g.id, err)
		}
		if verdict == evidenceAmbiguous {
			s.cfg.Logger.Warn("a receive's outcome is ambiguous and stays unacknowledged", "repositoryId", a.RepositoryID, "receiveId", g.id)
			blocked = g.first
			break
		}
		reports = append(reports, report{g.id, applied})
	}
	settled := pageLast
	if blocked != 0 {
		settled = blocked - 1
	}

	var first error
	for i, r := range reports {
		req := gitv1.OutcomeRequest{ReceiveID: r.id, Applied: r.applied}
		if g := full[r.id]; g != nil && g.first > 0 {
			position := g.first
			req.JournalPosition = &position
		}
		if i == len(reports)-1 && first == nil && settled > ack {
			seq := settled
			req.JournalSequence = &seq
		}
		cctx, cancel := context.WithTimeout(ctx, s.cfg.Limits.ControlTimeout)
		_, err := s.cfg.ControlPlane.Outcome(cctx, r.id, req)
		cancel()
		switch {
		case err == nil:
			s.markReported(a.RepositoryID, r.id)
			if req.JournalSequence != nil {
				s.ackAdvanced(a.RepositoryID, *req.JournalSequence)
			}
		default:
			if ce, ok := asContract(err); ok && ce.Code != domain.CodeUnavailable {
				// A final refusal (an unknown grant, for example) cannot be
				// repaired by repeating the report.
				s.markReported(a.RepositoryID, r.id)
				s.cfg.Logger.Warn("re-reported receive refused", "repositoryId", a.RepositoryID, "receiveId", r.id, "error", err)
				continue
			}
			if first == nil {
				first = fmt.Errorf("receive %s: %w", r.id, err)
			}
		}
	}
	if first == nil && settled > start {
		s.mu.Lock()
		if a := s.ackOf(a.RepositoryID); settled > a.cursor {
			a.cursor = settled
		}
		s.mu.Unlock()
	}
	return first
}
