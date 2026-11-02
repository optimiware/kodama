package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	"github.com/optimiware/kodama/internal/domain"
)

// The read-only mapping of a project's existing configured repository to a
// hosting: external record (docs/implementation/internal-git-service.md,
// "Migration and cutover", step 1). The record exists so that repository
// consumers can address every repository of a project the same way; it
// changes nothing about the workspace module, the publisher or the admitted
// runs, which keep reading their own configuration. The record is ready from
// the start, never empty, never provisioned, and every later change to it
// (patch, archive, restore, refs) is refused with hosting_mismatch.
//
// RegisterExternal is the adapter-facing function the composition calls for a
// configuration it has already resolved; MapConfigured reads it from the
// ExternalSource port.

// ExternalID is the deterministic identifier of the record mapping a project's
// configured repository: the same configuration always maps to the same ID, so
// repeating the mapping converges on one record, and the configured
// repository's own identifier (unique only within its project) cannot collide
// with another project's.
func ExternalID(scope domain.Scope, configuredID string) ID {
	sum := sha256.Sum256(fmt.Appendf(nil, "repository-external/%s/%s/%s", scope.TenantID, scope.ProjectID, configuredID))
	return ID("repo_ext_" + hex.EncodeToString(sum[:16]))
}

var nonNameChars = regexp.MustCompile(`[^a-z0-9]+`)

// externalNames lists, in order, the record names to try: the repository's own
// name (from its URL), then a suffixed one when an unrelated repository already
// holds it.
func externalNames(cfg ExternalConfig) []string {
	base := cfg.URL
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimRight(base, "/")
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.ToLower(base), ".git")
	base = strings.Trim(nonNameChars.ReplaceAllString(base, "-"), "-")
	if base == "" {
		base = "external"
	}
	if base[0] < 'a' || base[0] > 'z' {
		base = "r-" + base
	}
	if len(base) > 54 {
		base = strings.TrimRight(base[:54], "-")
	}
	sum := sha256.Sum256([]byte(cfg.ID))
	return []string{base, base + "-" + hex.EncodeToString(sum[:3])}
}

func externalBranch(baseRef string) (string, error) {
	branch := strings.TrimPrefix(baseRef, gitv1.HeadsPrefix)
	if branch == "" {
		branch = gitv1.DefaultBranchName
	}
	if err := gitv1.ValidateBranchName(branch); err != nil {
		return "", fmt.Errorf("%w: the configured base reference is not a branch: %v", ErrInvalidValue, err)
	}
	return branch, nil
}

// RegisterExternal records the project's configured external repository as a
// hosting: external repository, idempotently: the same configuration returns
// the same record with created false, and a name held by an unrelated
// repository of the project is avoided with a suffixed name. repository.created
// (hosting external) commits with the record. A URL that is not a
// credential-free HTTPS URL (an ssh remote, one with a password) is refused and
// nothing is recorded; the workspace configuration is never read or changed
// here beyond the values passed in.
func (s *Service) RegisterExternal(ctx context.Context, actor Actor, scope domain.Scope, cfg ExternalConfig) (*Repository, bool, error) {
	if err := s.authorize(ctx, actor, ActionRegisterRemote, scope, s.project(scope)); err != nil {
		return nil, false, err
	}
	if err := ID(cfg.ID).Validate(); err != nil {
		return nil, false, err
	}
	branch, err := externalBranch(cfg.BaseRef)
	if err != nil {
		return nil, false, err
	}
	id := ExternalID(scope, cfg.ID)
	// An earlier mapping under whichever name it got is the answer.
	if cur, err := s.d.Store.Get(ctx, scope, id); err == nil {
		return cur, false, nil
	} else if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}
	for _, name := range externalNames(cfg) {
		r, err := NewExternal(NewExternalParams{ID: id, Scope: scope, Name: name, DefaultBranch: branch, URL: cfg.URL, CreatedBy: actor}, s.now())
		if err != nil {
			return nil, false, err
		}
		persisted, created, err := s.d.Store.Insert(ctx, r, Effects{
			Audit: []Audit{audit("repository.external_registered", actor, "", map[string]any{
				"name": r.Name, "defaultBranch": r.DefaultBranch, "hosting": string(r.Hosting),
			})},
			Events: []Event{{Kind: EventCreated, Actor: actor}},
		})
		if err != nil {
			return nil, false, err
		}
		if created || persisted.ID == id {
			return persisted, created, nil
		}
	}
	return nil, false, ErrNameTaken
}

// MapConfigured maps the project's configured repository, read through the
// ExternalSource port, with RegisterExternal. A project without one maps
// nothing (nil, false, nil).
func (s *Service) MapConfigured(ctx context.Context, actor Actor, scope domain.Scope) (*Repository, bool, error) {
	if s.d.External == nil {
		return nil, false, fmt.Errorf("%w: External", ErrNotConfigured)
	}
	if err := scope.Validate(); err != nil {
		return nil, false, err
	}
	cfg, ok, err := s.d.External.Configured(ctx, scope)
	if err != nil {
		return nil, false, fmt.Errorf("%w: configured repository: %w", ErrUnavailable, err)
	}
	if !ok {
		return nil, false, nil
	}
	return s.RegisterExternal(ctx, actor, scope, cfg)
}
