package gitwebhooktest

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	"github.com/optimiware/kodama/internal/workspace"
)

// Names and commits used by the tests.
const (
	CommitA = workspace.Commit("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	CommitB = workspace.Commit("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	CommitC = workspace.Commit("cccccccccccccccccccccccccccccccccccccccc")
	Zero    = "0000000000000000000000000000000000000000"

	Installer      = "user-installer"
	CheckoutRef    = "secret-checkout"
	PublisherRef   = "secret-publisher"
	HookSecretRef  = "secret-hook"
	RepositoryName = "repo-main"
	RepositoryURL  = "https://git.example.test/org/app.git"
	DefinitionName = domain.DefinitionID("def-ci")
	MainRef        = "refs/heads/main"
	FixtureEpoch   = 1_800_000_000
)

// Clock is a settable clock.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// Now returns the current fixture time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// Fixture is a wired Service over in-memory persistence and port doubles, with
// one installed trigger on main and release/*.
type Fixture struct {
	T       *testing.T
	Service *gw.Service
	Store   *Store
	Auth    *Authorizer
	Secrets *Secrets
	Defs    *Definitions
	Repos   *Repositories
	Git     *Git
	Runs    *Runs
	Clock   *Clock

	Scope   domain.Scope
	Other   domain.Scope
	Secret  []byte
	Trigger gw.Trigger
}

// NewFixture builds the fixture; the Git double reports main existing at
// CommitA and containing it.
func NewFixture(t *testing.T) *Fixture {
	t.Helper()
	f := &Fixture{
		T: t, Store: NewStore(), Auth: &Authorizer{}, Secrets: &Secrets{}, Defs: &Definitions{},
		Repos: &Repositories{}, Git: &Git{}, Runs: &Runs{},
		Clock:  &Clock{t: time.Unix(FixtureEpoch, 0).UTC()},
		Scope:  domain.Scope{TenantID: "tenant-a", ProjectID: "project-a"},
		Other:  domain.Scope{TenantID: "tenant-b", ProjectID: "project-b"},
		Secret: []byte(strings.Repeat("s3cret-", 8)),
	}
	f.Repos.Set(f.Scope, workspace.RepositoryConfig{
		ID: RepositoryName, URL: RepositoryURL, CheckoutSecretRef: CheckoutRef, PublisherSecretRef: PublisherRef, BaseRef: "main",
	})
	f.Secrets.Put(f.Scope, HookSecretRef, f.Secret)
	f.Defs.Register(f.Scope, DefinitionName)
	f.Auth.Grant(Installer, f.Scope, gw.ActionInstall)
	f.Auth.Grant(Installer, f.Scope, gw.ActionRun)
	svc, err := gw.New(gw.Dependencies{
		Store: f.Store, Authorizer: f.Auth, Secrets: f.Secrets, Definitions: f.Defs, Repositories: f.Repos,
		Git: f.Git, Runs: f.Runs, Now: f.Clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Service = svc
	f.Trigger, err = svc.Install(t.Context(), gw.InstallCommand{
		Scope: f.Scope, Principal: Installer, DefinitionID: DefinitionName, Branches: []string{"main", "release/*"}, SecretRef: HookSecretRef,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.Git.SetBranch(MainRef, workspace.BranchInfo{Tip: CommitA, Exists: true, Contains: true})
	return f
}

// Payload returns a valid push payload for the trigger's repository.
func (f *Fixture) Payload(ref, before, after string) gw.Payload {
	return gw.Payload{SchemaVersion: gw.SchemaVersion, Repository: RepositoryName, Ref: ref, Before: before, After: after}
}

// Sign builds a correctly signed notification for the fixture's trigger.
func (f *Fixture) Sign(deliveryID string, body []byte) gw.Notification {
	return f.SignWith(f.Secret, f.Trigger.ID, deliveryID, f.Clock.Now().Unix(), body)
}

// SignWith signs with an explicit secret, trigger and timestamp.
func (f *Fixture) SignWith(secret []byte, id gw.TriggerID, deliveryID string, ts int64, body []byte) gw.Notification {
	return gw.Notification{
		TriggerID: id, DeliveryID: deliveryID, Timestamp: strconv.FormatInt(ts, 10),
		Signature: gw.Sign(secret, id, deliveryID, ts, body), Body: body,
	}
}

// Marshal encodes a payload.
func Marshal(t *testing.T, p gw.Payload) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
