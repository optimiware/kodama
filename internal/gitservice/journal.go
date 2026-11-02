package gitservice

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// The ref journal is an append-only JSON-lines file inside each bare
// repository, outside refs/ and objects/, so no Git endpoint serves it. Every
// append runs under an exclusive flock, is one write and is fsynced before it
// returns (a hook asks the control plane for commit intent only after that);
// readers take the shared lock and ignore an unterminated final line, which a
// crash mid-append can leave and the next append truncates. Sequence numbers
// start at 1 and increase by exactly 1.
const (
	journalDir  = "kodama"
	journalFile = "journal.jsonl"
	// maxJournalLine bounds one entry (a ref of 255 bytes and three 64-digit IDs
	// stay far below it); a longer line is corruption.
	maxJournalLine = 4096
	// scanBlock is the size of one backward read when an outcome looks for its
	// transaction's prepared entry.
	scanBlock = 256 << 10
)

// errJournalCorrupt reports a journal the worker cannot trust: appends and
// reads fail closed, and the control plane settles affected entries unknown
// (journal_unreadable).
var errJournalCorrupt = errors.New("gitservice: the ref journal is unreadable")

type journal struct{ path string }

func journalAt(repoDir string) journal {
	return journal{path: filepath.Join(repoDir, journalDir, journalFile)}
}

// attribution says whose transaction an entry belongs to: exactly one of a
// receive and a server operation.
type attribution struct {
	receiveID   string
	operationID string
}

func (a attribution) valid() bool { return (a.receiveID == "") != (a.operationID == "") }

func (a attribution) matches(e gitv1.JournalEntry) bool {
	if a.receiveID != "" {
		return e.ReceiveID != nil && *e.ReceiveID == a.receiveID
	}
	return e.OperationID != nil && *e.OperationID == a.operationID
}

func (a attribution) entry(u gitv1.RefUpdate, state gitv1.JournalState, observed *string) gitv1.JournalEntry {
	e := gitv1.JournalEntry{Ref: u.Ref, Old: u.Old, New: u.New, Observed: observed, State: state}
	if a.receiveID != "" {
		id := a.receiveID
		e.ReceiveID = &id
	} else {
		id := a.operationID
		e.OperationID = &id
	}
	return e
}

// create makes the journal file durable at repository initialization.
func (j journal) create() error {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(j.path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(j.path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// withFile opens the journal (creating it when exclusive) and runs fn with the
// requested flock held.
func (j journal) withFile(exclusive bool, fn func(f *os.File) error) error {
	flags, lock := os.O_RDONLY, syscall.LOCK_SH
	if exclusive {
		flags, lock = os.O_RDWR|os.O_CREATE|os.O_APPEND, syscall.LOCK_EX
		if err := os.MkdirAll(filepath.Dir(j.path), 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(j.path, flags, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		err = syscall.Flock(int(f.Fd()), lock)
		if err != syscall.EINTR {
			break
		}
	}
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn(f)
}

func parseEntry(line []byte) (gitv1.JournalEntry, error) {
	e, err := gitv1.DecodeRequest[gitv1.JournalEntry](line)
	if err != nil {
		return gitv1.JournalEntry{}, fmt.Errorf("%w: %v", errJournalCorrupt, err)
	}
	return e, nil
}

// repairTail truncates an unterminated final line (a crashed append whose
// caller never got its answer) and returns the size of the intact journal.
func repairTail(f *os.File) (int64, error) {
	st, err := f.Stat()
	if err != nil {
		return 0, err
	}
	size := st.Size()
	if size == 0 {
		return 0, nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, size-1); err != nil {
		return 0, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	window := int64(maxJournalLine)
	if window > size {
		window = size
	}
	buf := make([]byte, window)
	if _, err := f.ReadAt(buf, size-window); err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(buf, '\n')
	var keep int64
	switch {
	case i >= 0:
		keep = size - window + int64(i) + 1
	case window == size:
		keep = 0
	default:
		return 0, fmt.Errorf("%w: a line longer than %d bytes", errJournalCorrupt, maxJournalLine)
	}
	if err := f.Truncate(keep); err != nil {
		return 0, err
	}
	return keep, f.Sync()
}

// lastEntry returns the final entry of an intact journal of the given size, and
// false for an empty one.
func lastEntry(f *os.File, size int64) (gitv1.JournalEntry, bool, error) {
	if size == 0 {
		return gitv1.JournalEntry{}, false, nil
	}
	window := int64(maxJournalLine)
	if window > size {
		window = size
	}
	buf := make([]byte, window)
	if _, err := f.ReadAt(buf, size-window); err != nil {
		return gitv1.JournalEntry{}, false, err
	}
	buf = buf[:len(buf)-1] // the final newline
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	} else if window != size {
		return gitv1.JournalEntry{}, false, fmt.Errorf("%w: a line longer than %d bytes", errJournalCorrupt, maxJournalLine)
	}
	e, err := parseEntry(buf)
	return e, err == nil, err
}

// scanBackward visits the entries of an intact journal of the given size from
// the newest to the oldest, in blocks, until visit reports it is done. It reads
// as little as the visitor needs and as much as it takes: there is no cutoff.
func scanBackward(f *os.File, size int64, visit func(gitv1.JournalEntry) (done bool)) error {
	var carry []byte // the beginning of a line whose start is in an earlier block
	pos := size
	for pos > 0 {
		n := int64(scanBlock)
		if n > pos {
			n = pos
		}
		buf := make([]byte, n+int64(len(carry)))
		if _, err := f.ReadAt(buf[:n], pos-n); err != nil {
			return err
		}
		copy(buf[n:], carry)
		pos -= n
		lines := buf
		carry = nil
		if pos > 0 { // the first line of this block may start earlier
			i := bytes.IndexByte(buf, '\n')
			if i < 0 {
				if len(buf) > maxJournalLine {
					return fmt.Errorf("%w: a line longer than %d bytes", errJournalCorrupt, maxJournalLine)
				}
				carry = buf
				continue
			}
			carry = append([]byte(nil), buf[:i]...)
			lines = buf[i+1:]
		}
		parts := bytes.Split(bytes.TrimSuffix(lines, []byte("\n")), []byte("\n"))
		for i := len(parts) - 1; i >= 0; i-- {
			if len(parts[i]) == 0 {
				continue
			}
			e, err := parseEntry(parts[i])
			if err != nil {
				return err
			}
			if visit(e) {
				return nil
			}
		}
	}
	return nil
}

// append writes entries as one fsynced write, assigning consecutive sequence
// numbers. build receives the intact tail so a caller can decide what to write
// under the lock; returning no entries writes nothing.
func (j journal) append(build func(f *os.File, size int64, next int64) ([]gitv1.JournalEntry, error)) ([]gitv1.JournalEntry, error) {
	var written []gitv1.JournalEntry
	err := j.withFile(true, func(f *os.File) error {
		size, err := repairTail(f)
		if err != nil {
			return err
		}
		next := int64(1)
		if last, ok, err := lastEntry(f, size); err != nil {
			return err
		} else if ok {
			next = last.Sequence + 1
		}
		entries, err := build(f, size, next)
		if err != nil || len(entries) == 0 {
			return err
		}
		var out bytes.Buffer
		for i := range entries {
			entries[i].Sequence = next + int64(i)
			if err := entries[i].Validate(); err != nil {
				return fmt.Errorf("gitservice: journal entry: %w", err)
			}
			line, err := json.Marshal(entries[i])
			if err != nil {
				return err
			}
			if len(line) >= maxJournalLine {
				return fmt.Errorf("gitservice: journal entry of %d bytes", len(line))
			}
			out.Write(line)
			out.WriteByte('\n')
		}
		if _, err := f.Write(out.Bytes()); err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		written = entries
		return nil
	})
	return written, err
}

// appendPrepared records the prepared entries of one ref transaction with the
// values observed under Git's ref locks.
func (j journal) appendPrepared(a attribution, updates []gitv1.RefUpdate, observed []string) ([]gitv1.JournalEntry, error) {
	if !a.valid() || len(updates) != len(observed) {
		return nil, errors.New("gitservice: invalid journal append")
	}
	return j.append(func(*os.File, int64, int64) ([]gitv1.JournalEntry, error) {
		entries := make([]gitv1.JournalEntry, len(updates))
		for i, u := range updates {
			o := observed[i]
			entries[i] = a.entry(u, gitv1.JournalPrepared, &o)
		}
		return entries, nil
	})
}

// appendOutcome records state (committed or aborted) for every update of the
// transaction whose own prepared entry has no outcome yet. An update without a
// pending prepared entry gets nothing: an outcome without its prepared entry is
// inconsistent evidence and the settlement would call it unknown.
func (j journal) appendOutcome(a attribution, state gitv1.JournalState, updates []gitv1.RefUpdate) ([]gitv1.JournalEntry, error) {
	if !a.valid() || (state != gitv1.JournalCommitted && state != gitv1.JournalAborted) {
		return nil, errors.New("gitservice: invalid journal append")
	}
	return j.append(func(f *os.File, size, _ int64) ([]gitv1.JournalEntry, error) {
		pending, err := pendingPrepared(f, size, a, updates)
		if err != nil {
			return nil, err
		}
		var entries []gitv1.JournalEntry
		for i, u := range updates {
			if pending[i] {
				entries = append(entries, a.entry(u, state, nil))
			}
		}
		return entries, nil
	})
}

// appendVerifiedOutcome records the outcome of a transaction Git reported as
// committed, but only as far as the evidence supports it. Git runs that hook
// for refs it rejected inside a non-atomic receive, and other transactions on
// the same refs run concurrently (receives hold the mutation gate shared and Git
// releases its ref locks before the hook), so the ref's current value alone
// proves nothing about this transaction. Under the exclusive append lock, which
// every prepared entry also takes while its ref lock is held, for each update:
//
//   - no pending prepared entry of the transaction's own: nothing is written;
//   - any later prepared entry for the ref (of any transaction): nothing is
//     written, the ref may have moved on and the settlement decides from that
//     entry's observed value;
//   - otherwise no other transaction can have changed the ref (a transaction
//     that already holds its lock cannot journal prepared, and so cannot commit,
//     before this append releases the lock), so the exact current value is read:
//     the new value (absent, for a delete) is journaled committed, the old value
//     (absent, for a create) is journaled aborted, anything else, or a value that
//     cannot be read, nothing.
//
// current returns the exact value of a ref ("" when absent).
func (j journal) appendVerifiedOutcome(a attribution, updates []gitv1.RefUpdate, current func(ref string) (string, error)) ([]gitv1.JournalEntry, error) {
	if !a.valid() {
		return nil, errors.New("gitservice: invalid journal append")
	}
	return j.append(func(f *os.File, size, _ int64) ([]gitv1.JournalEntry, error) {
		pending, superseded, err := ownPendingUnsuperseded(f, size, a, updates)
		if err != nil {
			return nil, err
		}
		var entries []gitv1.JournalEntry
		for i, u := range updates {
			if !pending[i] || superseded[i] {
				continue
			}
			v, err := current(u.Ref)
			if err != nil {
				continue
			}
			switch {
			case v == objectOrAbsent(u.New):
				entries = append(entries, a.entry(u, gitv1.JournalCommitted, nil))
			case v == objectOrAbsent(u.Old):
				entries = append(entries, a.entry(u, gitv1.JournalAborted, nil))
			}
		}
		return entries, nil
	})
}

// objectOrAbsent maps the all-zero ID to the "" of an absent ref.
func objectOrAbsent(id string) string {
	if gitv1.IsZeroObjectID(id) {
		return ""
	}
	return id
}

// ownPendingUnsuperseded scans backward, like pendingPrepared, and reports for
// each update whether the transaction's own latest entry for its tuple is a
// prepared one without an outcome, and whether any prepared entry for that ref
// that is not the transaction's own tuple follows it.
func ownPendingUnsuperseded(f *os.File, size int64, a attribution, updates []gitv1.RefUpdate) (pending, superseded []bool, err error) {
	pending, superseded = make([]bool, len(updates)), make([]bool, len(updates))
	unresolved := make(map[gitv1.RefUpdate]int, len(updates))
	refs := make(map[string]bool, len(updates))
	for i, u := range updates {
		unresolved[u] = i
		refs[u.Ref] = true
	}
	later := map[string]bool{} // a foreign prepared entry for the ref seen so far (newer than the cursor)
	err = scanBackward(f, size, func(e gitv1.JournalEntry) bool {
		key := gitv1.RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New}
		if i, ok := unresolved[key]; ok && a.matches(e) {
			pending[i] = e.State == gitv1.JournalPrepared
			superseded[i] = later[e.Ref]
			delete(unresolved, key)
		} else if e.State == gitv1.JournalPrepared && refs[e.Ref] {
			later[e.Ref] = true
		}
		return len(unresolved) == 0
	})
	return pending, superseded, err
}

// pendingPrepared reports, for each update, whether the most recent entry of the
// transaction's tuple (attribution, ref, old, new) is a prepared one with no
// outcome after it. It scans backward from the end and stops as soon as every
// tuple is resolved, so the usual case (a transaction that is seconds old) reads
// one block; a tuple that never appears reads the whole journal.
func pendingPrepared(f *os.File, size int64, a attribution, updates []gitv1.RefUpdate) ([]bool, error) {
	pending := make([]bool, len(updates))
	unresolved := make(map[gitv1.RefUpdate]int, len(updates))
	for i, u := range updates {
		unresolved[u] = i
	}
	err := scanBackward(f, size, func(e gitv1.JournalEntry) bool {
		if !a.matches(e) {
			return false
		}
		key := gitv1.RefUpdate{Ref: e.Ref, Old: e.Old, New: e.New}
		if i, ok := unresolved[key]; ok {
			pending[i] = e.State == gitv1.JournalPrepared
			delete(unresolved, key)
		}
		return len(unresolved) == 0
	})
	return pending, err
}

// readResult is one page of the journal.
type readResult struct {
	Entries []gitv1.JournalEntry
	Head    int64
	HasMore bool
}

// read scans the journal and returns up to limit entries after since that pass
// match (nil matches all). A missing journal is empty. Every line is decoded
// strictly and the sequence must advance by exactly one: anything else is
// errJournalCorrupt.
func (j journal) read(since int64, limit int, match func(gitv1.JournalEntry) bool) (readResult, error) {
	return j.readFile(since, limit, match, false)
}

// readExisting is read for a repository that has been initialized: its journal
// was made durable at initialization, so a missing file is evidence of damage
// (a lost or partially restored volume), not an empty journal, and is
// errJournalCorrupt.
func (j journal) readExisting(since int64, limit int, match func(gitv1.JournalEntry) bool) (readResult, error) {
	return j.readFile(since, limit, match, true)
}

func (j journal) readFile(since int64, limit int, match func(gitv1.JournalEntry) bool, mustExist bool) (readResult, error) {
	var res readResult
	err := j.withFile(false, func(f *os.File) error {
		br := bufio.NewReaderSize(f, 64<<10)
		var seq int64
		for {
			line, err := br.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				return fmt.Errorf("%w: a line longer than the read buffer", errJournalCorrupt)
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil // an unterminated tail is ignored
				}
				return err
			}
			if len(line) > maxJournalLine {
				return fmt.Errorf("%w: a line longer than %d bytes", errJournalCorrupt, maxJournalLine)
			}
			e, err := parseEntry(bytes.TrimSuffix(line, []byte("\n")))
			if err != nil {
				return err
			}
			if e.Sequence != seq+1 {
				return fmt.Errorf("%w: sequence %d follows %d", errJournalCorrupt, e.Sequence, seq)
			}
			seq = e.Sequence
			res.Head = seq
			if e.Sequence <= since || (match != nil && !match(e)) {
				continue
			}
			if len(res.Entries) >= limit {
				res.HasMore = true
				continue
			}
			res.Entries = append(res.Entries, e)
		}
	})
	if errors.Is(err, os.ErrNotExist) {
		if mustExist {
			return readResult{}, fmt.Errorf("%w: the journal file is missing", errJournalCorrupt)
		}
		return readResult{}, nil
	}
	return res, err
}

// head returns the highest sequence of the journal.
func (j journal) head() (int64, error) {
	r, err := j.read(0, 0, func(gitv1.JournalEntry) bool { return false })
	return r.Head, err
}
