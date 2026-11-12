package git

// Retained-commit keep refs.
//
// A rewinding `RW+` update moves a branch back and leaves the discarded
// commits unreachable; a later `git gc` would then prune them even though an
// execution's retained evidence still names them. This file keeps one direct
// ref per retained commit under refs/kodama/keep/<sha>, so the commit and its
// ancestry stay reachable until no retained execution names it.
//
// The mechanism is deliberately policy-free: the caller supplies the
// authoritative retained set (the Shell's retention store in production).
// It never invents a window, never treats an unavailable source as "nothing is
// retained", and only releases a keep ref when the supplied set omits its
// commit. Missing or malformed commit names are skipped, never blamed on the
// repository.

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/optimiware/kodama/internal/id"
)

// KeepRefNamespace is the whole managed namespace. Smart-HTTP advertisement
// hides this namespace so clients never see retention bookkeeping.
const KeepRefNamespace = "refs/kodama"

// KeepRefPrefix is the managed namespace for retained-commit refs. A ref here
// points directly at the commit named by its suffix.
const KeepRefPrefix = KeepRefNamespace + "/keep/"

// MaxKeepRefs bounds one sweep so a corrupt retention source cannot force an
// unbounded number of native commands.
const MaxKeepRefs = 4096

// KeepRefsResult is the evidence of one keep-ref sweep.
type KeepRefsResult struct {
	Repository string
	Kept       []string
	Released   []string
	Missing    []string
	Invalid    []string
}

// KeepRefStore manages retained-commit refs for one storage boundary. It runs
// native Git commands and holds no retention policy.
type KeepRefStore struct {
	storage  Storage
	executor CommandExecutor
}

// NewKeepRefStore validates the native boundaries.
func NewKeepRefStore(storage Storage, executor CommandExecutor) (*KeepRefStore, error) {
	if storage == nil {
		return nil, fmt.Errorf("keep refs require a storage boundary")
	}
	if executor == nil {
		return nil, fmt.Errorf("keep refs require a command executor")
	}
	return &KeepRefStore{storage: storage, executor: executor}, nil
}

// KeepRefs reconciles the keep refs of one tenant-scoped repository with the
// authoritative retained commit set. It creates or refreshes a ref for every
// retained commit that still exists as a commit and releases every other
// managed ref. Running it with the same set is idempotent: an already-correct
// ref is not written again.
func (store *KeepRefStore) KeepRefs(ctx context.Context, tenant id.TenantID, repository string, retained []string) (KeepRefsResult, error) {
	if ctx == nil {
		return KeepRefsResult{}, fmt.Errorf("keep ref sweep requires a context")
	}
	if err := ctx.Err(); err != nil {
		return KeepRefsResult{}, err
	}
	if _, err := id.ParseTenantID(string(tenant)); err != nil {
		return KeepRefsResult{}, fmt.Errorf("%w: tenant %q", ErrInvalidRepository, tenant)
	}
	path, err := store.storage.RepositoryPath(tenant, repository)
	if err != nil {
		return KeepRefsResult{}, err
	}
	result := KeepRefsResult{Repository: repository}

	candidates, invalid, err := retainedCommits(retained)
	if err != nil {
		return KeepRefsResult{}, err
	}
	result.Invalid = invalid

	wanted := make(map[string]bool, len(candidates))
	for _, commit := range candidates {
		exists, err := store.commitExists(ctx, path, commit)
		if err != nil {
			return KeepRefsResult{}, err
		}
		if !exists {
			result.Missing = append(result.Missing, commit)
			continue
		}
		wanted[commit] = true
	}

	existing, err := store.existingKeepRefs(ctx, path)
	if err != nil {
		return KeepRefsResult{}, err
	}

	// Create and refresh every wanted ref before releasing anything. A failed
	// create therefore leaves the previous keep set in place instead of
	// stripping protection from a commit that is still retained.
	kept := make([]string, 0, len(wanted))
	for commit := range wanted {
		kept = append(kept, commit)
	}
	sort.Strings(kept)
	for _, commit := range kept {
		ref := KeepRefPrefix + commit
		if current, ok := existing[ref]; ok && current == commit {
			result.Kept = append(result.Kept, commit)
			continue
		}
		if _, err := store.run(ctx, path, "update-ref", ref, commit); err != nil {
			return KeepRefsResult{}, err
		}
		result.Kept = append(result.Kept, commit)
	}
	for ref, commit := range existing {
		suffix := strings.TrimPrefix(ref, KeepRefPrefix)
		if wanted[suffix] {
			continue
		}
		// Release with a compare-and-swap on the observed object. A concurrent
		// writer that moved the ref is not silently overwritten; the next
		// sweep re-observes it.
		if _, err := store.run(ctx, path, "update-ref", "-d", ref, commit); err != nil {
			return KeepRefsResult{}, err
		}
		result.Released = append(result.Released, suffix)
	}
	sort.Strings(result.Released)
	sort.Strings(result.Missing)
	sort.Strings(result.Invalid)
	return result, nil
}

// commitExists reports whether the commit name resolves to a commit object.
// Exit 1 is a missing object; any other non-zero exit or executor failure is an
// uncertainty that aborts the sweep before a ref is released.
func (store *KeepRefStore) commitExists(ctx context.Context, path, commit string) (bool, error) {
	result, err := store.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "rev-parse", "--verify", "--quiet", commit + "^{commit}"},
	})
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		// Git uses exit 1 for "not a valid object name". The commit is simply
		// absent here (another repository may still have it), so it is skipped.
		return false, nil
	default:
		return false, fmt.Errorf("rev-parse keep commit %s exited %d: %s", commit, result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
}

// existingKeepRefs reads the managed refs. The format keeps the ref and its
// object separate so a concurrent move is detected by the delete CAS.
func (store *KeepRefStore) existingKeepRefs(ctx context.Context, path string) (map[string]string, error) {
	execution, err := store.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "for-each-ref", "--format=%(refname) %(objectname)", KeepRefPrefix},
	})
	if err != nil {
		return nil, err
	}
	if execution.ExitCode != 0 {
		return nil, fmt.Errorf("for-each-ref keep refs exited %d", execution.ExitCode)
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(string(execution.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("keep-ref line %q is malformed", line)
		}
		if !strings.HasPrefix(fields[0], KeepRefPrefix) || !validCommitID(strings.TrimPrefix(fields[0], KeepRefPrefix)) {
			return nil, fmt.Errorf("keep-ref %q is outside the managed namespace", fields[0])
		}
		refs[fields[0]] = fields[1]
	}
	return refs, nil
}

// run executes one native command and returns whether it exited zero. An
// executor error is returned unchanged; a non-zero exit is an error here
// because every keep-ref command is expected to succeed.
func (store *KeepRefStore) run(ctx context.Context, path string, args ...string) (bool, error) {
	result, err := store.executor.Run(ctx, Command{
		Name: "git",
		Args: append([]string{"-C", path}, args...),
	})
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, fmt.Errorf("git %s exited %d: %s", args[0], result.ExitCode, strings.TrimSpace(string(result.Stderr)))
	}
	return true, nil
}

// retainedCommits normalizes, validates and deduplicates one authoritative
// retained set. Invalid or empty names are reported separately and never
// become refs. Exceeding the bound is an error: dropping names from an
// over-large set would release refs the caller still asked to keep.
func retainedCommits(retained []string) (valid, invalid []string, err error) {
	seen := make(map[string]bool, len(retained))
	for _, value := range retained {
		commit := strings.ToLower(strings.TrimSpace(value))
		if !validCommitID(commit) {
			if strings.TrimSpace(value) != "" {
				invalid = append(invalid, value)
			}
			continue
		}
		if seen[commit] {
			continue
		}
		seen[commit] = true
		valid = append(valid, commit)
	}
	if len(valid) > MaxKeepRefs {
		return nil, nil, fmt.Errorf("retained commit set has %d commits; at most %d are allowed", len(valid), MaxKeepRefs)
	}
	sort.Strings(valid)
	sort.Strings(invalid)
	return valid, invalid, nil
}

// validCommitID accepts exactly the full object names Git can write: a
// lowercase hexadecimal SHA-1 or SHA-256. Abbreviated names are refused
// because a keep ref must name one immutable object.
func validCommitID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
