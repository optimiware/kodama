package process

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	kr "github.com/optimiware/kodama/internal/kernel/runtime"
)

// The kernel adapter keeps its state on disk under one dedicated directory so a
// restarted adapter finds what the previous one created:
//
//	records/TYPE-KEY.json   the durable intent, fence and outcome of one
//	                        workload, workspace or service; written BEFORE the
//	                        object exists and kept as a tombstone after it is
//	                        removed, so a stopped or removed identity is never
//	                        started again
//	holders/WSKEY/WLKEY.json  a workload that holds a run workspace and the
//	                        process group it was recorded under, written before
//	                        the workload is let run
//	TYPEs/KEY/              the object's own files (work/, output.log,
//	                        workspace/, .owner)
//
// KEY is a digest of the tenant, project, type and stable identity, so another
// tenant's identical identifier is a different object and never observable.

const (
	typeWorkload  = "workload"
	typeWorkspace = "workspace"
	typeService   = "service"

	kRecordsDir = "records"
	kHoldersDir = "holders"
)

// kRecord is the durable state of one workload, workspace or service.
type kRecord struct {
	Type    string        `json:"type"`
	Key     string        `json:"key"`
	Scope   domain.Scope  `json:"scope"`
	RunID   domain.RunID  `json:"runId"`
	StepKey domain.StepID `json:"stepKey,omitempty"`
	Attempt int           `json:"attempt,omitempty"`
	// ID is the workload id or the resource identity; Name is the resource name.
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Service is a service's own name, how steps of the run reach it.
	Service string `json:"service,omitempty"`
	// Digest is the immutable specification identity.
	Digest string `json:"digest,omitempty"`
	// Fence is the highest writer fence that has operated on the object.
	Fence kr.Fence `json:"fence"`
	// UID identifies this instance and is also written next to a workspace.
	UID string `json:"uid,omitempty"`
	// Untracked marks a tombstone left by a stop or remove of an object nobody
	// recorded: it fences a late create and proves nothing about launch.
	Untracked bool `json:"untracked,omitempty"`
	// Stopping is the stop intent (the fence on starting); Stopped the
	// verified stop.
	Stopping bool `json:"stopping,omitempty"`
	Stopped  bool `json:"stopped,omitempty"`
	Removed  bool `json:"removed,omitempty"`
	// Owned is true once the object's directory creation was issued.
	Owned    bool   `json:"owned,omitempty"`
	Created  bool   `json:"created,omitempty"`
	Prepared bool   `json:"prepared,omitempty"`
	FsDev    uint64 `json:"fsDev,omitempty"`
	FsIno    uint64 `json:"fsIno,omitempty"`
	// IssuedAt is when a launch was announced, before any process existed. Pgid
	// and StartID identify the launched process group once recorded, before the
	// launch gate opened.
	IssuedAt *time.Time `json:"issuedAt,omitempty"`
	Pgid     int        `json:"pgid,omitempty"`
	StartID  string     `json:"startId,omitempty"`
	// ExitKnown and the fields after it record how the leader ended, once.
	ExitKnown bool  `json:"exitKnown,omitempty"`
	ExitCode  int   `json:"exitCode,omitempty"`
	TimedOut  bool  `json:"timedOut,omitempty"`
	Cancelled bool  `json:"cancelled,omitempty"`
	Ports     []int `json:"ports,omitempty"`
}

// fenced reports that the object may never be created or started again.
func (r kRecord) fenced() bool { return r.Stopping || r.Stopped || r.Removed }

// kstore is the on-disk state under one directory.
type kstore string

// objectKey derives the key that namespaces an identity by tenant and project.
func objectKey(scope domain.Scope, typ, id string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{string(scope.TenantID), string(scope.ProjectID), typ, id}, "\x00")))
	return hex.EncodeToString(sum[:])[:40]
}

func (s kstore) recordPath(typ, key string) string {
	return filepath.Join(string(s), kRecordsDir, typ+"-"+key+".json")
}
func (s kstore) dir(typ, key string) string { return filepath.Join(string(s), typ+"s", key) }
func (s kstore) workspaceDir(key string) string {
	return filepath.Join(s.dir(typeWorkspace, key), workspaceSub)
}
func (s kstore) ownerPath(typ, key string) string { return filepath.Join(s.dir(typ, key), ownerFile) }
func (s kstore) logPath(typ, key string) string   { return filepath.Join(s.dir(typ, key), "output.log") }

// lock serializes the operations of one object across every adapter of this
// process on one state directory.
func (s kstore) lock(typ, key string) func() { return store(s).lockName(typ + "-" + key) }

func (s kstore) load(typ, key string) (kRecord, bool, error) {
	raw, err := os.ReadFile(s.recordPath(typ, key))
	if errors.Is(err, fs.ErrNotExist) {
		return kRecord{}, false, nil
	}
	if err != nil {
		return kRecord{}, false, fmt.Errorf("process: reading the record of %s %s: %w", typ, key, err)
	}
	var rec kRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Key != key || rec.Type != typ {
		return kRecord{}, false, fmt.Errorf("process: the record of %s %s is unreadable", typ, key)
	}
	return rec, true, nil
}

// create durably writes a new record and fails with fs.ErrExist when one
// exists: it is linked into place, which never replaces a file.
func (s kstore) create(rec kRecord) error { return s.write(rec, os.Link) }

// save durably replaces the record.
func (s kstore) save(rec kRecord) error { return s.write(rec, os.Rename) }

func (s kstore) write(rec kRecord, place func(from, to string) error) error {
	dir := filepath.Join(string(s), kRecordsDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return writeFileDurably(dir, s.recordPath(rec.Type, rec.Key), raw, place)
}

func (s kstore) deleteRecord(typ, key string) error {
	err := os.Remove(s.recordPath(typ, key))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// list returns every readable record, for reconciliation.
func (s kstore) list() ([]kRecord, error) {
	entries, err := os.ReadDir(filepath.Join(string(s), kRecordsDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []kRecord
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		typ, key, ok := strings.Cut(strings.TrimSuffix(name, ".json"), "-")
		if !ok {
			continue
		}
		rec, found, err := s.load(typ, key)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	return out, nil
}

// dirExists reports whether the object's directory exists; an error other than
// absence is undeterminable.
func (s kstore) dirExists(typ, key string) (bool, error) {
	_, err := os.Lstat(s.dir(typ, key))
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, fmt.Errorf("process: inspecting %s %s: %w", typ, key, err)
}

// makeDir creates the object's directory once with its owner marker already
// inside: it is built under a temporary name and renamed into place, so a
// directory at the name either holds our marker or is not ours.
func (s kstore) makeDir(rec kRecord) error {
	exists, err := s.dirExists(rec.Type, rec.Key)
	if err != nil {
		return err
	}
	if exists {
		return s.checkOwner(rec)
	}
	parent := filepath.Dir(s.dir(rec.Type, rec.Key))
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".new-*")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(tmp, ownerFile), []byte(rec.UID), 0o600); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, s.dir(rec.Type, rec.Key)); err != nil {
		_ = os.RemoveAll(tmp)
		return err
	}
	return nil
}

// checkOwner verifies that an existing directory carries the record's owner
// marker; anything else is a collision and is never deleted.
func (s kstore) checkOwner(rec kRecord) error {
	raw, err := os.ReadFile(s.ownerPath(rec.Type, rec.Key))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return errCollision
	case err != nil:
		return fmt.Errorf("process: reading the owner marker: %w", err)
	case strings.TrimSpace(string(raw)) != rec.UID:
		return errCollision
	}
	return nil
}

// removeOwned deletes the directory the record created after verifying it is
// that directory, and confirms its absence.
func (s kstore) removeOwned(rec kRecord) error {
	exists, err := s.dirExists(rec.Type, rec.Key)
	if err != nil || !exists {
		return err
	}
	if err := s.checkOwner(rec); err != nil {
		return err
	}
	if err := os.RemoveAll(s.dir(rec.Type, rec.Key)); err != nil {
		return fmt.Errorf("process: removing %s %s: %w", rec.Type, rec.ID, err)
	}
	if exists, err := s.dirExists(rec.Type, rec.Key); err != nil {
		return err
	} else if exists {
		return fmt.Errorf("process: %s %s still exists", rec.Type, rec.ID)
	}
	return nil
}

// verifyWorkspace checks that the workspace directory is the instance the
// record pinned: same device and inode, a directory (never a symlink), and the
// owner marker holding the instance UID. A replacement is a collision; a
// verifiably missing directory is lost scratch (wsMissing), never recreated.
func (s kstore) verifyWorkspace(rec kRecord) (wsState, error) {
	fi, err := os.Lstat(s.workspaceDir(rec.Key))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return wsMissing, nil
	case err != nil:
		return wsPresent, fmt.Errorf("process: inspecting the workspace: %w", err)
	case !fi.IsDir():
		return wsPresent, errCollision
	}
	dev, ino, err := fsIdentity(fi)
	if err != nil {
		return wsPresent, err
	}
	if dev != rec.FsDev || ino != rec.FsIno {
		return wsPresent, errCollision
	}
	return wsPresent, s.checkOwner(rec)
}

// ---- workspace holders ----

// kholder is the marker of a workload holding a run workspace.
type kholder struct {
	WorkloadID  string `json:"workloadId"`
	WorkloadKey string `json:"workloadKey"`
	Pgid        int    `json:"pgid"`
	StartID     string `json:"startId"`
}

func (s kstore) holderFile(wsKey, wlKey string) string {
	return filepath.Join(string(s), kHoldersDir, wsKey, wlKey+".json")
}

func (s kstore) saveHolder(wsKey string, h kholder) error {
	file := s.holderFile(wsKey, h.WorkloadKey)
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return writeFileDurably(dir, file, raw, os.Rename)
}

func (s kstore) dropHolder(wsKey, wlKey string) { _ = os.Remove(s.holderFile(wsKey, wlKey)) }

// holder returns the live workload holding the workspace, if any. A marker is
// held while its recorded process group lives; a marker without a group cannot
// be one that runs (see the launch gate). An undeterminable marker or group is
// an error, never "released".
func (s kstore) holder(wsKey string, probe groupProbe) (kholder, bool, error) {
	dir := filepath.Join(string(s), kHoldersDir, wsKey)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return kholder{}, false, nil
	}
	if err != nil {
		return kholder{}, false, fmt.Errorf("process: reading the holders of %s: %w", wsKey, err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		var h kholder
		if err != nil || json.Unmarshal(raw, &h) != nil {
			return kholder{}, false, fmt.Errorf("process: a holder marker of %s is unreadable", wsKey)
		}
		if h.Pgid == 0 {
			continue
		}
		alive, err := probe.alive(h.Pgid, h.StartID, false)
		if err != nil {
			return kholder{}, false, err
		}
		if alive {
			return h, true, nil
		}
	}
	return kholder{}, false, nil
}

// ---- process group probe ----

// errUnverifiable reports a process group that exists but cannot be examined:
// it can be neither signalled nor shown to be gone.
var errUnverifiable = errors.New("process: the process group cannot be verified")

// groupProbe is how the adapter looks at a process group. It is a value so tests
// can inject what the operating system answers.
type groupProbe struct {
	// signal sends sig to the process group (signal 0 probes it).
	signal func(pgid int, sig syscall.Signal) error
	// leader reports the start time of the process pgid, and gone when no such
	// process exists.
	leader func(pid int) (start string, gone bool, err error)
	// members counts the processes of the group that have not terminated
	// (zombies awaiting reaping are not counted), whatever their credentials.
	members func(pgid int) (int, error)
}

var osProbe = groupProbe{
	signal:  func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) },
	leader:  startOf,
	members: liveMembers,
}

// liveMembers lists the live members of a process group from the process table,
// which shows processes of every user.
func liveMembers(pgid int) (int, error) {
	out, err := exec.Command("ps", "-A", "-o", "pgid=,stat=").Output()
	if err != nil {
		return 0, fmt.Errorf("ps: %w", err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if id, err := strconv.Atoi(f[0]); err == nil && id == pgid && !strings.HasPrefix(f[1], "Z") {
			n++
		}
	}
	return n, nil
}

// alive reports whether a process of the group still exists, with an answer only
// when the group is positively seen or positively gone:
//
//   - no such group is gone;
//   - a group that can be signalled is alive, and (for a group this adapter did
//     not launch, ours false) it is the recorded one only while its leader has
//     the recorded start time: another leader is a recycled pid, so the original
//     group is gone, and a group whose leader ended still has members;
//   - a group that exists but permission is denied for is NOT thereby proven
//     terminated: a descendant that changed its credentials looks exactly so
//     (and some systems answer so for a group of zombies). It is gone only on
//     independent evidence: its leader's pid is held by a process with a
//     different start time (the id was recycled, which cannot happen while a
//     member lives), or the process table shows no live member. It is alive when
//     a live member exists, and unverifiable (an error, never absence) when the
//     process table cannot be read.
func (p groupProbe) alive(pgid int, startID string, ours bool) (bool, error) {
	if pgid <= 1 {
		return false, nil
	}
	err := p.signal(pgid, 0)
	switch {
	case err == nil:
		if ours {
			return true, nil
		}
		start, gone, err := p.leader(pgid)
		switch {
		case err != nil:
			return false, fmt.Errorf("%w: examining the leader of group %d: %w", errUnverifiable, pgid, err)
		case gone:
			return true, nil // the leader ended; members of its group remain
		}
		return startID == "" || start == startID, nil // another leader: a recycled pid
	case errors.Is(err, syscall.ESRCH):
		return false, nil
	case errors.Is(err, syscall.EPERM):
		return p.deniedEvidence(pgid, startID)
	}
	return false, fmt.Errorf("%w: probing process group %d: %w", errUnverifiable, pgid, err)
}

// deniedEvidence decides about a group the system will not let us signal.
func (p groupProbe) deniedEvidence(pgid int, startID string) (bool, error) {
	start, gone, err := p.leader(pgid)
	if err != nil {
		return false, fmt.Errorf("%w: examining the leader of group %d: %w", errUnverifiable, pgid, err)
	}
	if !gone && startID != "" && start != startID {
		return false, nil // the pid is another process: the original group is gone
	}
	n, err := p.members(pgid)
	if err != nil {
		return false, fmt.Errorf("%w: group %d denies permission and the process table is unreadable: %w", errUnverifiable, pgid, err)
	}
	return n > 0, nil
}
