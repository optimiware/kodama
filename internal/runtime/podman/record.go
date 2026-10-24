package podman

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/runtime/bootstrap"
	"github.com/optimiware/kodama/internal/runtimeauth"
)

var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// record is the durable state of one workload. It is written before any
// container exists and survives the container, as a tombstone, so that a
// stopped or removed workload is never started again.
type record struct {
	Name          string                   `json:"name"`
	Scope         domain.Scope             `json:"scope"`
	AttemptID     domain.AttemptID         `json:"attemptId"`
	UID           string                   `json:"uid,omitempty"`
	Fingerprint   string                   `json:"fingerprint,omitempty"`
	ContextDigest string                   `json:"contextDigest,omitempty"`
	ContainerID   string                   `json:"containerId,omitempty"`
	Stopped       bool                     `json:"stopped,omitempty"`
	Removed       bool                     `json:"removed,omitempty"`
	Launch        execution.LaunchEvidence `json:"launch,omitempty"`
}

func (r record) handle() execution.Workload {
	return execution.Workload{Scope: r.Scope, AttemptID: r.AttemptID, Name: r.Name, UID: r.UID}
}

func (r record) matches(h execution.Workload) error {
	if r.Scope != h.Scope || r.AttemptID != h.AttemptID || (h.UID != "" && r.UID != "" && h.UID != r.UID) {
		return fmt.Errorf("%w: the workload is another attempt's or instance's", execution.ErrConflict)
	}
	return nil
}

func evidence(r record) runtimeauth.JobEvidence { return bootstrap.Evidence(Namespace, r.handle()) }

// records stores one directory per workload under the state directory: the
// record and the files mounted into its container.
type records string

func (r *Runtime) records() records { return records(r.cfg.StateDir) }

func (s records) dir(name string) string  { return filepath.Join(string(s), name) }
func (s records) file(name string) string { return filepath.Join(string(s), name, "record.json") }

func (s records) load(name string) (record, error) {
	raw, err := os.ReadFile(s.file(name))
	if errors.Is(err, fs.ErrNotExist) {
		return record{}, fmt.Errorf("%w: no workload %s", execution.ErrNotFound, name)
	}
	if err != nil {
		return record{}, err
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Name != name {
		return record{}, fmt.Errorf("%w: the record of %s is unreadable", execution.ErrUnavailable, name)
	}
	return rec, nil
}

// begin returns the record of the spec's workload, creating it with a fresh
// instance UID. A workload stopped or removed, or recorded for another spec,
// is a conflict.
func (s records) begin(spec execution.AttemptSpec) (record, error) {
	rec, err := s.load(spec.WorkloadName)
	if errors.Is(err, execution.ErrNotFound) {
		rec = record{
			Name: spec.WorkloadName, Scope: spec.Scope, AttemptID: spec.AttemptID, UID: newUID(),
			Fingerprint: spec.SpecFingerprint, ContextDigest: spec.ContextDigest,
		}
		err = s.create(rec)
		if errors.Is(err, fs.ErrExist) {
			return s.begin(spec) // a concurrent start recorded it first
		}
		return rec, err
	}
	switch {
	case err != nil:
		return record{}, err
	case rec.Stopped || rec.Removed:
		return record{}, fmt.Errorf("%w: the workload was stopped and is never started again", execution.ErrConflict)
	case rec.Scope != spec.Scope || rec.AttemptID != spec.AttemptID || rec.Fingerprint != spec.SpecFingerprint || rec.ContextDigest != spec.ContextDigest:
		return record{}, fmt.Errorf("%w: the workload name is recorded for another spec", execution.ErrConflict)
	}
	return rec, nil
}

// stop marks the workload stopped, leaving a tombstone when it was never
// recorded.
func (s records) stop(h execution.Workload) (record, error) {
	rec, err := s.load(h.Name)
	if errors.Is(err, execution.ErrNotFound) {
		rec = record{Name: h.Name, Scope: h.Scope, AttemptID: h.AttemptID, Stopped: true, Launch: execution.LaunchNotStarted}
		if err := s.create(rec); !errors.Is(err, fs.ErrExist) {
			return rec, err
		}
		return s.stop(h)
	}
	if err != nil {
		return record{}, err
	}
	if err := rec.matches(h); err != nil {
		return record{}, err
	}
	if !rec.Stopped {
		rec.Stopped = true
		err = s.save(rec)
	}
	return rec, err
}

// remove keeps the record as a removed tombstone and deletes everything else
// of the workload.
func (s records) remove(rec record) error {
	rec.Removed = true
	if err := s.save(rec); err != nil {
		return err
	}
	entries, err := os.ReadDir(s.dir(rec.Name))
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "record.json" {
			if err := os.RemoveAll(filepath.Join(s.dir(rec.Name), e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// create durably writes a new record and fails with fs.ErrExist when one
// exists: it is linked into place, which never replaces a file.
func (s records) create(rec record) error {
	return s.write(rec, os.Link)
}

// save durably replaces the record.
func (s records) save(rec record) error {
	return s.write(rec, os.Rename)
}

func (s records) write(rec record, place func(from, to string) error) error {
	dir := s.dir(rec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(raw)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := place(tmp.Name(), s.file(rec.Name)); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// newUID returns a random version 4 UUID.
func newUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
