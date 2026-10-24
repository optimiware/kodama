package identity

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/optimiware/kodama/internal/domain"
)

// MaxPageRows bounds the rows one list read may request (callers ask for page
// size + 1).
const MaxPageRows = 1001

// MaxProjectBindingOps bounds the grants and revocations of one project update.
const MaxProjectBindingOps = 20

// Config wires a Service. Repository and Directory are required: there is no
// default that grants access.
type Config struct {
	Repository Repository
	Directory  Directory
	Clock      func() time.Time
	NewID      func(prefix string) (string, error)
}

// Service holds the identity use cases. Every mutation re-reads the acting
// user's bindings inside the mutating transaction, after the authorization
// locks, so a concurrent revocation is seen and the decision stays valid until
// commit.
type Service struct {
	repo  Repository
	dir   Directory
	now   func() time.Time
	newID func(string) (string, error)
}

// NewService validates cfg.
func NewService(cfg Config) (*Service, error) {
	if cfg.Repository == nil || cfg.Directory == nil {
		return nil, errors.New("identity: a repository and a directory are required")
	}
	s := &Service{repo: cfg.Repository, dir: cfg.Directory, now: cfg.Clock, newID: cfg.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = RandomID
	}
	return s, nil
}

// Caller is the verified user behind a request.
type Caller struct {
	UserID    UserID
	RequestID string
}

func (c Caller) validate() error { return c.UserID.Validate() }

func (c Caller) actor() Actor { return UserActor(c.UserID) }

func (s *Service) id(prefix string) (string, error) {
	id, err := s.newID(prefix)
	if err != nil {
		return "", Unavailablef(err, "identifier generation failed")
	}
	return id, nil
}

// Me returns the verified caller's own record. It needs no membership.
func (s *Service) Me(ctx context.Context, c Caller) (User, error) {
	if err := c.validate(); err != nil {
		return User{}, err
	}
	return s.dir.User(ctx, c.UserID)
}

// Tenants discovers the tenants where the caller holds a binding. A caller
// without bindings gets an empty list; an outage is an error, never an empty list.
func (s *Service) Tenants(ctx context.Context, c Caller, afterID string, limit int) ([]TenantSummary, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPageRows {
		return nil, invalid("page size is out of range")
	}
	return s.dir.Tenants(ctx, c.UserID, afterID, limit)
}

// authorizeInTx re-reads the caller's bindings in the transaction and decides.
func authorizeInTx(ctx context.Context, tx Tx, c Caller, target Target, perm Permission) ([]Binding, error) {
	bindings, err := tx.Bindings(ctx, c.UserID)
	if err != nil {
		return nil, err
	}
	if err := Decide(bindings, c.actor(), target, perm); err != nil {
		return nil, err
	}
	return bindings, nil
}

// GrantCommand adds one fixed-role binding.
type GrantCommand struct {
	Caller  Caller
	Tenant  domain.TenantID
	User    UserID
	Project domain.ProjectID // empty: tenant-wide
	Role    Role
}

// Grant adds a binding. Tenant-wide bindings need a tenant-wide administrator; a
// project binding needs administration of that project (tenant-wide or
// project-scoped). It never creates a user and never widens the caller's reach.
func (s *Service) Grant(ctx context.Context, cmd GrantCommand) (Binding, error) {
	if err := cmd.Caller.validate(); err != nil {
		return Binding{}, err
	}
	now := s.now()
	bid, err := s.id("mb_")
	if err != nil {
		return Binding{}, err
	}
	chg, err := s.id("chg_")
	if err != nil {
		return Binding{}, err
	}
	b, err := Grant(BindingID(bid), cmd.Tenant, cmd.User, cmd.Project, cmd.Role, now)
	if err != nil {
		return Binding{}, err
	}
	var out Binding
	err = s.repo.Transact(ctx, cmd.Tenant, func(ctx context.Context, tx Tx) error {
		var project Project
		if cmd.Project != "" {
			if project, err = tx.LockProject(ctx, cmd.Project); err != nil {
				return err
			}
		}
		if err := tx.LockAuthorization(ctx, []UserID{cmd.User}, []UserID{cmd.Caller.UserID}); err != nil {
			if errors.Is(err, ErrNotFound) {
				return invalid("userId does not identify a known user")
			}
			return err
		}
		if _, err := authorizeInTx(ctx, tx, cmd.Caller, Target{TenantID: cmd.Tenant, ProjectID: cmd.Project}, PermMembershipManage); err != nil {
			return err
		}
		existing, err := tx.Bindings(ctx, cmd.User)
		if err != nil {
			return err
		}
		if err := CheckNoDuplicate(b, existing); err != nil {
			return err
		}
		if err := tx.InsertBinding(ctx, b); err != nil {
			return err
		}
		rev, err := tx.BumpAuthorization(ctx, cmd.User)
		if err != nil {
			return err
		}
		change := Change{ID: chg, Kind: ChangeBindingGranted, Actor: cmd.Caller.actor(), RequestID: cmd.Caller.RequestID, OccurredAt: now, Tenant: cmd.Tenant,
			Bindings: []BindingChange{{User: b.User, Project: b.Project, EffectiveVersion: rev,
				Ops: []BindingOp{{ID: b.ID, Kind: ChangeBindingGranted, Role: b.Role, Version: b.Version}}}}}
		if cmd.Project != "" {
			if change.Project, err = bumpProject(ctx, tx, project, ProjectPatch{}, true, now); err != nil {
				return err
			}
		}
		out = b
		return tx.Record(ctx, change)
	})
	return out, err
}

// bumpProject applies the patch and/or a bindings change to the locked project,
// stores it and returns its change record.
func bumpProject(ctx context.Context, tx Tx, current Project, patch ProjectPatch, bindings bool, now time.Time) (*ProjectChange, error) {
	next, changed, err := current.Apply(patch, bindings, now)
	if err != nil {
		return nil, err
	}
	if err := tx.UpdateProject(ctx, next, current.Version); err != nil {
		return nil, err
	}
	return &ProjectChange{Scope: next.Scope, Version: next.Version, Changed: changed}, nil
}

// ChangeRoleCommand changes the role of one binding.
type ChangeRoleCommand struct {
	Caller   Caller
	Tenant   domain.TenantID
	ID       BindingID
	Expected domain.Version
	Role     Role
}

// ChangeRole moves a binding to another fixed role. The same role is a no-op that
// returns the current binding.
func (s *Service) ChangeRole(ctx context.Context, cmd ChangeRoleCommand) (Binding, error) {
	return s.modify(ctx, cmd.Caller, cmd.Tenant, cmd.ID, cmd.Expected, &cmd.Role)
}

// RevokeCommand removes one binding.
type RevokeCommand struct {
	Caller   Caller
	Tenant   domain.TenantID
	ID       BindingID
	Expected domain.Version
}

// Revoke removes a binding; access ends with the commit.
func (s *Service) Revoke(ctx context.Context, cmd RevokeCommand) (Binding, error) {
	return s.modify(ctx, cmd.Caller, cmd.Tenant, cmd.ID, cmd.Expected, nil)
}

// modify changes the role (role != nil) or revokes (role == nil) one binding.
func (s *Service) modify(ctx context.Context, c Caller, tenant domain.TenantID, id BindingID, expected domain.Version, role *Role) (Binding, error) {
	if err := c.validate(); err != nil {
		return Binding{}, err
	}
	if err := id.Validate(); err != nil {
		return Binding{}, err
	}
	if err := expected.Validate(); err != nil {
		return Binding{}, invalid("%v", err)
	}
	if role != nil && !role.Valid() {
		return Binding{}, invalid("role must be one of admin, analyst, engineer, reviewer, viewer")
	}
	now := s.now()
	chg, err := s.id("chg_")
	if err != nil {
		return Binding{}, err
	}
	var out Binding
	err = s.repo.Transact(ctx, tenant, func(ctx context.Context, tx Tx) error {
		first, err := tx.Binding(ctx, id)
		if err != nil {
			return err
		}
		var project Project
		if first.Project != "" {
			if project, err = tx.LockProject(ctx, first.Project); err != nil {
				return err
			}
		}
		if err := tx.LockAuthorization(ctx, []UserID{first.User}, []UserID{c.UserID}); err != nil {
			return err
		}
		// The binding may have changed or vanished while waiting for the locks.
		cur, err := tx.Binding(ctx, id)
		if err != nil {
			return err
		}
		if _, err := authorizeInTx(ctx, tx, c, Target{TenantID: tenant, ProjectID: cur.Project}, PermMembershipManage); err != nil {
			return err
		}
		if cur.Version != expected {
			return ErrVersionConflict
		}
		op := BindingOp{ID: cur.ID, Kind: ChangeBindingRevoked, Role: cur.Role, Version: cur.Version}
		next := cur
		if role != nil {
			if next, err = cur.ChangeRole(*role); errors.Is(err, ErrNoChange) {
				out = cur
				return nil
			} else if err != nil {
				return err
			}
			existing, err := tx.Bindings(ctx, cur.User)
			if err != nil {
				return err
			}
			if err := CheckNoDuplicate(next, existing); err != nil {
				return err
			}
			if err := tx.UpdateBinding(ctx, next, expected); err != nil {
				return err
			}
			op = BindingOp{ID: next.ID, Kind: ChangeBindingRoleChanged, Role: next.Role, PreviousRole: cur.Role, Version: next.Version}
		} else if err := tx.DeleteBinding(ctx, cur); err != nil {
			return err
		}
		rev, err := tx.BumpAuthorization(ctx, cur.User)
		if err != nil {
			return err
		}
		change := Change{ID: chg, Kind: op.Kind, Actor: c.actor(), RequestID: c.RequestID, OccurredAt: now, Tenant: tenant,
			Bindings: []BindingChange{{User: cur.User, Project: cur.Project, EffectiveVersion: rev, Ops: []BindingOp{op}}}}
		if cur.Project != "" {
			if change.Project, err = bumpProject(ctx, tx, project, ProjectPatch{}, true, now); err != nil {
				return err
			}
		}
		out = next
		return tx.Record(ctx, change)
	})
	return out, err
}

// ListBindingsCommand lists the bindings of scopes the caller administers.
type ListBindingsCommand struct {
	Caller  Caller
	Tenant  domain.TenantID
	Project domain.ProjectID // optional filter
	User    UserID           // optional filter
	After   BindingID
	Limit   int
}

// ListBindings returns the bindings of every scope the caller administers (all
// of the tenant for a tenant-wide administrator, otherwise the administered
// projects), narrowed by the filters. A filter naming a project the caller does
// not administer yields an empty page.
func (s *Service) ListBindings(ctx context.Context, cmd ListBindingsCommand) ([]Binding, error) {
	if err := cmd.Caller.validate(); err != nil {
		return nil, err
	}
	if cmd.Limit < 1 || cmd.Limit > MaxPageRows {
		return nil, invalid("page size is out of range")
	}
	var out []Binding
	err := s.repo.Transact(ctx, cmd.Tenant, func(ctx context.Context, tx Tx) error {
		bindings, err := authorizeInTx(ctx, tx, cmd.Caller, Target{TenantID: cmd.Tenant, AnyProject: true}, PermMembershipManage)
		if err != nil {
			return err
		}
		reach := ReachOf(bindings, cmd.Tenant, PermMembershipManage)
		q := BindingQuery{AllScopes: reach.All, Projects: reach.Projects, Project: cmd.Project, User: cmd.User, After: cmd.After, Limit: cmd.Limit}
		if !reach.All && cmd.Project != "" && !reach.Contains(cmd.Project) {
			return nil
		}
		out, err = tx.ListBindings(ctx, q)
		return err
	})
	return out, err
}

// ProjectView is a project with the caller's own roles reaching it.
type ProjectView struct {
	Project     Project
	CallerRoles []Role
}

func roleNames(b []Binding, t Target) []Role {
	roles := RolesAt(b, t)
	slices.Sort(roles)
	return roles
}

// ListProjects returns the projects the caller may read: every project for a
// tenant-wide binding, otherwise the bound projects.
func (s *Service) ListProjects(ctx context.Context, c Caller, tenant domain.TenantID, after domain.ProjectID, limit int) ([]ProjectView, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if limit < 1 || limit > MaxPageRows {
		return nil, invalid("page size is out of range")
	}
	var out []ProjectView
	err := s.repo.Transact(ctx, tenant, func(ctx context.Context, tx Tx) error {
		bindings, err := authorizeInTx(ctx, tx, c, Target{TenantID: tenant, AnyProject: true}, PermProjectRead)
		if err != nil {
			return err
		}
		reach := ReachOf(bindings, tenant, PermProjectRead)
		if !reach.All && len(reach.Projects) == 0 {
			return nil
		}
		ps, err := tx.ListProjects(ctx, ProjectQuery{All: reach.All, Only: reach.Projects, After: after, Limit: limit})
		if err != nil {
			return err
		}
		for _, p := range ps {
			out = append(out, ProjectView{Project: p, CallerRoles: roleNames(bindings, Target{TenantID: tenant, ProjectID: p.Scope.ProjectID})})
		}
		return nil
	})
	return out, err
}

// GetProject returns one project the caller may read.
func (s *Service) GetProject(ctx context.Context, c Caller, scope domain.Scope) (ProjectView, error) {
	if err := c.validate(); err != nil {
		return ProjectView{}, err
	}
	if err := scope.Validate(); err != nil {
		return ProjectView{}, invalid("%v", err)
	}
	var out ProjectView
	err := s.repo.Transact(ctx, scope.TenantID, func(ctx context.Context, tx Tx) error {
		target := Target{TenantID: scope.TenantID, ProjectID: scope.ProjectID}
		bindings, err := authorizeInTx(ctx, tx, c, target, PermProjectRead)
		if err != nil {
			return err
		}
		p, err := tx.Project(ctx, scope.ProjectID)
		if err != nil {
			return err
		}
		out = ProjectView{Project: p, CallerRoles: roleNames(bindings, target)}
		return nil
	})
	return out, err
}

// CreateProjectCommand creates a project.
type CreateProjectCommand struct {
	Caller         Caller
	Tenant         domain.TenantID
	Key            string
	DisplayName    string
	Settings       json.RawMessage
	ActiveWIPLimit int
}

// CreateProject creates a project at version 1 for a tenant-wide administrator.
// A duplicate key is ErrConflict and records nothing.
func (s *Service) CreateProject(ctx context.Context, cmd CreateProjectCommand) (ProjectView, error) {
	if err := cmd.Caller.validate(); err != nil {
		return ProjectView{}, err
	}
	pid, err := s.id("p_")
	if err != nil {
		return ProjectView{}, err
	}
	chg, err := s.id("chg_")
	if err != nil {
		return ProjectView{}, err
	}
	now := s.now()
	p, err := NewProject(domain.Scope{TenantID: cmd.Tenant, ProjectID: domain.ProjectID(pid)}, cmd.Key, cmd.DisplayName, cmd.Settings, cmd.ActiveWIPLimit, now)
	if err != nil {
		return ProjectView{}, err
	}
	var out ProjectView
	err = s.repo.Transact(ctx, cmd.Tenant, func(ctx context.Context, tx Tx) error {
		if err := tx.LockAuthorization(ctx, nil, []UserID{cmd.Caller.UserID}); err != nil {
			return err
		}
		bindings, err := authorizeInTx(ctx, tx, cmd.Caller, Target{TenantID: cmd.Tenant}, PermProjectCreate)
		if err != nil {
			return err
		}
		if err := tx.InsertProject(ctx, p); err != nil {
			return err
		}
		out = ProjectView{Project: p, CallerRoles: roleNames(bindings, Target{TenantID: cmd.Tenant, ProjectID: p.Scope.ProjectID})}
		return tx.Record(ctx, Change{ID: chg, Kind: ChangeProjectCreated, Actor: cmd.Caller.actor(), RequestID: cmd.Caller.RequestID, OccurredAt: now, Tenant: cmd.Tenant,
			Project: &ProjectChange{Scope: p.Scope, Version: p.Version, Changed: []string{SettingKey, SettingDisplayName, SettingSettings, SettingActiveWIPLimit}}})
	})
	return out, err
}

// BindingGrant is one project-binding grant of a project update.
type BindingGrant struct {
	User UserID
	Role Role
}

// UpdateProjectCommand updates settings and/or role bindings of one project in
// one transaction and one version step. Bindings are project-scoped only: this
// route cannot create tenant-wide bindings or touch another project.
type UpdateProjectCommand struct {
	Caller   Caller
	Scope    domain.Scope
	Expected domain.Version
	Patch    ProjectPatch
	Grant    []BindingGrant
	Revoke   []BindingID
}

// UpdateProject applies the command. Binding changes use the same transitions,
// checks, revisions, audit and events as the membership routes, and advance the
// project version exactly once.
func (s *Service) UpdateProject(ctx context.Context, cmd UpdateProjectCommand) (ProjectView, error) {
	c := cmd.Caller
	if err := c.validate(); err != nil {
		return ProjectView{}, err
	}
	if err := cmd.Scope.Validate(); err != nil {
		return ProjectView{}, invalid("%v", err)
	}
	if err := cmd.Expected.Validate(); err != nil {
		return ProjectView{}, invalid("%v", err)
	}
	if len(cmd.Grant) > MaxProjectBindingOps || len(cmd.Revoke) > MaxProjectBindingOps {
		return ProjectView{}, invalid("at most %d grants and %d revocations are allowed", MaxProjectBindingOps, MaxProjectBindingOps)
	}
	for _, g := range cmd.Grant {
		if err := g.User.Validate(); err != nil {
			return ProjectView{}, err
		}
		if !g.Role.Valid() {
			return ProjectView{}, invalid("role must be one of admin, analyst, engineer, reviewer, viewer")
		}
	}
	for _, id := range cmd.Revoke {
		if err := id.Validate(); err != nil {
			return ProjectView{}, err
		}
	}
	hasBindings := len(cmd.Grant) > 0 || len(cmd.Revoke) > 0
	now := s.now()
	chg, err := s.id("chg_")
	if err != nil {
		return ProjectView{}, err
	}
	grants := make([]Binding, 0, len(cmd.Grant))
	for _, g := range cmd.Grant {
		bid, err := s.id("mb_")
		if err != nil {
			return ProjectView{}, err
		}
		b, err := Grant(BindingID(bid), cmd.Scope.TenantID, g.User, cmd.Scope.ProjectID, g.Role, now)
		if err != nil {
			return ProjectView{}, err
		}
		grants = append(grants, b)
	}
	var out ProjectView
	err = s.repo.Transact(ctx, cmd.Scope.TenantID, func(ctx context.Context, tx Tx) error {
		project, err := tx.LockProject(ctx, cmd.Scope.ProjectID)
		if err != nil {
			return err
		}
		// Users whose authorization changes: grant targets and owners of revoked
		// bindings (read once before locking; re-read under the lock below).
		var changed []UserID
		for _, g := range grants {
			changed = append(changed, g.User)
		}
		for _, id := range cmd.Revoke {
			b, err := tx.Binding(ctx, id)
			if err != nil {
				return err
			}
			changed = append(changed, b.User)
		}
		if err := tx.LockAuthorization(ctx, changed, []UserID{c.UserID}); err != nil {
			if errors.Is(err, ErrNotFound) {
				return invalid("a userId does not identify a known user")
			}
			return err
		}
		target := Target{TenantID: cmd.Scope.TenantID, ProjectID: cmd.Scope.ProjectID}
		bindings, err := authorizeInTx(ctx, tx, c, target, PermProjectSettings)
		if err != nil {
			return err
		}
		roles := roleNames(bindings, target)
		if hasBindings {
			if _, err := authorizeInTx(ctx, tx, c, target, PermMembershipManage); err != nil {
				return err
			}
		}
		if project.Version != cmd.Expected {
			return ErrVersionConflict
		}
		perUser := map[UserID]*BindingChange{}
		var order []UserID
		touch := func(u UserID) *BindingChange {
			if bc, ok := perUser[u]; ok {
				return bc
			}
			bc := &BindingChange{User: u, Project: cmd.Scope.ProjectID}
			perUser[u] = bc
			order = append(order, u)
			return bc
		}
		for _, b := range grants {
			existing, err := tx.Bindings(ctx, b.User)
			if err != nil {
				return err
			}
			if err := CheckNoDuplicate(b, existing); err != nil {
				return err
			}
			if err := tx.InsertBinding(ctx, b); err != nil {
				return err
			}
			bc := touch(b.User)
			bc.Ops = append(bc.Ops, BindingOp{ID: b.ID, Kind: ChangeBindingGranted, Role: b.Role, Version: b.Version})
		}
		for _, id := range cmd.Revoke {
			b, err := tx.Binding(ctx, id)
			if err != nil {
				return err
			}
			if b.Project != cmd.Scope.ProjectID {
				return ErrNotFound // this route administers only its own project's bindings
			}
			if err := tx.DeleteBinding(ctx, b); err != nil {
				return err
			}
			bc := touch(b.User)
			bc.Ops = append(bc.Ops, BindingOp{ID: b.ID, Kind: ChangeBindingRevoked, Role: b.Role, Version: b.Version})
		}
		change := Change{ID: chg, Kind: ChangeProjectUpdated, Actor: c.actor(), RequestID: c.RequestID, OccurredAt: now, Tenant: cmd.Scope.TenantID}
		for _, u := range order {
			rev, err := tx.BumpAuthorization(ctx, u)
			if err != nil {
				return err
			}
			perUser[u].EffectiveVersion = rev
			change.Bindings = append(change.Bindings, *perUser[u])
		}
		pc, err := bumpProject(ctx, tx, project, cmd.Patch, hasBindings, now)
		if errors.Is(err, ErrNoChange) {
			out = ProjectView{Project: project, CallerRoles: roles}
			return nil
		}
		if err != nil {
			return err
		}
		change.Project = pc
		updated, err := tx.Project(ctx, cmd.Scope.ProjectID)
		if err != nil {
			return err
		}
		out = ProjectView{Project: updated, CallerRoles: roles}
		return tx.Record(ctx, change)
	})
	return out, err
}
