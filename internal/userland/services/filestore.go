package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/optimiware/kodama/internal/id"
)

// File-store locking bounds. The in-process mutex serializes this process; the
// advisory lock on the open lock file is the cross-process guard. The operating
// system releases that lock when its holder exits, so a crashed writer never
// strands a record and a slow writer is never mistaken for a dead one.
const (
	fileLockWait         = 5 * time.Second
	fileLockPollInterval = 5 * time.Millisecond
)

// fileStoreLocks serializes writers per record path within one process.
var fileStoreLocks sync.Map

// NewFileCoordinationStore creates the coordination directory (and parents)
// when missing and returns a store over it.
func NewFileCoordinationStore(directory string) (*FileCoordinationStore, error) {
	if directory == "" {
		return nil, fmt.Errorf("coordination store directory is empty")
	}
	cleaned := filepath.Clean(directory)
	if err := os.MkdirAll(cleaned, 0o700); err != nil {
		return nil, fmt.Errorf("create coordination store directory: %w", err)
	}
	absolute, err := filepath.Abs(cleaned)
	if err != nil {
		return nil, fmt.Errorf("resolve coordination store directory: %w", err)
	}
	return &FileCoordinationStore{directory: absolute, syncDir: syncDirectory}, nil
}

// Load reads one tenant's record for the key. A missing record is (zero, false,
// nil).
func (store *FileCoordinationStore) Load(ctx context.Context, tenant id.TenantID, key MergeKey) (CoordinationRecord, bool, error) {
	if err := validateCoordinationScope(tenant, key); err != nil {
		return CoordinationRecord{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return CoordinationRecord{}, false, err
	}
	return store.load(store.recordPath(tenant, key), tenant, key)
}

// CreateClaim atomically creates the next generation when the stored record is
// missing, released or expired at now. A live record returns ErrClaimed.
func (store *FileCoordinationStore) CreateClaim(ctx context.Context, tenant id.TenantID, key MergeKey, owner string, operationID id.OperationID, expiresAt, now time.Time) (CoordinationRecord, error) {
	if err := validateCoordinationScope(tenant, key); err != nil {
		return CoordinationRecord{}, err
	}
	if owner == "" {
		return CoordinationRecord{}, fmt.Errorf("%w: claim owner is empty", ErrClaimed)
	}
	if _, err := id.ParseOperationID(string(operationID)); err != nil {
		return CoordinationRecord{}, fmt.Errorf("claim operation identity %q is invalid: %w", operationID, err)
	}
	if expiresAt.IsZero() || now.IsZero() {
		return CoordinationRecord{}, fmt.Errorf("claim expiry and claim time are required")
	}
	// JSON cannot carry a monotonic reading or a process-local zone, so write
	// the canonical instant form and return the same form to the caller.
	expiresAt = normalizedTime(expiresAt)
	now = normalizedTime(now)
	if err := ctx.Err(); err != nil {
		return CoordinationRecord{}, err
	}
	path := store.recordPath(tenant, key)
	var created CoordinationRecord
	err := store.withLock(path, func() error {
		previous, ok, err := store.load(path, tenant, key)
		if err != nil {
			return err
		}
		if ok && !previous.Released && !previous.Expired(now) {
			return fmt.Errorf("%w: owned by %q", ErrClaimed, previous.Owner)
		}
		next := CoordinationRecord{
			Claim: Claim{
				Key:         key,
				Owner:       owner,
				OperationID: operationID,
				Generation:  previous.Claim.Generation + 1,
				ExpiresAt:   expiresAt,
			},
			TenantID:  tenant,
			State:     EffectNotStarted,
			UpdatedAt: now,
		}
		if ok && unsettledRecord(previous) {
			next.Claim.OperationID = previous.Claim.OperationID
			next.State = previous.State
			next.Request = previous.Request
			next.Result = previous.Result
			next.Reconciled = previous.Reconciled
		}
		if err := next.Validate(); err != nil {
			return err
		}
		if err := store.write(path, next); err != nil {
			return err
		}
		created = next
		return nil
	})
	if err != nil {
		return CoordinationRecord{}, err
	}
	return created, nil
}

// Transition atomically replaces expected with next only when the stored record
// still equals expected and next keeps the same tenant, key, owner, operation
// identity and generation.
func (store *FileCoordinationStore) Transition(ctx context.Context, tenant id.TenantID, expected, next CoordinationRecord) error {
	if err := validateCoordinationScope(tenant, expected.Claim.Key); err != nil {
		return err
	}
	if next.TenantID != tenant ||
		next.Claim.Key != expected.Claim.Key ||
		next.Claim.Owner != expected.Claim.Owner ||
		next.Claim.OperationID != expected.Claim.OperationID ||
		next.Claim.Generation != expected.Claim.Generation {
		return fmt.Errorf("%w: transition changes tenant, key, owner, operation or generation", ErrStaleClaim)
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path := store.recordPath(tenant, expected.Claim.Key)
	return store.withLock(path, func() error {
		stored, ok, err := store.load(path, tenant, expected.Claim.Key)
		if err != nil {
			return err
		}
		if !ok || !recordsEqual(stored, expected) {
			return fmt.Errorf("%w: stored record changed", ErrStaleClaim)
		}
		return store.write(path, next)
	})
}

// recordPath derives a fixed-length, traversal-free file name for one
// (tenant, repository, ref) record.
func (store *FileCoordinationStore) recordPath(tenant id.TenantID, key MergeKey) string {
	sum := sha256.Sum256([]byte(string(tenant) + "\x00" + key.Repository + "\x00" + key.Ref))
	return filepath.Join(store.directory, hex.EncodeToString(sum[:])+".json")
}

// load reads and validates one record file.
func (store *FileCoordinationStore) load(path string, tenant id.TenantID, key MergeKey) (CoordinationRecord, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return CoordinationRecord{}, false, nil
	}
	if err != nil {
		return CoordinationRecord{}, false, fmt.Errorf("read coordination record: %w", err)
	}
	var record CoordinationRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return CoordinationRecord{}, false, fmt.Errorf("decode coordination record: %w", err)
	}
	if record.TenantID != tenant || record.Claim.Key != key {
		return CoordinationRecord{}, false, fmt.Errorf("coordination record scope does not match its slot")
	}
	record = normalizeRecord(record)
	if err := record.Validate(); err != nil {
		return CoordinationRecord{}, false, fmt.Errorf("invalid coordination record: %w", err)
	}
	return record, true, nil
}

// write replaces one record atomically: a complete temporary file is renamed
// over the target so a reader never sees a partial record.
func (store *FileCoordinationStore) write(path string, record CoordinationRecord) error {
	record = normalizeRecord(record)
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(store.directory, ".record-*")
	if err != nil {
		return fmt.Errorf("create coordination record: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace coordination record: %w", err)
	}
	// The rename is visible to readers but not yet durable: flush the directory
	// entry so a crash cannot revert it. A failure here means the caller must
	// treat the write as not durable rather than as a committed record.
	if err := store.syncStoreDirectory(); err != nil {
		return fmt.Errorf("make coordination record durable: %w", err)
	}
	return nil
}

// syncStoreDirectory flushes the store directory so a published record survives
// a crash.
func (store *FileCoordinationStore) syncStoreDirectory() error {
	return store.syncDirectory(store.directory)
}

// syncDirectory flushes one directory. A nil injection uses the real flush.
func (store *FileCoordinationStore) syncDirectory(path string) error {
	if store.syncDir != nil {
		return store.syncDir(path)
	}
	return syncDirectory(path)
}

// syncDirectory fsyncs one directory so a rename or a newly created entry
// survives a crash.
func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open coordination store directory: %w", err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return fmt.Errorf("sync coordination store directory: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close coordination store directory: %w", closeErr)
	}
	return nil
}

// withLock serializes writers per record path in this process and with the
// advisory lock on the record's lock file across processes.
func (store *FileCoordinationStore) withLock(path string, action func() error) error {
	value, _ := fileStoreLocks.LoadOrStore(path, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	release, err := acquireFileLock(path+".lock", store.syncDirectory)
	if err != nil {
		return err
	}
	defer release()
	return action()
}

// acquireFileLock takes the exclusive advisory lock on the lock file. The file
// is opened (never unlinked) and held with flock(2) on its open descriptor: the
// operating system releases it when the process dies, and no wall-clock age can
// transfer it to a second writer. A nil syncDirectory uses the real directory
// flush. It returns the release function.
func acquireFileLock(lockPath string, syncDir func(string) error) (func(), error) {
	if syncDir == nil {
		syncDir = syncDirectory
	}
	file, created, err := openLockFile(lockPath)
	if err != nil {
		return nil, fmt.Errorf("open coordination lock: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("set coordination lock mode: %w", err)
	}
	if created {
		// The lock file's first creation must survive a crash too. The caller
		// may proceed only when the directory entry is durable.
		if err := syncDir(filepath.Dir(lockPath)); err != nil {
			file.Close()
			return nil, fmt.Errorf("make coordination lock durable: %w", err)
		}
	}
	deadline := time.Now().Add(fileLockWait)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			file.Close()
			return nil, fmt.Errorf("acquire coordination lock: %w", err)
		}
		if time.Now().After(deadline) {
			file.Close()
			return nil, fmt.Errorf("coordination record %s is locked by another writer", lockPath)
		}
		time.Sleep(fileLockPollInterval)
	}
	released := false
	return func() {
		if released {
			return
		}
		released = true
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

// openLockFile opens the lock file and reports whether this call created it, so
// its first creation can be made durable. The file is never unlinked: removing
// it would let a waiter that already opened the old inode and a fresh contender
// believe they own the same record.
func openLockFile(lockPath string) (*os.File, bool, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		return file, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, err
	}
	file, err = os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return nil, false, err
	}
	return file, false, nil
}

// Scan enumerates every record of one tenant. It is the coordinator's
// convergence path for an assignment whose request resolution committed but
// whose response was lost.
func (store *FileCoordinationStore) Scan(ctx context.Context, tenant id.TenantID) ([]CoordinationRecord, error) {
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return nil, fmt.Errorf("coordination tenant %q is invalid: %w", tenant, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return nil, fmt.Errorf("read coordination directory: %w", err)
	}
	records := make([]CoordinationRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(store.directory, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read coordination record: %w", err)
		}
		var record CoordinationRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, fmt.Errorf("decode coordination record: %w", err)
		}
		if record.TenantID != tenant {
			continue
		}
		record = normalizeRecord(record)
		if err := record.Validate(); err != nil {
			return nil, fmt.Errorf("invalid coordination record: %w", err)
		}
		records = append(records, record)
	}
	sort.Slice(records, func(first, second int) bool {
		if records[first].Claim.Key != records[second].Claim.Key {
			return records[first].Claim.Key.Repository < records[second].Claim.Key.Repository ||
				(records[first].Claim.Key.Repository == records[second].Claim.Key.Repository &&
					records[first].Claim.Key.Ref < records[second].Claim.Key.Ref)
		}
		if records[first].Claim.Generation != records[second].Claim.Generation {
			return records[first].Claim.Generation < records[second].Claim.Generation
		}
		return records[first].Claim.OperationID < records[second].Claim.OperationID
	})
	return records, nil
}

// validateCoordinationScope refuses an unusable tenant or merge key before any
// file is touched.
func validateCoordinationScope(tenant id.TenantID, key MergeKey) error {
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return fmt.Errorf("coordination tenant %q is invalid: %w", tenant, err)
	}
	return key.Validate()
}

// unsettledRecord reports whether a stored record holds an assignment that may
// have reached the outside world and must therefore keep its operation identity
// and result until it is settled. An applied but unresolved assignment is
// unsettled: releasing it would discard the only replay protection.
func unsettledRecord(record CoordinationRecord) bool {
	if record.TenantID == "" || record.Released {
		return false
	}
	if record.State == EffectUncertain || record.State == EffectApplied {
		return true
	}
	return record.State == EffectNotStarted && record.Request.Key != (MergeKey{})
}

// compile-time assertions that the file store satisfies the declared port and the
// coordinator's optional enumeration capability.
var (
	_ CoordinationStore   = (*FileCoordinationStore)(nil)
	_ CoordinationScanner = (*FileCoordinationStore)(nil)
)
