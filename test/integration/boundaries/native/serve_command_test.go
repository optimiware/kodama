//go:build integration

// Package native_test proves the real `kodama git serve` command boundary.
//
// The contract server in this file is a fixture of Shell facts, not full
// factory composition: it authenticates the calling service, verifies the
// forwarded actor for the declared operations and records reports. The real
// boundaries under test are the built kodama binary, its production flag and
// composition path, the host git binary, the installed native hooks and the
// private hook socket.
package native_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
	"github.com/optimiware/kodama/internal/id"
	shelloidc "github.com/optimiware/kodama/internal/shell/integration/oidc"
	gitapp "github.com/optimiware/kodama/internal/userland/git/app"
	"github.com/optimiware/kodama/internal/userland/git/app/native"
	idfix "github.com/optimiware/kodama/test/support/boundaries/identity"
	nativefix "github.com/optimiware/kodama/test/support/boundaries/native"
)

// The fixture identities used by the command-boundary tests. They never enter
// the hook environment, a journal, a log or an assertion message.
const (
	scTenantID     = "tenant-serve"
	scProjectID    = "project-serve"
	scServiceID    = "service-serve"
	scServiceToken = "kodsvc.serve.fixture-token"
	scResourceID   = "res-serve"
	scRepository   = "shop/web"
	scHumanSubject = "auth0|serve-engineer"
)

// scPolicy grants the engineer role repository reads and writes below
// refs/heads/ while denying the blocked namespace first (first match wins).
const scPolicy = `
repo shop/web
    R = @all
    - refs/heads/blocked/ = @all
    RW refs/heads/ = @engineer
`

// scFixture is one started `kodama git serve` command boundary.
type scFixture struct {
	t *testing.T

	root             string
	storageRoot      string
	journalDirectory string
	tenantHome       string
	clientHome       string
	adminPath        string
	repositoryPath   string
	contract         *idfix.ContractServer
	humanToken       string
	baseURL          string

	cancel context.CancelFunc
	stderr *lockedText
	done   chan error
}

// scEnvironment returns one deterministic, isolated host environment.
func scEnvironment(home string) []string {
	return []string{
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"HOME=" + home,
		"PATH=" + os.Getenv("PATH"),
	}
}

// scBasicHeader builds the Basic authorization value for the kodama username.
func scBasicHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("kodama:"+token))
}

// lockedText is a concurrency-safe child stderr buffer.
type lockedText struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (text *lockedText) Write(data []byte) (int, error) {
	text.mu.Lock()
	defer text.mu.Unlock()
	return text.buf.Write(data)
}

func (text *lockedText) String() string {
	text.mu.Lock()
	defer text.mu.Unlock()
	return text.buf.String()
}

// scGit runs one host git command with an exact environment.
func scGit(t *testing.T, environment []string, directory string, args ...string) (string, string, int) {
	t.Helper()
	command := exec.Command("git", args...)
	if directory != "" {
		command.Dir = directory
	}
	command.Env = environment
	var stdout, stderr strings.Builder
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return stdout.String(), stderr.String(), exit.ExitCode()
	}
	t.Fatalf("git %v: %v", args, err)
	return "", "", -1
}

// scGitOK runs one host git command and fails on a nonzero exit.
func scGitOK(t *testing.T, environment []string, directory string, args ...string) string {
	t.Helper()
	stdout, stderr, code := scGit(t, environment, directory, args...)
	if code != 0 {
		t.Fatalf("git %v exited %d: %s", args, code, stderr)
	}
	return stdout
}

// scSeedPolicy installs one committed conf/git.conf blob over the provisioned
// trusted commit. It is fixture state setup, not a policy source.
func scSeedPolicy(t *testing.T, environment []string, adminPath, policy string) {
	t.Helper()
	parent := strings.TrimSpace(scGitOK(t, environment, "",
		"--git-dir="+adminPath, "rev-parse", "--verify", "refs/heads/main"))

	policyFile := filepath.Join(t.TempDir(), "git.conf")
	if err := os.WriteFile(policyFile, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	blob := strings.TrimSpace(scGitOK(t, environment, "",
		"--git-dir="+adminPath, "hash-object", "-w", "--no-filters", policyFile))
	indexPath := filepath.Join(t.TempDir(), "index")
	indexEnvironment := append(append([]string(nil), environment...), "GIT_INDEX_FILE="+indexPath)
	scGitOK(t, indexEnvironment, "", "--git-dir="+adminPath,
		"read-tree", parent)
	scGitOK(t, indexEnvironment, "", "--git-dir="+adminPath,
		"update-index", "--add", "--cacheinfo", "100644,"+blob+",conf/git.conf")
	tree := strings.TrimSpace(scGitOK(t, indexEnvironment, "", "--git-dir="+adminPath, "write-tree"))
	commit := strings.TrimSpace(scGitOK(t, environment, "", "--git-dir="+adminPath,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit-tree", tree, "-p", parent, "-m", "fixture committed policy"))
	scGitOK(t, environment, "", "--git-dir="+adminPath, "update-ref", "refs/heads/main", commit)
	if observed := strings.TrimSpace(scGitOK(t, environment, "",
		"--git-dir="+adminPath, "cat-file", "blob", "refs/heads/main:conf/git.conf")); observed != strings.TrimSpace(policy) {
		t.Fatalf("committed policy differs from the fixture policy")
	}
}

// scSeedRepository creates one bare repository with one commit on
// refs/heads/main using real native Git.
func scSeedRepository(t *testing.T, environment []string, root, repositoryPath string) string {
	t.Helper()
	worktree := filepath.Join(root, "seed-worktree")
	if err := os.MkdirAll(worktree, 0o700); err != nil {
		t.Fatalf("create seed worktree: %v", err)
	}
	if err := native.InitBareRepository(context.Background(), native.BareRepositoryConfig{
		Path: repositoryPath,
		Env:  environment,
	}); err != nil {
		t.Fatalf("InitBareRepository: %v", err)
	}
	scGitOK(t, environment, worktree, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(worktree, "seed.txt"), []byte("seed\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	scGitOK(t, environment, worktree, "add", "--all")
	scGitOK(t, environment, worktree,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", "seed")
	scGitOK(t, environment, worktree, "push", "--quiet", repositoryPath, "HEAD:refs/heads/main")
	return strings.TrimSpace(scGitOK(t, environment, worktree, "rev-parse", "HEAD"))
}

// scPeerCertificatePEM returns the loopback fixture server certificate as PEM
// so the child process can trust it through SSL_CERT_FILE.
func scPeerCertificatePEM(t *testing.T, rawURL string) []byte {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse fixture URL: %v", err)
	}
	connection, err := tls.Dial("tcp", parsed.Host, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatalf("read fixture certificate: %v", err)
	}
	defer connection.Close()
	certificates := connection.ConnectionState().PeerCertificates
	if len(certificates) == 0 {
		t.Fatal("fixture presented no certificate")
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificates[0].Raw})
}

// scStartContract starts the Shell-facts contract fixture with both operations
// the production Git edge uses explicitly declared.
func scStartContract(t *testing.T) (*idfix.ContractServer, string) {
	t.Helper()
	provider, err := idfix.NewProvider(idfix.OIDCOptions{Clock: idfix.NewClock(nativefix.FixtureTime())})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	t.Cleanup(provider.Close)
	human, err := provider.Mint(idfix.TokenSpec{
		Subject: scHumanSubject,
		Tenant:  scTenantID,
		Project: scProjectID,
		Roles:   []string{"engineer"},
	})
	if err != nil {
		t.Fatalf("mint human token: %v", err)
	}
	principal, err := shelloidc.PrincipalIDFor(provider.Issuer(), scHumanSubject)
	if err != nil {
		t.Fatalf("human principal id: %v", err)
	}
	contract, err := idfix.NewContractServer(idfix.ContractOptions{
		Clock: provider.Clock(),
		OIDC:  provider,
		Services: []idfix.ServiceAccount{{
			ID:        scServiceID,
			Token:     scServiceToken,
			TenantID:  scTenantID,
			ProjectID: scProjectID,
			Enabled:   true,
		}},
		Bindings: []idfix.Binding{{
			ID:         "binding-serve",
			ServiceID:  scServiceID,
			TenantID:   scTenantID,
			ProjectID:  scProjectID,
			ResourceID: scResourceID,
			Operations: []string{
				string(api.ResourceOperationGitRead),
				string(api.ResourceOperationGitRefUpdate),
			},
			ReportableEvents: []string{"git.ref.updated"},
		}},
		ActorRoles: map[string][]string{
			string(principal): {"engineer"},
		},
	})
	if err != nil {
		t.Fatalf("NewContractServer: %v", err)
	}
	t.Cleanup(contract.Close)
	return contract, human.Value
}

// scStart builds the repository state and starts the real `kodama git serve`
// binary. hostile enables the hostile home, global and system configuration.
func scStart(t *testing.T, hostile bool) *scFixture {
	t.Helper()
	root := t.TempDir()
	fixture := &scFixture{
		t:                t,
		root:             root,
		storageRoot:      filepath.Join(root, "repos"),
		journalDirectory: filepath.Join(root, "journal"),
		tenantHome:       filepath.Join(root, "tenant-home"),
		clientHome:       filepath.Join(root, "client-home"),
	}
	for _, directory := range []string{fixture.storageRoot, fixture.tenantHome, fixture.clientHome} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create fixture directory %q: %v", directory, err)
		}
	}
	seedEnvironment := scEnvironment(fixture.clientHome)
	result, err := gitapp.ProvisionTenant(context.Background(), gitapp.ProvisionConfig{
		TenantID:         id.TenantID(scTenantID),
		StorageRoot:      fixture.storageRoot,
		JournalDirectory: fixture.journalDirectory,
		HookBinary:       kodamaBinary,
		Environment:      seedEnvironment,
	})
	if err != nil {
		t.Fatalf("ProvisionTenant: %v", err)
	}
	fixture.adminPath = result.RepositoryPath
	scSeedPolicy(t, seedEnvironment, fixture.adminPath, scPolicy)
	fixture.repositoryPath = filepath.Join(fixture.storageRoot, scTenantID, filepath.FromSlash(scRepository))
	if err := os.MkdirAll(filepath.Dir(fixture.repositoryPath), 0o700); err != nil {
		t.Fatalf("create repository parent: %v", err)
	}
	scSeedRepository(t, seedEnvironment, root, fixture.repositoryPath)

	contract, humanToken := scStartContract(t)
	fixture.contract = contract
	fixture.humanToken = humanToken
	credentialFile := filepath.Join(root, "service-token")
	if err := os.WriteFile(credentialFile, []byte(scServiceToken+"\n"), 0o600); err != nil {
		t.Fatalf("write service credential: %v", err)
	}
	certificateFile := filepath.Join(root, "contract-ca.pem")
	if err := os.WriteFile(certificateFile, scPeerCertificatePEM(t, contract.URL()), 0o600); err != nil {
		t.Fatalf("write contract certificate: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve listen port: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()

	environment := append(scEnvironment(fixture.tenantHome), "SSL_CERT_FILE="+certificateFile)
	if hostile {
		environment = append(environment, scHostileEnvironment(t, root)...)
	}
	ctx, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(ctx, kodamaBinary,
		"git", "serve",
		"--tenant", scTenantID,
		"--storage-root", fixture.storageRoot,
		"--journal-dir", fixture.journalDirectory,
		"--listen", address,
		"--base-url", contract.URL(),
		"--credential-file", credentialFile,
		"--resource", scRepository+"="+scResourceID,
	)
	command.Env = environment
	command.Dir = root
	fixture.stderr = &lockedText{}
	command.Stderr = fixture.stderr
	command.Stdout = io.Discard
	if err := command.Start(); err != nil {
		cancel()
		t.Fatalf("start kodama git serve: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	fixture.cancel = cancel
	fixture.done = done
	fixture.baseURL = "http://" + address
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("kodama git serve did not exit")
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for {
		select {
		case err := <-done:
			t.Fatalf("kodama git serve exited early: %v: %s", err, fixture.stderr.String())
		default:
		}
		connection, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("kodama git serve did not listen: %v: %s", err, fixture.stderr.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	response, err := http.Get(fixture.baseURL + "/" + scRepository + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("probe smart HTTP endpoint: %v: %s", err, fixture.stderr.String())
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated probe status = %d; want 401: %s", response.StatusCode, fixture.stderr.String())
	}
	return fixture
}

// scHostileEnvironment writes a hostile home, global and system Git
// configuration that would disable the installed hooks and break policy reads
// if it reached a native child.
func scHostileEnvironment(t *testing.T, root string) []string {
	t.Helper()
	hostile := filepath.Join(root, "hostile")
	home := filepath.Join(hostile, "home")
	xdg := filepath.Join(hostile, "xdg")
	hooks := filepath.Join(hostile, "hooks")
	templateDirectory := filepath.Join(hostile, "template")
	for _, directory := range []string{home, filepath.Join(xdg, "git"), hooks, templateDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create hostile directory: %v", err)
		}
	}
	// An allow-all pre-receive hook: if a native child honored this hooks path,
	// the prohibited push below would be accepted.
	allowAll := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte(allowAll), 0o755); err != nil {
		t.Fatalf("write hostile hook: %v", err)
	}
	hostileConfig := "[core]\n" +
		"\thooksPath = " + hooks + "\n" +
		"\tlogAllRefUpdates = true\n" +
		"[init]\n" +
		"\ttemplateDir = " + templateDirectory + "\n" +
		"\tdefaultBranch = hostile\n" +
		"[user]\n" +
		"\tname = Hostile Person\n" +
		"\temail = hostile@evil.example\n" +
		"[safe]\n" +
		"\tdirectory = *\n"
	globalConfig := filepath.Join(home, ".gitconfig")
	xdgConfig := filepath.Join(xdg, "git", "config")
	systemConfig := filepath.Join(hostile, "system-gitconfig")
	for _, path := range []string{globalConfig, xdgConfig, systemConfig} {
		if err := os.WriteFile(path, []byte(hostileConfig), 0o600); err != nil {
			t.Fatalf("write hostile configuration: %v", err)
		}
	}
	return []string{
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + xdg,
		"GIT_CONFIG_GLOBAL=" + globalConfig,
		"GIT_CONFIG_SYSTEM=" + systemConfig,
		"GIT_CONFIG_NOSYSTEM=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_DIR=" + filepath.Join(hostile, "redirected-repository"),
		"GIT_WORK_TREE=" + filepath.Join(hostile, "redirected-worktree"),
		"GIT_INDEX_FILE=" + filepath.Join(hostile, "redirected-index"),
		"GIT_AUTHOR_NAME=Hostile Person",
		"GIT_AUTHOR_EMAIL=hostile@evil.example",
	}
}

// clonePath clones the served repository for one token and returns the
// directory plus its isolated client environment.
func (fixture *scFixture) clonePath(token, name string) (string, []string) {
	fixture.t.Helper()
	directory := filepath.Join(fixture.root, name)
	environment := scEnvironment(fixture.clientHome)
	stderr, code := func() (string, int) {
		_, stderr, code := scGit(fixture.t, environment, "",
			"-c", "http.extraHeader="+scBasicHeader(token),
			"clone", fixture.baseURL+"/"+scRepository+".git", directory)
		return stderr, code
	}()
	if code != 0 {
		fixture.t.Fatalf("clone %s: %d: %s", name, code, stderr)
	}
	return directory, environment
}

// push pushes HEAD to ref with one token.
func (fixture *scFixture) push(environment []string, directory, token, ref string) (string, int) {
	fixture.t.Helper()
	_, stderr, code := scGit(fixture.t, environment, "",
		"-C", directory, "-c", "http.extraHeader="+scBasicHeader(token),
		"push", "origin", "HEAD:"+ref)
	return stderr, code
}

// commit writes one commit in the clone and returns its object id.
func (fixture *scFixture) commit(environment []string, directory, message string) string {
	fixture.t.Helper()
	file := filepath.Join(directory, "content-"+strings.ReplaceAll(message, " ", "-")+".txt")
	if err := os.WriteFile(file, []byte(message+"\n"), 0o600); err != nil {
		fixture.t.Fatalf("write commit file: %v", err)
	}
	scGitOK(fixture.t, environment, directory, "add", "--all")
	scGitOK(fixture.t, environment, directory,
		"-c", "user.name=kodama", "-c", "user.email=kodama@example.invalid",
		"commit", "--quiet", "-m", message)
	return strings.TrimSpace(scGitOK(fixture.t, environment, directory, "rev-parse", "HEAD"))
}

// remoteRef returns one ref in the bare repository.
func (fixture *scFixture) remoteRef(environment []string, ref string) string {
	fixture.t.Helper()
	stdout, _, code := scGit(fixture.t, environment, "", "-C", fixture.repositoryPath, "rev-parse", "--verify", "-q", ref)
	if code != 0 {
		return ""
	}
	return strings.TrimSpace(stdout)
}

// TestGitServeBinaryUsesPublicActorVerification proves the production command
// boundary: the real binary composes the public read verifier, an allowed clone
// verifies git.read through the fixture Shell facts, and an allowed push
// independently verifies git.ref.update and reports the effect.
func TestGitServeBinaryUsesPublicActorVerification(t *testing.T) {
	fixture := scStart(t, false)
	directory, environment := fixture.clonePath(fixture.humanToken, "clone-public-verification")
	commit := fixture.commit(environment, directory, "public verification commit")
	stderr, code := fixture.push(environment, directory, fixture.humanToken, "refs/heads/allowed/public")
	if code != 0 {
		t.Fatalf("allowed push exited %d: %s\nserve stderr: %s\ncontract: %s", code, stderr, fixture.stderr.String(), fixture.contract.RecordedJSON())
	}
	if observed := fixture.remoteRef(environment, "refs/heads/allowed/public"); observed != commit {
		t.Fatalf("allowed ref = %q; want %q", observed, commit)
	}

	recorded := fixture.contract.Recorded()
	var read, update int
	for _, entry := range recorded {
		if entry.Operation != "actor.verify" {
			continue
		}
		if entry.ResourceID != scResourceID {
			t.Errorf("verification resource = %q; want %q", entry.ResourceID, scResourceID)
		}
		switch entry.ResourceOperation {
		case string(api.ResourceOperationGitRead):
			read++
			if entry.ActorKind != "human" {
				t.Errorf("git.read actor kind = %q; want human", entry.ActorKind)
			}
		case string(api.ResourceOperationGitRefUpdate):
			update++
			if entry.ActorKind != "human" {
				t.Errorf("git.ref.update actor kind = %q; want human", entry.ActorKind)
			}
		default:
			t.Errorf("unexpected verification operation %q", entry.ResourceOperation)
		}
	}
	if read == 0 {
		t.Fatal("no git.read verification was recorded through production wiring")
	}
	if update == 0 {
		t.Fatal("no git.ref.update verification was recorded through production wiring")
	}
	if fixture.contract.EventCount() == 0 {
		t.Fatal("the allowed push produced no verified resource report")
	}
	if fixture.contract.VerificationCount() != read+update {
		t.Fatalf("verification count = %d; want %d", fixture.contract.VerificationCount(), read+update)
	}
}

// TestGitServeBinaryIgnoresHostGitConfiguration proves the served binary
// isolates the default executor (policy reads) and the default HTTP backend
// (installed hooks) from a hostile home, global and system Git configuration:
// an allowed control push succeeds and a prohibited push leaves its ref
// unchanged.
func TestGitServeBinaryIgnoresHostGitConfiguration(t *testing.T) {
	fixture := scStart(t, true)
	directory, environment := fixture.clonePath(fixture.humanToken, "clone-hostile-configuration")

	control := fixture.commit(environment, directory, "allowed control commit")
	stderr, code := fixture.push(environment, directory, fixture.humanToken, "refs/heads/allowed/control")
	if code != 0 {
		t.Fatalf("allowed control push exited %d: %s\nserve stderr: %s", code, stderr, fixture.stderr.String())
	}
	if observed := fixture.remoteRef(environment, "refs/heads/allowed/control"); observed != control {
		t.Fatalf("control ref = %q; want %q", observed, control)
	}

	blocked := fixture.commit(environment, directory, "prohibited commit")
	stderr, code = fixture.push(environment, directory, fixture.humanToken, "refs/heads/blocked/main")
	if code == 0 {
		t.Fatalf("prohibited push was accepted; hostile configuration disabled the installed hooks\nserve stderr: %s", fixture.stderr.String())
	}
	if !strings.Contains(stderr, "kodama git: update rejected") {
		t.Fatalf("prohibited push stderr = %q; want the sanitized rejection", stderr)
	}
	if observed := fixture.remoteRef(environment, "refs/heads/blocked/main"); observed != "" {
		t.Fatalf("prohibited ref = %q; want it absent", observed)
	}
	if blocked == control {
		t.Fatal("fixture commits are identical")
	}

	// The serve process must still be running: a hostile configuration may
	// never redirect policy reads or crash the command.
	response, err := http.Get(fixture.baseURL + "/" + scRepository + "/info/refs?service=git-upload-pack")
	if err != nil {
		t.Fatalf("probe after refusal: %v\nserve stderr: %s", err, fixture.stderr.String())
	}
	_, _ = io.Copy(io.Discard, bufio.NewReader(response.Body))
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated probe after refusal = %d; want 401", response.StatusCode)
	}
}
