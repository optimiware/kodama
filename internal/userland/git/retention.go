package git

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/optimiware/kodama/api"
)

// maxRetentionPages bounds retained-reference discovery so a broken cursor
// cannot loop forever.
const maxRetentionPages = 1000

// Retention discovers the commits referenced by retained executions through the
// authorized public query. Event hints are advisory only: they never become
// retained commits without an authoritative answer, and a query failure is not
// replaced by hints.
func (service *Service) Retention(ctx context.Context, repository string, _ []string) (RetentionPlan, error) {
	if _, err := service.currentPolicy(ctx, repository); err != nil {
		return RetentionPlan{}, err
	}
	plan := RetentionPlan{Repository: repository}
	seen := make(map[string]bool)
	cursors := make(map[string]bool)
	cursor := ""
	for {
		if plan.Pages >= maxRetentionPages {
			return RetentionPlan{}, fmt.Errorf("retention discovery exceeded %d pages", maxRetentionPages)
		}
		page := api.Page{Cursor: cursor}
		commits, err := service.retention.ReferencedCommits(ctx, page)
		if err != nil {
			return RetentionPlan{}, err
		}
		plan.Pages++
		for _, commit := range commits.Commits {
			if commit == "" || seen[commit] {
				continue
			}
			seen[commit] = true
			plan.Commits = append(plan.Commits, commit)
		}
		next := commits.Next.NextCursor
		if next == "" {
			return plan, nil
		}
		if cursors[next] {
			return RetentionPlan{}, fmt.Errorf("retention cursor %q repeated", next)
		}
		cursors[next] = true
		cursor = next
	}
}

// BackupManifest describes one tenant-scoped repository backup: the
// tenant-scoped path, the observed refs and the retained commits. It carries no
// credential material.
func (service *Service) BackupManifest(ctx context.Context, repository string) (BackupManifest, error) {
	if _, err := service.currentPolicy(ctx, repository); err != nil {
		return BackupManifest{}, err
	}
	path, err := service.repositoryPath(repository)
	if err != nil {
		return BackupManifest{}, err
	}
	result, err := service.executor.Run(ctx, Command{
		Name: "git",
		Args: []string{"-C", path, "for-each-ref", "--format=%(refname) %(objectname)"},
	})
	if err != nil {
		return BackupManifest{}, err
	}
	if result.ExitCode != 0 {
		return BackupManifest{}, fmt.Errorf("for-each-ref exited %d", result.ExitCode)
	}
	refs, err := parseRefStates(result.Stdout)
	if err != nil {
		return BackupManifest{}, err
	}
	plan, err := service.Retention(ctx, repository, nil)
	if err != nil {
		return BackupManifest{}, err
	}
	return BackupManifest{
		TenantID:   service.tenant,
		Repository: repository,
		Path:       path,
		Refs:       refs,
		Retained:   plan.Commits,
	}, nil
}

// parseRefStates parses `for-each-ref` output and sorts it by ref.
func parseRefStates(output []byte) ([]RefState, error) {
	var refs []RefState
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("for-each-ref line %q is malformed", line)
		}
		if !validRef(fields[0]) {
			return nil, fmt.Errorf("for-each-ref ref %q is invalid", fields[0])
		}
		refs = append(refs, RefState{Ref: fields[0], Commit: fields[1]})
	}
	sort.Slice(refs, func(first, second int) bool { return refs[first].Ref < refs[second].Ref })
	return refs, nil
}
