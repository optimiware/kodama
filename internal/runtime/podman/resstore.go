package podman

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
)

// The durable state of the run resources lives under StateDir/resources:
//
//	NAME.lock           the file lock serializing every operation on NAME
//	NAME/record.json    the intent of the resource, written BEFORE its
//	                    engine object exists and kept as a tombstone after the
//	                    object is removed, so a stopped or removed resource is
//	                    never created again (not even by a late create)
//	NAME/bootstrap/     what a preparation or service mounts read-only: its
//	                    admission credential, the CA and checkout material;
//	                    destroyed at stop
//
// An engine object without a record of this installation is not ours; a record
// owns an object only if the object's labels, instance UID and engine identity
// all match it.

// resRecord is the durable state of one run resource.
type resRecord struct {
	Name  string                 `json:"name"`
	Scope domain.Scope           `json:"scope"`
	RunID domain.RunID           `json:"runId"`
	Kind  execution.ResourceKind `json:"kind"`
	Key   string                 `json:"key"`
	// UID is the instance identity. A tombstone made without a record keeps the
	// UID the caller supplied (possibly empty) and is Untracked: its identity was
	// never verified, and it never yields a not-started proof.
	UID         string `json:"uid"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Untracked   bool   `json:"untracked,omitempty"`
	// Stopping is the stop intent (the fence); Stopped is the verified stop.
	Stopping bool                     `json:"stopping,omitempty"`
	Stopped  bool                     `json:"stopped,omitempty"`
	Removed  bool                     `json:"removed,omitempty"`
	Launch   execution.LaunchEvidence `json:"launch,omitempty"`
	// slot is the bookkeeping of the creation requests issued for the object (see
	// slot): announced durably before the engine is asked.
	slot
	// ProbeSlot is the bookkeeping of a service's tcp-probe container.
	ProbeSlot *probeSlot `json:"probeSlot,omitempty"`
	// ObjectID pins the engine identity once the object exists: the network ID,
	// the volume's creation time or the container ID. A replacement created under
	// the same name by anyone else differs from it.
	ObjectID string `json:"objectId,omitempty"`
	// Created is true once the engine object was created by this adapter.
	Created bool       `json:"created,omitempty"`
	ReadyAt *time.Time `json:"readyAt,omitempty"`
	// Probe is a service's readiness contract, kept so Observe needs only the
	// handle.
	Probe *probeRec `json:"probe,omitempty"`
}

// slot is the bookkeeping of the creation requests issued for one engine object.
// Time never turns uncertainty into evidence: a request whose answer was not seen
// (it errored, was cancelled, or the controller died after announcing it) may
// still be completed by the engine, and the resource stays unresolved while one is
// outstanding: until the object is found and adopted, or an operator explicitly
// retires the request (see RetireUnresolvedRequest). The adapter creates no
// placeholder to settle it and infers nothing from clocks.
//
// At most one request is outstanding for a name: while one is, no other is issued.
type slot struct {
	// IssuedAt is when the latest request was announced.
	IssuedAt *time.Time `json:"issuedAt,omitempty"`
	Issues   int        `json:"issues,omitempty"`
	// Epoch is the operator-retirement epoch the request was announced under: a
	// retirement applies only to requests announced before it.
	Epoch int `json:"epoch,omitempty"`
	// Unanswered counts the requests whose answer was not seen (at most one).
	Unanswered int `json:"unanswered,omitempty"`
}

func (s *slot) announce(now time.Time, epoch int) {
	s.Issues++
	s.Unanswered++
	s.IssuedAt = &now
	s.Epoch = epoch
}

// answered: the outstanding request's object was bound (its answer, or the object
// found by name).
func (s *slot) answered() {
	if s.Unanswered > 0 {
		s.Unanswered--
	}
}

// retire: an operator retired the outstanding request.
func (s *slot) retire() { s.Unanswered = 0 }

// issued reports that a creation request was ever announced.
func (s slot) issued() bool { return s.Issues > 0 }

// outstanding reports that a request may still be completed by the engine.
func (s slot) outstanding() bool { return s.Unanswered > 0 }

// probeSlot is the durable identity of a service's tcp-probe container: the
// deterministic name serializes probes, Nonce (random, per service) marks the
// requests of this record, and ID binds the container once its creation was
// answered. A container is deleted only by the ID bound here; an outstanding
// creation is resolved only by the name together with the nonce.
type probeSlot struct {
	slot
	Nonce string `json:"nonce"`
	ID    string `json:"id,omitempty"`
}

// probeRec is what a record keeps of a service's readiness contract.
type probeRec struct {
	Kind        string   `json:"kind"`
	Host        string   `json:"host,omitempty"`
	Port        int64    `json:"port,omitempty"`
	Exec        []string `json:"exec,omitempty"`
	TimeoutSecs int64    `json:"timeoutSeconds,omitempty"`
}

func (r resRecord) handle() execution.ResourceHandle {
	return execution.ResourceHandle{Scope: r.Scope, RunID: r.RunID, Kind: r.Kind, Key: r.Key, Name: r.Name, UID: r.UID}
}

// fenced reports that the resource may never be created or started again.
func (r resRecord) fenced() bool { return r.Stopping || r.Stopped || r.Removed }

// owns reports whether the record is the resource a handle names: scope, run,
// kind and key, and (when bound) the instance UID. A tombstone of a lost record
// that never had a UID accepts any.
func (r resRecord) owns(h execution.ResourceHandle) bool {
	if r.Scope != h.Scope || r.RunID != h.RunID || r.Kind != h.Kind || r.Key != h.Key || r.Name != h.Name {
		return false
	}
	return h.UID == "" || h.UID == r.UID || (r.Untracked && r.UID == "")
}

// tombstone is the record a stop or remove leaves for a resource nobody
// recorded: it keeps the identity the caller supplied and never claims the
// resource was not started.
func tombstone(h execution.ResourceHandle, removed bool) resRecord {
	return resRecord{
		Name: h.Name, Scope: h.Scope, RunID: h.RunID, Kind: h.Kind, Key: h.Key, UID: h.UID, Untracked: true,
		Stopping: true, Stopped: true, Removed: removed, Launch: execution.LaunchUnknown,
	}
}

// resStore is the on-disk state of the run resources under StateDir/resources.
type resStore struct{ root string }

func newResStore(stateDir string) (*resStore, error) {
	s := &resStore{root: filepath.Join(stateDir, "resources")}
	return s, os.MkdirAll(s.root, 0o700)
}

func (s *resStore) dir() string                  { return s.root }
func (s *resStore) resDir(name string) string    { return filepath.Join(s.root, name) }
func (s *resStore) file(name string) string      { return filepath.Join(s.root, name, "record.json") }
func (s *resStore) bootstrap(name string) string { return filepath.Join(s.root, name, "bootstrap") }

// load reads the record of name; found is false when there is none.
func (s *resStore) load(name string) (resRecord, bool, error) {
	raw, err := os.ReadFile(s.file(name))
	if errors.Is(err, fs.ErrNotExist) {
		return resRecord{}, false, nil
	}
	if err != nil {
		return resRecord{}, false, fmt.Errorf("%w: reading the record of %s: %w", execution.ErrUnavailable, name, err)
	}
	var rec resRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Name != name {
		return resRecord{}, false, fmt.Errorf("%w: the record of %s is unreadable", execution.ErrUnavailable, name)
	}
	return rec, true, nil
}

// create durably writes a new record and fails with fs.ErrExist when one
// exists: it is linked into place, which never replaces a file.
func (s *resStore) create(rec resRecord) error { return s.write(rec, os.Link) }

// save durably replaces the record.
func (s *resStore) save(rec resRecord) error { return s.write(rec, os.Rename) }

func (s *resStore) write(rec resRecord, place func(from, to string) error) error {
	dir := s.resDir(rec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeDurably(dir, s.file(rec.Name), raw, place)
}

// writeDurably writes data to a temporary file in dir, syncs it, places it at
// file and syncs dir.
func writeDurably(dir, file string, data []byte, place func(from, to string) error) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := place(tmp.Name(), file); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// shareWithWorkload makes the files of a read-only bootstrap mount readable by
// the container's non-root workload user. On a Linux host a rootless engine maps
// files owned by the host user to the container's root, which a non-root user
// cannot read through mode 0600; the material is therefore 0644 in a 0755
// directory. Its confidentiality rests on the state directory and its ancestors
// (0700, created by this package) and on the mount being private to one
// workload, not on file modes.
func shareWithWorkload(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() {
			if err := os.Chmod(filepath.Join(dir, e.Name()), 0o644); err != nil {
				return err
			}
		}
	}
	return os.Chmod(dir, 0o755)
}
