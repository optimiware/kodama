package git

import (
	"bytes"
	"context"
	"fmt"
	"strings"
)

// maxDisjointRebaseCommits bounds the number of linear commits compared during
// a single disjoint-rebase check so pre-receive remains bounded in runtime.
const maxDisjointRebaseCommits = 256

// linearCommit is one single-parent commit in a verified linear chain.
type linearCommit struct {
	oid    string
	parent string
}

// validStrictCommitOID reports whether value is a strict 40-character lowercase
// hexadecimal SHA-1 commit object ID and not the all-zero null OID.
func validStrictCommitOID(value string) bool {
	if len(value) != 40 || isNullObjectID(value) {
		return false
	}
	for index := 0; index < len(value); index++ {
		c := value[index]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// verifyDisjointRebase checks all Git object-store invariants required to accept
// a rebased push under `option approve-rebase.<ref> = disjoint`:
//
//  1. ApprovedBase, ApprovedCandidate, ExpectedBase (current target ref), and
//     Candidate (pushed tip) are valid existing commit objects in the repository
//     (including quarantined push objects via the inherited hook environment).
//  2. ApprovedBase is a strict ancestor of ApprovedCandidate, ApprovedBase is an
//     ancestor of (or equal to) ExpectedBase, and ExpectedBase is a strict
//     ancestor of Candidate (fast-forward).
//  3. Both ApprovedBase..ApprovedCandidate and ExpectedBase..Candidate are
//     strictly linear, merge-free commit chains of identical length.
//  4. Every path touched by ApprovedBase..ApprovedCandidate (and by
//     ExpectedBase..Candidate) is 100% disjoint from every path modified in
//     ApprovedBase..ExpectedBase, including both old and new paths on renames
//     (`--no-renames`) and file/directory prefix collisions.
//  5. Each commit in ExpectedBase..Candidate has a byte-for-byte identical
//     `diff-tree -r --no-commit-id --no-renames --raw --no-abbrev -z` entry set
//     to its corresponding commit in ApprovedBase..ApprovedCandidate, proving
//     identical patch content, whitespace, binary blobs, file modes, symlinks,
//     empty files, and renames/deletes.
//
// Policy/Git invariant failures return a *RefusalError wrapping
// ErrApprovalRequired with a sanitized one-line diagnostic. Executor or context
// errors are returned directly so the hook fails closed without recording a
// permanent refusal.
func (service *Service) verifyDisjointRebase(ctx context.Context, path string, request UpdateRequest, actorID string) error {
	refusal := func(message string) error {
		return &RefusalError{
			Category:      "disjoint_rebase_refused",
			ClientMessage: message,
			ActorID:       actorID,
			Ref:           request.Ref,
			Cause:         ErrApprovalRequired,
		}
	}

	approvedBase := request.ApprovedBase
	approvedCandidate := request.ApprovedCandidate
	currentRef := request.ExpectedBase
	pushed := request.Candidate

	if !validStrictCommitOID(approvedBase) || !validStrictCommitOID(approvedCandidate) {
		return refusal("disjoint rebase refused: invalid approved commit oid; fresh review required")
	}
	if !validObjectID(currentRef) || isNullObjectID(currentRef) || !validObjectID(pushed) || isNullObjectID(pushed) {
		return refusal("disjoint rebase refused: invalid target or pushed commit oid; fresh review required")
	}

	// 1. Verify all four OIDs resolve to real commit objects.
	for _, check := range []struct {
		oid     string
		message string
	}{
		{oid: approvedBase, message: "disjoint rebase refused: approved base commit not found in repository; fresh review required"},
		{oid: approvedCandidate, message: "disjoint rebase refused: approved candidate commit not found in repository; fresh review required"},
		{oid: currentRef, message: "disjoint rebase refused: current target commit not found in repository; fresh review required"},
		{oid: pushed, message: "disjoint rebase refused: pushed commit not found in repository; fresh review required"},
	} {
		ok, err := service.isCommitObject(ctx, path, check.oid)
		if err != nil {
			return err
		}
		if !ok {
			return refusal(check.message)
		}
	}

	// 2. Verify strict ancestry relationships.
	if approvedBase == approvedCandidate {
		return refusal("disjoint rebase refused: approved candidate must be a strict descendant of approved base; fresh review required")
	}
	ok, err := service.isAncestor(ctx, path, approvedBase, approvedCandidate)
	if err != nil {
		return err
	}
	if !ok {
		return refusal("disjoint rebase refused: approved candidate is not a descendant of approved base; fresh review required")
	}

	if approvedBase != currentRef {
		ok, err = service.isAncestor(ctx, path, approvedBase, currentRef)
		if err != nil {
			return err
		}
		if !ok {
			return refusal("disjoint rebase refused: target ref has diverged from approved base; fresh review required")
		}
	}

	if currentRef == pushed {
		return refusal("disjoint rebase refused: push does not advance target ref; fresh review required")
	}
	ok, err = service.isAncestor(ctx, path, currentRef, pushed)
	if err != nil {
		return err
	}
	if !ok {
		return refusal("disjoint rebase refused: push is not a fast-forward of current target ref; fresh review required")
	}

	// 3. Verify linear, merge-free commit series and equal commit counts.
	approvedCommits, reason, err := service.linearCommitSeries(ctx, path, approvedBase, approvedCandidate, "approved")
	if err != nil {
		return err
	}
	if reason != "" {
		return refusal(reason)
	}
	pushedCommits, reason, err := service.linearCommitSeries(ctx, path, currentRef, pushed, "pushed")
	if err != nil {
		return err
	}
	if reason != "" {
		return refusal(reason)
	}
	if len(approvedCommits) != len(pushedCommits) {
		return refusal("disjoint rebase refused: rebased commit count differs from approved series; fresh review required")
	}

	// 4. Collect per-commit raw tree diffs and touched paths for both series.
	approvedRaws := make([][]byte, len(approvedCommits))
	pushedRaws := make([][]byte, len(pushedCommits))
	approvedPaths := make(map[string]struct{})
	pushedPaths := make(map[string]struct{})
	for index := range approvedCommits {
		if err := ctx.Err(); err != nil {
			return err
		}
		approvedRaw, paths, err := service.commitRawDiff(ctx, path, approvedCommits[index].parent, approvedCommits[index].oid)
		if err != nil {
			return err
		}
		approvedRaws[index] = approvedRaw
		for touched := range paths {
			approvedPaths[touched] = struct{}{}
		}

		pushedRaw, pPaths, err := service.commitRawDiff(ctx, path, pushedCommits[index].parent, pushedCommits[index].oid)
		if err != nil {
			return err
		}
		pushedRaws[index] = pushedRaw
		for touched := range pPaths {
			pushedPaths[touched] = struct{}{}
		}
	}

	// 5. Verify touched paths are 100% disjoint from changes on the target ref
	//    between approvedBase and currentRef.
	if approvedBase != currentRef {
		targetPaths, err := service.diffTreePaths(ctx, path, approvedBase, currentRef)
		if err != nil {
			return err
		}
		if pathsOverlap(approvedPaths, targetPaths) || pathsOverlap(pushedPaths, targetPaths) {
			return refusal("disjoint rebase refused: rebased changes overlap files modified on target ref since approved base; fresh review required")
		}
	}

	// 6. Verify exact per-commit patch, content, binary blob, and file-mode
	//    equivalence across the ordered commit series.
	for index := range approvedRaws {
		if !bytes.Equal(approvedRaws[index], pushedRaws[index]) {
			return refusal("disjoint rebase refused: rebased commit patch or file mode differs from approved series; fresh review required")
		}
	}

	return nil
}

func (service *Service) isCommitObject(ctx context.Context, path, oid string) (bool, error) {
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "--no-replace-objects", "cat-file", "-t", oid},
	})
	if err != nil {
		return false, err
	}
	if result.ExitCode != 0 {
		return false, nil
	}
	return strings.TrimSpace(string(result.Stdout)) == "commit", nil
}

func (service *Service) isAncestor(ctx context.Context, path, ancestor, descendant string) (bool, error) {
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "--no-replace-objects", "merge-base", "--is-ancestor", ancestor, descendant},
	})
	if err != nil {
		return false, err
	}
	switch result.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor exited %d", result.ExitCode)
	}
}

func (service *Service) linearCommitSeries(ctx context.Context, path, base, tip, label string) ([]linearCommit, string, error) {
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{
			"-C", path,
			"--no-replace-objects",
			"rev-list",
			fmt.Sprintf("--max-count=%d", maxDisjointRebaseCommits+1),
			"--reverse",
			"--parents",
			base + ".." + tip,
		},
	})
	if err != nil {
		return nil, "", err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Sprintf("disjoint rebase refused: cannot inspect %s commit series; fresh review required", label), nil
	}
	raw := strings.TrimSpace(string(result.Stdout))
	if raw == "" {
		return nil, fmt.Sprintf("disjoint rebase refused: %s commit series is empty; fresh review required", label), nil
	}
	lines := strings.Split(raw, "\n")
	if len(lines) > maxDisjointRebaseCommits {
		return nil, fmt.Sprintf("disjoint rebase refused: %s commit series exceeds %d commits; fresh review required", label, maxDisjointRebaseCommits), nil
	}
	// Check for merge commits across the whole range first so any merge commit
	// produces the specific merge-commit diagnostic before parent-chain checks.
	parsed := make([][]string, len(lines))
	for index, line := range lines {
		fields := strings.Fields(line)
		if len(fields) > 2 {
			return nil, fmt.Sprintf("disjoint rebase refused: merge commit in %s series is not permitted; fresh review required", label), nil
		}
		if len(fields) != 2 {
			return nil, fmt.Sprintf("disjoint rebase refused: non-linear %s commit history; fresh review required", label), nil
		}
		parsed[index] = fields
	}
	commits := make([]linearCommit, 0, len(parsed))
	expectedParent := base
	for _, fields := range parsed {
		oid, parent := fields[0], fields[1]
		if !validObjectID(oid) || !validObjectID(parent) {
			return nil, "", fmt.Errorf("unexpected rev-list output in %s series", label)
		}
		if parent != expectedParent {
			return nil, fmt.Sprintf("disjoint rebase refused: non-linear %s commit history; fresh review required", label), nil
		}
		commits = append(commits, linearCommit{oid: oid, parent: parent})
		expectedParent = oid
	}
	if expectedParent != tip {
		return nil, fmt.Sprintf("disjoint rebase refused: non-linear %s commit history; fresh review required", label), nil
	}
	return commits, "", nil
}

func (service *Service) commitRawDiff(ctx context.Context, path, parent, commit string) ([]byte, map[string]struct{}, error) {
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "--no-replace-objects", "diff-tree", "-r", "--no-commit-id", "--no-renames", "--raw", "--no-abbrev", "-z", parent, commit},
	})
	if err != nil {
		return nil, nil, err
	}
	if result.ExitCode != 0 {
		return nil, nil, fmt.Errorf("git diff-tree --raw exited %d", result.ExitCode)
	}
	paths, err := parseRawDiffPaths(result.Stdout)
	if err != nil {
		return nil, nil, err
	}
	return result.Stdout, paths, nil
}

func (service *Service) diffTreePaths(ctx context.Context, path, from, to string) (map[string]struct{}, error) {
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "--no-replace-objects", "diff-tree", "-r", "--no-commit-id", "--no-renames", "--name-only", "-z", from, to},
	})
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("git diff-tree --name-only exited %d", result.ExitCode)
	}
	return parseNULPaths(result.Stdout)
}

// parseRawDiffPaths extracts every touched path from NUL-delimited
// `git diff-tree -r --no-commit-id --no-renames --raw --no-abbrev -z` output.
// With `--no-renames`, each entry is strictly two NUL-terminated tokens:
// `:<oldmode> <newmode> <oldoid> <newoid> <status>` followed by `<path>`.
func parseRawDiffPaths(data []byte) (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	if len(data) == 0 {
		return paths, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("malformed raw diff-tree output: missing NUL terminator")
	}
	tokens := bytes.Split(data[:len(data)-1], []byte{0})
	if len(tokens)%2 != 0 {
		return nil, fmt.Errorf("malformed raw diff-tree output: unexpected token count %d", len(tokens))
	}
	for index := 0; index < len(tokens); index += 2 {
		meta := string(tokens[index])
		path := string(tokens[index+1])
		if !strings.HasPrefix(meta, ":") || path == "" {
			return nil, fmt.Errorf("malformed raw diff-tree entry")
		}
		fields := strings.Fields(meta[1:])
		if len(fields) != 5 {
			return nil, fmt.Errorf("malformed raw diff-tree metadata")
		}
		status := fields[4]
		if status != "A" && status != "M" && status != "D" && status != "T" {
			return nil, fmt.Errorf("unexpected raw diff-tree status %q", status)
		}
		paths[path] = struct{}{}
	}
	return paths, nil
}

func parseNULPaths(data []byte) (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	if len(data) == 0 {
		return paths, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("malformed name-only diff-tree output: missing NUL terminator")
	}
	for _, token := range bytes.Split(data[:len(data)-1], []byte{0}) {
		if len(token) == 0 {
			return nil, fmt.Errorf("malformed name-only diff-tree output: empty path")
		}
		paths[string(token)] = struct{}{}
	}
	return paths, nil
}

// pathsOverlap reports whether two path sets share any exact path or any
// file/directory prefix collision (for example "dir" vs "dir/file.txt").
func pathsOverlap(left, right map[string]struct{}) bool {
	if len(left) == 0 || len(right) == 0 {
		return false
	}
	rightAncestors := pathAncestors(right)
	for path := range left {
		if _, exact := right[path]; exact {
			return true
		}
		if _, isPrefix := rightAncestors[path]; isPrefix {
			return true
		}
		for idx := strings.IndexByte(path, '/'); idx != -1; {
			if _, exists := right[path[:idx]]; exists {
				return true
			}
			next := strings.IndexByte(path[idx+1:], '/')
			if next == -1 {
				break
			}
			idx += 1 + next
		}
	}
	return false
}

func pathAncestors(paths map[string]struct{}) map[string]struct{} {
	ancestors := make(map[string]struct{})
	for path := range paths {
		for idx := strings.IndexByte(path, '/'); idx != -1; {
			ancestors[path[:idx]] = struct{}{}
			next := strings.IndexByte(path[idx+1:], '/')
			if next == -1 {
				break
			}
			idx += 1 + next
		}
	}
	return ancestors
}
