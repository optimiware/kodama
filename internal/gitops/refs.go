package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// operationID derives a deterministic worker operation ID from the parts that
// identify the logical operation, so a repeat after a crash or an unknown
// outcome is the identical request (the worker converges on it).
func operationID(prefix string, parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s|", len(p), p)
	}
	return prefix + "_" + hex.EncodeToString(h.Sum(nil))[:40]
}

// listRefs reads every ref under the namespace, following the cursor.
func listRefs(ctx context.Context, w Worker, tenant domain.TenantID, repositoryID, prefix string) ([]gitv1.RefTip, error) {
	var out []gitv1.RefTip
	var cursor *string
	for range 10000 {
		resp, err := w.ReadRefs(ctx, tenant, gitv1.ReadRefsRequest{RepositoryID: repositoryID, Prefix: prefix, Cursor: cursor, Limit: gitv1.MaxPage})
		if err != nil {
			return nil, err
		}
		out = append(out, resp.Refs...)
		if resp.NextCursor == nil {
			return out, nil
		}
		cursor = resp.NextCursor
	}
	return nil, errors.New("gitops: the ref listing does not end")
}

// refValue reads one ref's commit ("" when absent) and the journal head of the
// same read.
func refValue(ctx context.Context, w Worker, tenant domain.TenantID, repositoryID, ref string) (commit string, journalHead int64, err error) {
	resp, err := w.ReadRefValues(ctx, tenant, gitv1.ReadRefValuesRequest{RepositoryID: repositoryID, Refs: []string{ref}})
	if err != nil {
		return "", 0, err
	}
	for _, v := range resp.Values {
		if v.Ref == ref && v.Commit != nil {
			commit = *v.Commit
		}
	}
	return commit, resp.JournalHead, nil
}

func repositories(ctx context.Context, c Catalog, tenant domain.TenantID, fn func(Repository) error) error {
	after := ""
	for {
		page, err := c.Repositories(ctx, tenant, after, 100)
		if err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		for _, r := range page {
			if err := fn(r); err != nil {
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
		after = page[len(page)-1].ID
	}
}

func isManagedBranch(defaultBranch, ref string) bool {
	r, err := gitv1.ClassifyRef(defaultBranch, ref)
	if err != nil {
		return false
	}
	return r.Class == gitv1.ClassTask || r.Class == gitv1.ClassAgent || r.Class == gitv1.ClassRun
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// listedRepositories is the set of repository IDs the catalog lists for the tenant.
func listedRepositories(ctx context.Context, c Catalog, tenant domain.TenantID) (map[string]bool, error) {
	out := map[string]bool{}
	err := repositories(ctx, c, tenant, func(r Repository) error {
		out[r.ID] = true
		return nil
	})
	return out, err
}
