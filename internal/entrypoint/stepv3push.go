package entrypoint

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
	runtimev3 "github.com/optimiware/kodama/contracts/runtime/v3"
	"github.com/optimiware/kodama/internal/domain"
)

// Explicit pushes of a v3 attempt.
//
// After a successful command the entrypoint publishes what the attempt produced
// to the internal Git service with the attempt's own Git grant. Nothing the
// command could have configured is trusted: Git runs sealed (sealedgit.go) with a
// credential helper scoped to the service (gitcred.go), and the server's push
// authorizer re-decides every update at the push (the grant is a ceiling the
// entrypoint stays far inside of):
//
//   - the writer (workspace access write) commits the whole non-ignored tree of
//     each write source on top of its HEAD, as the v2 writer did, and pushes that
//     commit to the source's task branch with a lease on the expected tip, so a
//     moved branch is a conflict, never an overwrite;
//   - every attempt, writer or not, pushes the branch its command left checked out
//     in a source when, and only when, a pattern of the attempt's grant selects it
//     (the run's agent-branch namespace, or the writer's task branch); a detached
//     HEAD, another branch or an uncommitted tree publishes nothing;
//   - a seed or integration grant the command requested through `kodama step-git`
//     (stepgit.go) is used for exactly the one update it allows;
//   - every pushed commit is scanned first for any injected secret value or Git
//     credential; one hit publishes nothing at all.
//
// One `git push --atomic` is made per source (one receive, one push record); the
// push record's ID is what the result reports, and Git does not return it. The
// service's post-receive hook prints one line, `kodama-push-id: <id>`, which Git
// relays to the client as `remote: kodama-push-id: <id>`; see README.md for the
// requirement this puts on the Git worker (T-128) and push journal (T-130).

var (
	pushIDLine  = regexp.MustCompile(`(?m)^remote:\s+kodama-push-id:\s+(\S+)\s*$`)
	pushIDValue = regexp.MustCompile(domain.IDPattern)
)

// plannedUpdate is one ref update of one push.
type plannedUpdate struct {
	ref   string
	new   string
	old   string // the expected remote value, "" for none
	lease bool   // the update is conditional on old (an empty old: the ref must not exist)
}

// plannedPush is one receive: one source or runtime grant, one repository, atomic.
type plannedPush struct {
	sr           *sourceRepo
	repositoryID string
	url          string
	credFile     string // the credential file of the grant that pushes
	scopeURL     string // the one repository URL a runtime grant answers for; "" for the attempt grant
	updates      []plannedUpdate
	bases        []string // objects reachable from these are already on the server: not scanned
	advance      string   // the commit the workspace HEAD follows once the push applied
}

// pushAll plans, scans and performs every push of the attempt and returns the
// commit outputs of the refs it updated.
func (a *attempt) pushAll() ([]runtimev3.CommitOutput, *verdict, *abortError) {
	ctx := a.finalCtx()
	reject := func(reason, format string, args ...any) ([]runtimev3.CommitOutput, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(reason, nil)
		return nil, &v, nil
	}
	a.loadGitCredentials()
	var plans []*plannedPush
	for _, sr := range a.srcs {
		p, v, ab := a.planSource(ctx, sr)
		if ab != nil || v != nil {
			return nil, v, ab
		}
		if p != nil {
			plans = append(plans, p)
		}
	}
	extra, v, ab := a.planRuntimeGrants(ctx)
	if ab != nil || v != nil {
		return nil, v, ab
	}
	plans = append(plans, extra...)
	if len(plans) == 0 {
		return []runtimev3.CommitOutput{}, nil, nil
	}

	// Prior disclosure: nothing is pushed unless nothing to push carries an injected
	// value. The server re-checks; this keeps the value from ever leaving the Pod.
	// The destination ref names are disclosure too: a branch a command named after a
	// secret would put it in the git arguments, on the server and in the result.
	needles := a.secretNeedles()
	for _, p := range plans {
		for _, u := range p.updates {
			if containsNeedle(u.ref, needles) {
				// Neither the name nor the value is repeated anywhere.
				return reject(ReasonSecretInSource, "a destination ref name of repository %s contains an injected secret value; nothing was pushed", p.repositoryID)
			}
		}
	}
	for _, p := range plans {
		for _, u := range p.updates {
			where, err := p.sr.sg.scanObjects(ctx, p.sr.dir, p.bases, u.new, needles)
			if err != nil {
				if ab := a.classify(ctx, err); ab != nil {
					return nil, nil, ab
				}
				return reject(ReasonSourceFailed, "cannot scan %s for injected secrets: %v", u.ref, err)
			}
			if where != "" {
				return reject(ReasonSecretInSource, "path %q in the change for %s contains an injected secret value; nothing was pushed", where, u.ref)
			}
		}
	}

	var outputs []runtimev3.CommitOutput
	for _, p := range plans {
		out, v, ab := a.push(ctx, p)
		if ab != nil || v != nil {
			return nil, v, ab
		}
		outputs = append(outputs, out...)
	}
	sort.Slice(outputs, func(i, j int) bool {
		if outputs[i].RepositoryID != outputs[j].RepositoryID {
			return outputs[i].RepositoryID < outputs[j].RepositoryID
		}
		return outputs[i].Ref < outputs[j].Ref
	})
	for i := 1; i < len(outputs); i++ {
		if outputs[i-1].RepositoryID == outputs[i].RepositoryID && outputs[i-1].Ref == outputs[i].Ref {
			return reject(ReasonSourceFailed, "two sources of repository %s updated %s", outputs[i].RepositoryID, outputs[i].Ref)
		}
	}
	if outputs == nil {
		outputs = []runtimev3.CommitOutput{}
	}
	return outputs, nil, nil
}

// grantPatterns are the ref patterns the attempt's own grant allows in a repository.
func (a *attempt) grantPatterns(repositoryID string) []string {
	if a.c3.Git == nil {
		return nil
	}
	for _, r := range a.c3.Git.Repositories {
		if r.RepositoryID == repositoryID {
			return r.Update
		}
	}
	return nil
}

// containsNeedle reports whether s contains any value of the disclosure set that is
// long enough to be scanned (as in the object scan).
func containsNeedle(s string, needles [][]byte) bool {
	for _, n := range needles {
		if len(n) >= minSecretScanLen && bytes.Contains([]byte(s), n) {
			return true
		}
	}
	return false
}

func permits(patterns []string, ref string) bool {
	for _, p := range patterns {
		if gitv1.MatchRefPattern(p, ref) {
			return true
		}
	}
	return false
}

// planSource decides what the attempt publishes from one source.
func (a *attempt) planSource(ctx context.Context, sr *sourceRepo) (*plannedPush, *verdict, *abortError) {
	reject := func(reason, format string, args ...any) (*plannedPush, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(reason, nil)
		return nil, &v, nil
	}
	src := sr.src
	// The command may have committed, reset or rewritten the repository: the sealed
	// Git is pointed at whatever HEAD is now, as data.
	if _, err := sr.sg.sync(ctx); err != nil {
		if ab := a.classify(ctx, err); ab != nil {
			return nil, nil, ab
		}
		return reject(ReasonSourceFailed, "the repository of source %s cannot be read: %v", src.Path, err)
	}
	head, headRef, err := sr.sg.headAndRef()
	if err != nil {
		return reject(ReasonSourceFailed, "cannot read HEAD of source %s: %v", src.Path, err)
	}
	if a.c3.Git == nil || a.c3.Git.CredentialFile == nil {
		return reject(ReasonSourceFailed, "the context delivers no Git credential for source %s", src.Path)
	}
	plan := &plannedPush{sr: sr, repositoryID: src.RepositoryID, url: src.RepositoryURL, bases: []string{string(src.HeadCommit), src.Commit}}
	plan.credFile, err = a.s.cfg.gitFile(*a.c3.Git.CredentialFile)
	if err != nil {
		return reject(ReasonSourceFailed, "%v", err)
	}
	patterns := a.grantPatterns(src.RepositoryID)

	// The writer's task branch.
	writer := a.c3.Workspace.Access == runtimev3.AccessWrite && src.Access == gitv1.AccessWrite && src.Branch != nil && src.ExpectedTip != nil
	candidate := head
	if writer {
		dirty, err := sr.sg.dirty(ctx, sr.dir)
		if err != nil {
			return reject(ReasonSourceFailed, "cannot inspect source %s: %v", src.Path, err)
		}
		if dirty {
			msg := fmt.Sprintf("kodama: %s (attempt %s)", a.c.StepID, a.c.AttemptID)
			if candidate, err = sr.sg.commitAll(ctx, sr.dir, msg); err != nil {
				return reject(ReasonSourceFailed, "cannot commit the changes of source %s: %v", src.Path, err)
			}
		}
		tip := string(*src.ExpectedTip)
		if candidate != tip {
			ok, err := sr.sg.isAncestor(ctx, sr.dir, tip, candidate)
			if err != nil || !ok {
				return reject(ReasonSourceFailed, "the commit %s of source %s does not descend from the expected tip %s", candidate, src.Path, tip)
			}
			if !permits(patterns, *src.Branch) {
				return reject(ReasonSourceFailed, "the attempt's Git grant does not allow %s", *src.Branch)
			}
			plan.updates = append(plan.updates, plannedUpdate{ref: *src.Branch, new: candidate, old: tip, lease: true})
			plan.advance = candidate
		}
	}

	// The branch the command left checked out, when the grant allows it.
	if headRef != "" && (src.Branch == nil || headRef != *src.Branch) && permits(patterns, headRef) &&
		head != src.Commit && head != string(src.HeadCommit) {
		plan.updates = append(plan.updates, plannedUpdate{ref: headRef, new: head})
	}
	if len(plan.updates) == 0 {
		return nil, nil, nil
	}
	return plan, nil, nil
}

// push performs one atomic push and reads its outcome back: which refs Git
// updated, and the push record they were recorded under.
func (a *attempt) push(ctx context.Context, p *plannedPush) ([]runtimev3.CommitOutput, *verdict, *abortError) {
	reject := func(reason, format string, args ...any) ([]runtimev3.CommitOutput, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(reason, nil)
		return nil, &v, nil
	}
	base, err := a.s.cfg.serviceBase(a.c3.Git.BaseURL, p.url)
	if err != nil {
		return reject(ReasonSourceFailed, "%v", err)
	}
	cfgArgs, err := a.s.cfg.credentialConfig(base, p.scopeURL, p.credFile)
	if err != nil {
		return reject(ReasonPublicationFailed, "%v", err)
	}
	g := *p.sr.sg.git
	g.cfg = append(append([]string{}, g.cfg...), cfgArgs...)
	g.redactors = a.gitRedactors()
	if a.c3.Git.CAFile != nil {
		ca, err := a.s.cfg.gitFile(*a.c3.Git.CAFile)
		if err != nil {
			return reject(ReasonPublicationFailed, "%v", err)
		}
		g.caFile = ca
	}

	args := []string{"push", "--porcelain", "--atomic", "--no-verify", "--recurse-submodules=no"}
	for _, u := range p.updates {
		if u.lease {
			args = append(args, "--force-with-lease="+u.ref+":"+u.old)
		}
	}
	args = append(args, p.url)
	for _, u := range p.updates {
		args = append(args, u.new+":"+u.ref)
	}

	var (
		statuses []pushStatus
		ids      []string
		runErr   error
	)
	backoff := a.s.cfg.SettleBackoff
	unknownBefore := false // an earlier try ended without a verdict: it may have applied
	for try := 0; try < 3; try++ {
		if try > 0 {
			if err := sleep(ctx, backoff); err != nil {
				return nil, nil, &abortError{ExitFinalization, err}
			}
			backoff = min(backoff*2, maxSettleBackoff)
		}
		var stdout, stderr string
		stdout, stderr, runErr = g.push(ctx, p.sr.dir, args...)
		statuses, ids = parsePorcelain(stdout), parsePushIDs(stderr)
		if ab := a.classify(ctx, runErr); runErr != nil && ab != nil {
			return nil, nil, ab
		}
		if runErr == nil || hasRejection(statuses) {
			if runErr != nil {
				a.note("git push for %s: %s", p.repositoryID, tail(stderr))
			}
			break
		}
		// No verdict from the service: the receive may or may not have applied. The
		// identical push is repeated (idempotent: an applied ref reports up to date).
		unknownBefore = true
		a.note("git push for %s did not complete (try %d): %s", p.repositoryID, try+1, tail(stderr))
		a.s.log.Warn("git push outcome unknown; will repeat the identical push", "repository", p.repositoryID, "try", try+1)
	}
	byRef := map[string]pushStatus{}
	for _, s := range statuses {
		byRef[s.ref] = s
	}
	if hasRejection(statuses) {
		// A request whose answer was lost (Git's own transport retry, or our repeat) may
		// have applied; the repeat then finds the ref moved by that very update and the
		// lease refuses it. Ask the service where the refs are before calling it a
		// conflict.
		for _, u := range p.updates {
			out, err := g.run(ctx, p.sr.dir, nil, "ls-remote", p.url, u.ref)
			if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), u.new+"\t") {
				return reject(ReasonPublicationUnsettled, "the push to repository %s was applied by a try whose response was lost, so its push record is unknown", p.repositoryID)
			}
		}
	}
	if hasRejection(statuses) {
		for _, u := range p.updates {
			if s := byRef[u.ref]; s.flag == '!' {
				reason := ReasonPublicationFailed
				if s.conflict() {
					reason = ReasonPublicationConflict
				}
				return reject(reason, "the push of %s to repository %s was rejected: %s", u.ref, p.repositoryID, s.summary)
			}
		}
		return reject(ReasonPublicationFailed, "the push to repository %s was rejected", p.repositoryID)
	}
	if runErr != nil {
		return reject(ReasonPublicationUnsettled, "the push to repository %s did not complete and its outcome is unknown; it may still have been applied", p.repositoryID)
	}
	var outputs []runtimev3.CommitOutput
	applied, upToDate := 0, 0
	for _, u := range p.updates {
		s, ok := byRef[u.ref]
		switch {
		case !ok:
			return reject(ReasonPublicationUnsettled, "the push to repository %s reported no status for %s", p.repositoryID, u.ref)
		case s.flag == '=':
			upToDate++
		default:
			applied++
			outputs = append(outputs, runtimev3.CommitOutput{RepositoryID: p.repositoryID, Ref: u.ref, Commit: runtimev3.GitCommit(u.new)})
		}
	}
	if unknownBefore && upToDate > 0 {
		// A try whose answer was lost applied the update; the repeat only found the ref
		// already there, and Git does not say which push record made it.
		return reject(ReasonPublicationUnsettled, "the push to repository %s was applied by a try whose response was lost, so its push record is unknown", p.repositoryID)
	}
	if applied > 0 {
		id, why := pickPushID(ids)
		if why != "" {
			return reject(ReasonPublicationUnsettled, "the push to repository %s was applied but its push record is unknown: %s", p.repositoryID, why)
		}
		for i := range outputs {
			outputs[i].PushID = id
		}
	}
	if upToDate > 0 && applied == 0 {
		a.s.log.Info("push found the remote already up to date", "repository", p.repositoryID)
	}
	if p.advance != "" && applied > 0 {
		// The published commit is now this source's head: the commit the checkout is
		// left detached at when the attempt ends (restoreHeads). The next context
		// states it as headCommit only after a SUCCEEDED attempt; if this attempt
		// ends failed after the push applied (another source's push rejected, the
		// result refused), the successor's verification refuses the workspace, by
		// design (README.md, "Source HEADs between attempts").
		p.sr.next = p.advance
	}
	return outputs, nil, nil
}

func hasRejection(statuses []pushStatus) bool {
	for _, s := range statuses {
		if s.flag == '!' {
			return true
		}
	}
	return false
}

func pickPushID(ids []string) (string, string) {
	seen := map[string]bool{}
	var out []string
	for _, id := range ids {
		if !pushIDValue.MatchString(id) {
			return "", "the service reported a malformed push record ID"
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	switch len(out) {
	case 0:
		return "", "the service did not report one (remote: kodama-push-id: <id>)"
	case 1:
		return out[0], ""
	}
	return "", "the service reported several"
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2048 {
		s = "…" + s[len(s)-2048:]
	}
	return s
}

// serviceBase checks that the repository URL lies beneath the delivered base URL
// (so the helper scoped to the base can never offer the credential elsewhere) and
// returns the base.
func (c Config) serviceBase(base, repoURL string) (string, error) {
	b, err := parseScopeURL(base, "the Git base URL")
	if err != nil {
		return "", err
	}
	r, err := parseScopeURL(repoURL, "the repository URL")
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(b.Host, r.Host) || !(r.Path == b.Path || strings.HasPrefix(r.Path, b.Path+"/")) {
		return "", fmt.Errorf("the repository URL is not beneath the Git base URL")
	}
	return strings.TrimRight(base, "/"), nil
}

// pushStatus is one `git push --porcelain` line.
type pushStatus struct {
	flag    byte // ' ' fast-forward, '+' forced, '*' new, '-' deleted, '=' up to date, '!' rejected
	ref     string
	summary string
}

// conflict reports a rejection that says the remote moved (a lease or fast-forward
// failure) rather than a refusal.
func (s pushStatus) conflict() bool {
	return strings.Contains(s.summary, "stale info") || strings.Contains(s.summary, "non-fast-forward") ||
		strings.Contains(s.summary, "fetch first") || strings.Contains(s.summary, "already exists")
}

// parsePorcelain reads the per-ref lines (`<flag>\t<from>:<to>\t<summary>`).
func parsePorcelain(out string) []pushStatus {
	var res []pushStatus
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 || line[1] != '\t' || !strings.ContainsRune(" +-*=!", rune(line[0])) {
			continue
		}
		fields := strings.SplitN(line[2:], "\t", 2)
		_, to, ok := strings.Cut(fields[0], ":")
		if !ok {
			continue
		}
		s := pushStatus{flag: line[0], ref: to}
		if len(fields) == 2 {
			s.summary = fields[1]
		}
		res = append(res, s)
	}
	return res
}

func parsePushIDs(stderr string) []string {
	var ids []string
	for _, m := range pushIDLine.FindAllStringSubmatch(stderr, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// push runs `git push` and returns its stdout (the porcelain report) and stderr
// (the service's `remote:` lines), both bounded and redacted.
func (g *git) push(ctx context.Context, dir string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, g.bin, append(g.base(), args...)...)
	cmd.Dir = dir
	cmd.Env = g.env()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limitedBuffer{buf: &stdout, max: 64 << 10}, &limitedBuffer{buf: &stderr, max: 64 << 10}
	g.confine(cmd)
	err := cmd.Run()
	return g.redact(stdout.String()), g.redact(stderr.String()), err
}

// ---- runtime-issued grants (seed and integration) ----

// planRuntimeGrants plans the pushes of the seed and integration grants the command
// obtained through the grant broker (one update each); the claims of every grant
// issued are already recorded for the result check.
func (a *attempt) planRuntimeGrants(ctx context.Context) ([]*plannedPush, *verdict, *abortError) {
	a.grantMu.Lock()
	seed, integ := a.grants[gitv1.GrantSeed], a.grants[gitv1.GrantIntegration]
	a.grantMu.Unlock()
	if a.integrationConflicted() {
		// The command went on after the control plane refused the integration: the
		// approval no longer applies and nothing was integrated.
		a.note("the integration was refused: the approved base is no longer the default branch's tip")
		v := failed(ReasonPublicationConflict, nil)
		return nil, &v, nil
	}
	var plans []*plannedPush
	if seed != nil {
		p, v, ab := a.planSeed(ctx, seed, seed.CredFile)
		if v != nil || ab != nil {
			return nil, v, ab
		}
		plans = append(plans, p)
	}
	if integ != nil {
		p, v, ab := a.planIntegration(ctx, integ, integ.CredFile)
		if v != nil || ab != nil {
			return nil, v, ab
		}
		plans = append(plans, p)
	}
	return plans, nil, nil
}

// planSeed pushes the commit at the HEAD of the directory the command named to the
// default branch the seed grant creates: the receive's only update. The history is
// all new to the service, so all of it is scanned.
func (a *attempt) planSeed(ctx context.Context, st *grantState, cred string) (*plannedPush, *verdict, *abortError) {
	reject := func(format string, args ...any) (*plannedPush, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(ReasonSourceFailed, nil)
		return nil, &v, nil
	}
	cfg := a.s.cfg
	dir, err := filepath.Abs(st.Dir)
	if err != nil || !within(cfg.WorkspaceDir, dir) || dir == filepath.Clean(cfg.WorkspaceDir) && len(a.c3.Workspace.Sources) > 0 {
		return reject("the seed directory %q is not inside the run workspace", st.Dir)
	}
	if rel, err := filepath.Rel(cfg.WorkspaceDir, dir); err != nil || checkSourceDir(cfg.WorkspaceDir, filepath.ToSlash(rel)) != nil {
		return reject("the seed directory %q is not a plain directory of the run workspace", st.Dir)
	}
	if _, err := cfg.serviceBase(a.c3.Git.BaseURL, st.RepositoryURL); err != nil {
		return reject("the seed repository URL: %v", err)
	}
	sg, err := newSealedGit(ctx, a.s.git, dir, cfg.ScratchDir, nil)
	if err != nil {
		return reject("the seed repository cannot be used: %v", err)
	}
	sr := &sourceRepo{dir: dir, sg: sg}
	a.srcs = append(a.srcs, sr) // closed with the others
	head, err := sg.sync(ctx)
	if err != nil {
		return reject("the seed repository cannot be read: %v", err)
	}
	ref := st.Grant.Repositories[0].Update[0]
	return &plannedPush{
		sr: sr, repositoryID: st.RepositoryID, url: st.RepositoryURL, credFile: cred, scopeURL: st.RepositoryURL,
		updates: []plannedUpdate{{ref: ref, new: head}},
	}, nil, nil
}

// planIntegration pushes the approved candidate to the integration ref, from the
// approved base, from the write source of that repository: exactly the update the
// grant's constraint allows.
func (a *attempt) planIntegration(ctx context.Context, st *grantState, cred string) (*plannedPush, *verdict, *abortError) {
	reject := func(format string, args ...any) (*plannedPush, *verdict, *abortError) {
		a.note(format, args...)
		v := failed(ReasonSourceFailed, nil)
		return nil, &v, nil
	}
	in := st.Grant.Repositories[0].Integration
	var sr *sourceRepo
	for _, s := range a.srcs {
		if s.src.RepositoryID == st.RepositoryID && s.src.Access == gitv1.AccessWrite {
			sr = s
			break
		}
	}
	if sr == nil {
		return reject("the integration grant names repository %s, which has no write source in this run", st.RepositoryID)
	}
	if !sr.sg.hasCommit(ctx, in.To) {
		return reject("the approved candidate %s is not in the workspace of source %s", in.To, sr.src.Path)
	}
	return &plannedPush{
		sr: sr, repositoryID: st.RepositoryID, url: sr.src.RepositoryURL, credFile: cred, scopeURL: sr.src.RepositoryURL,
		updates: []plannedUpdate{{ref: in.Ref, new: in.To, old: in.From, lease: true}},
		bases:   []string{in.From},
	}, nil, nil
}
