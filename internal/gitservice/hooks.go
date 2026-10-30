package gitservice

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	gitv1 "github.com/optimiware/kodama/contracts/git/v1"
)

// The hooks are server-owned: core.hooksPath (command scope) points every Git
// process the worker starts at one directory of shims, each of which runs
// `<kodama binary> git-hook <name>`. The binary's `git-hook` command (T-132)
// calls HookMain. Pushed content and repository-local configuration cannot
// replace them (GR-08).

// Hook names the worker installs.
const (
	HookPreReceive           = "pre-receive"
	HookReferenceTransaction = "reference-transaction"
	HookPostReceive          = "post-receive"
)

// HookNames lists the installed hooks.
func HookNames() []string {
	return []string{HookPreReceive, HookReferenceTransaction, HookPostReceive}
}

// HookCommand is the command line the shim of a hook runs: the Kodama binary's
// `git-hook` command with the hook's name (Git appends its own arguments).
func HookCommand(binary, name string) []string { return []string{binary, "git-hook", name} }

// HookScript returns the shim of a hook: it replaces the shell with the Kodama
// binary, so Git's stdin, arguments and exit status pass through unchanged.
func HookScript(binary, name string) []byte {
	cmd := HookCommand(binary, name)
	quoted := "'" + strings.ReplaceAll(cmd[0], "'", `'\''`) + "'"
	return []byte("#!/bin/sh\n# Server-owned Kodama hook (generated at worker start; do not edit).\nexec " + quoted + " " + cmd[1] + " " + cmd[2] + " \"$@\"\n")
}

// InstallHooks (re)writes the hook shims into dir, atomically and executable.
func InstallHooks(dir, binary string) error {
	if !filepath.IsAbs(binary) {
		return errors.New("gitservice: the hook binary must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, name := range HookNames() {
		if err := writeFileAtomic(filepath.Join(dir, name), HookScript(binary, name), 0o700); err != nil {
			return err
		}
	}
	return syncDir(dir)
}

// Environment the worker sets on every Git process it starts for a receive or
// a server-side ref operation, and the hooks read. Only the worker sets them
// (clients cannot influence a Git server process's environment).
const (
	envHookAddr    = "KODAMA_HOOK_ADDR"
	envHookToken   = "KODAMA_HOOK_TOKEN"
	envHookKind    = "KODAMA_HOOK_KIND" // "receive" or "operation"
	envReceiveID   = "KODAMA_RECEIVE_ID"
	envOperationID = "KODAMA_OPERATION_ID"
	envRepoID      = "KODAMA_REPOSITORY_ID"
	envRepoDir     = "KODAMA_REPOSITORY_DIR"
	envGrantID     = "KODAMA_GRANT_ID"  // informational: the worker adds the grant to the authorization itself
	envTenantID    = "KODAMA_TENANT_ID" // informational
	envGitBinary   = "KODAMA_GIT_BINARY"
	envHookTimeout = "KODAMA_HOOK_TIMEOUT" // seconds

	kindReceive   = "receive"
	kindOperation = "operation"
)

// Bounds of what a hook reads from Git.
const (
	maxHookUpdates = 4 * gitv1.MaxPushUpdates
	maxHookBody    = 8 << 20
)

// generationLockFD is the descriptor the worker passes its generation lock as
// (cmd.ExtraFiles[0] is descriptor 3 in the child).
const generationLockFD = 3

var generationLockFile = sync.OnceValue(func() *os.File {
	f := os.NewFile(generationLockFD, "generation-lock")
	if f == nil {
		return nil
	}
	if _, err := f.Stat(); err != nil { // not open: the hook was not started by the worker's Git
		return nil
	}
	return f // kept for the life of the process: a finalizer must not close it
})

// generationLock returns the inherited generation lock, or nil.
func generationLock() *os.File { return generationLockFile() }

// HookMain is the entry point of `kodama git-hook <name> [args...]`: args[0] is
// the hook name, the rest are Git's hook arguments. It returns the process exit
// status; Git aborts the operation on a non-zero status from pre-receive and
// from the prepared state of reference-transaction.
func HookMain(args []string, stdin io.Reader, stdout, stderr io.Writer, environ []string) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: kodama git-hook <pre-receive|reference-transaction|post-receive> [args...]")
		return 2
	}
	return RunHook(context.Background(), args[0], args[1:], stdin, stdout, stderr, environ)
}

// RunHook runs one hook with Git's stdin, its arguments and the environment
// Git gave the hook process.
func RunHook(ctx context.Context, name string, args []string, stdin io.Reader, stdout, stderr io.Writer, environ []string) int {
	h := newHook(ctx, stdin, stdout, stderr, environ)
	switch name {
	case HookPreReceive:
		return h.preReceive()
	case HookReferenceTransaction:
		return h.referenceTransaction(args)
	case HookPostReceive:
		return h.postReceive()
	}
	fmt.Fprintf(stderr, "kodama git-hook: unknown hook %q\n", name)
	return 2
}

type hook struct {
	ctx            context.Context
	stdin          io.Reader
	stdout, stderr io.Writer
	env            []string

	addr, token, kind            string
	receiveID, operationID       string
	repositoryID, dir, gitBinary string
	timeout                      time.Duration
	client                       *http.Client
}

func newHook(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, environ []string) *hook {
	get := func(key string) string {
		for i := len(environ) - 1; i >= 0; i-- {
			if v, ok := strings.CutPrefix(environ[i], key+"="); ok {
				return v
			}
		}
		return ""
	}
	h := &hook{
		ctx: ctx, stdin: stdin, stdout: stdout, stderr: stderr, env: environ,
		addr: get(envHookAddr), token: get(envHookToken), kind: get(envHookKind),
		receiveID: get(envReceiveID), operationID: get(envOperationID),
		repositoryID: get(envRepoID), dir: get(envRepoDir), gitBinary: get(envGitBinary),
		timeout: 2 * time.Minute,
	}
	if h.gitBinary == "" {
		h.gitBinary = "git"
	}
	if n, err := strconv.Atoi(get(envHookTimeout)); err == nil && n > 0 {
		h.timeout = time.Duration(n) * time.Second
	}
	// A zero Transport has no proxy: the hook only talks to the loopback worker.
	h.client = &http.Client{Timeout: h.timeout, Transport: &http.Transport{DisableKeepAlives: true}}
	return h
}

// maxDiagnostic bounds one diagnostic line.
const maxDiagnostic = 1000

// sanitizeLine makes text that may come from outside the hook (a control-plane
// message, a ref name, Git's or the filesystem's error text) safe as one physical
// line of stderr: control characters, line and paragraph separators and invalid
// UTF-8 are escaped, the length is bounded, and the client-visible marker
// `kodama-push-id:` is defused, so no diagnostic can start or forge the line
// printPushID alone writes.
func sanitizeLine(s string) string {
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < maxDiagnostic; {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsControl(r) || r == '\u2028' || r == '\u2029':
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return strings.ReplaceAll(b.String(), "kodama-push-id:", `kodama-push-id\:`)
}

// diagf writes one diagnostic: a single physical line, always starting with
// `kodama: `, with every interpolated value sanitized.
func (h *hook) diagf(format string, args ...any) {
	fmt.Fprintf(h.stderr, "kodama: %s\n", sanitizeLine(fmt.Sprintf(format, args...)))
}

// printPushID is the only writer of the client-visible push ID line. The ID must
// have the opaque-ID syntax; anything else is not printed.
func (h *hook) printPushID(id string) {
	if !repositoryIDPattern.MatchString(id) {
		h.diagf("the push ID is not valid; not printed")
		return
	}
	fmt.Fprintf(h.stderr, "%s%s\n", PushIDLinePrefix, id)
}

func (h *hook) failf(format string, args ...any) int {
	h.diagf(format, args...)
	return 1
}

// attribution returns who the hook's transactions belong to, or an error when
// the process was not started by the worker (no ref may change then).
func (h *hook) attribution() (attribution, error) {
	a := attribution{receiveID: h.receiveID, operationID: h.operationID}
	switch {
	case h.dir == "" || !filepath.IsAbs(h.dir):
		return a, errors.New("the repository was not opened by the Git worker")
	case h.kind == kindReceive && h.receiveID != "" && h.operationID == "" && h.addr != "" && h.token != "":
	case h.kind == kindOperation && h.operationID != "" && h.receiveID == "":
	default:
		return a, errors.New("ref changes are accepted only through the Git worker")
	}
	return a, nil
}

// readUpdates parses Git's `<old> <new> <ref>` lines.
func (h *hook) readUpdates() ([]gitv1.RefUpdate, error) {
	var updates []gitv1.RefUpdate
	sc := bufio.NewScanner(io.LimitReader(h.stdin, maxHookBody))
	sc.Buffer(make([]byte, 0, 4096), 4096)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 3 {
			return nil, fmt.Errorf("unexpected update line %q", truncateText(line, 80))
		}
		u := gitv1.RefUpdate{Old: parts[0], New: parts[1], Ref: parts[2]}
		if len(updates) >= maxHookUpdates {
			return nil, contractError(gitv1.CodeTooManyUpdates, "a receive may update at most %d refs; push the refs in batches", gitv1.MaxPushUpdates)
		}
		if err := u.Validate(); err != nil {
			return nil, fmt.Errorf("ref %q: %v", truncateText(u.Ref, 80), err)
		}
		updates = append(updates, u)
	}
	return updates, sc.Err()
}

// git runs Git against the repository with the hook's own environment: Git
// gave it the quarantine of the incoming objects, which pre-receive needs.
func (h *hook) git(args ...string) (stdout []byte, exit int, err error) {
	cmd := exec.CommandContext(h.ctx, h.gitBinary, append([]string{"--git-dir=" + h.dir}, args...)...)
	cmd.Env = h.env
	// The generation lock reaches the hook as descriptor 3, inherited from Git;
	// os/exec would drop it, so it is passed on explicitly.
	if f := generationLock(); f != nil {
		cmd.ExtraFiles = []*os.File{f}
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err = cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return out.Bytes(), exitErr.ExitCode(), nil
	}
	return out.Bytes(), 0, err
}

// refValue reads the exact current value of ref ("" when absent). It lists with
// for-each-ref and keeps only the exact name: rev-parse would resolve a short
// name through other namespaces.
func (h *hook) refValue(ref string) (string, error) {
	out, exit, err := h.git("for-each-ref", "--format=%(refname) %(objectname)", ref)
	if err != nil || exit != 0 {
		return "", fmt.Errorf("reading %s: %v (exit %d)", ref, err, exit)
	}
	return exactRefValue(out, ref), nil
}

func exactRefValue(out []byte, ref string) string {
	for _, line := range strings.Split(string(out), "\n") {
		if name, oid, ok := strings.Cut(line, " "); ok && name == ref {
			return oid
		}
	}
	return ""
}

// call posts to the worker's hook listener.
func (h *hook) call(path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(h.ctx, http.MethodPost, "http://"+h.addr+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return fmt.Errorf("the Git worker did not answer: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxHookBody))
	if err != nil {
		return fmt.Errorf("the Git worker's answer was cut off: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		var e gitv1.ErrorResponse
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			return &hookRefusal{Code: string(e.Error.Code), Message: e.Error.Message}
		}
		return fmt.Errorf("the Git worker answered %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// hookRefusal is a refusal the worker (or the control plane behind it) gave.
type hookRefusal struct{ Code, Message string }

func (r *hookRefusal) Error() string { return r.Code + ": " + r.Message }

// ---- pre-receive ----

func (h *hook) preReceive() int {
	if _, err := h.attribution(); err != nil {
		return h.failf("push refused: %v", err)
	}
	if h.kind != kindReceive {
		return h.failf("push refused: not a receive")
	}
	updates, err := h.readUpdates()
	if err != nil {
		return h.failf("push refused: %v", err)
	}
	if len(updates) == 0 {
		return 0
	}
	out, exit, err := h.git("for-each-ref", "--count=1", "--format=%(refname)")
	if err != nil || exit != 0 {
		return h.failf("push refused: cannot read the repository's refs")
	}
	empty := len(bytes.TrimSpace(out)) == 0
	facts := make([]gitv1.GitFact, len(updates))
	for i, u := range updates {
		facts[i] = gitv1.GitFact{Ref: u.Ref, RepositoryEmpty: empty}
		if !gitv1.IsZeroObjectID(u.Old) && !gitv1.IsZeroObjectID(u.New) {
			// An update of an existing ref is a fast-forward only when old is an
			// ancestor of new. Anything Git cannot decide (a tag object, a missing
			// object) is conservatively not one.
			_, code, err := h.git("merge-base", "--is-ancestor", u.Old, u.New)
			if err != nil {
				return h.failf("push refused: cannot compare the history of %s", u.Ref)
			}
			ff := code == 0
			facts[i].FastForward = &ff
		}
	}
	req := hookAuthorizeRequest{Updates: updates, PushOptions: h.pushOptions(), GitFacts: facts}
	var resp gitv1.PushAuthorizeResponse
	if err := h.call("/authorize", req, &resp); err != nil {
		return h.failf("push refused: %v", refusalText(err))
	}
	denied := 0
	for _, v := range resp.Updates {
		if v.Verdict == gitv1.VerdictDeny {
			denied++
			reason := "refused"
			if v.Reason != nil {
				reason = *v.Reason
			}
			h.diagf("refused %s: %s", v.Ref, reason)
		}
	}
	if denied > 0 {
		return h.failf("push refused: %d of %d updates denied; nothing was changed", denied, len(resp.Updates))
	}
	return 0
}

func (h *hook) pushOptions() []string {
	options := []string{}
	n, err := strconv.Atoi(envValue(h.env, "GIT_PUSH_OPTION_COUNT"))
	if err != nil || n < 0 {
		return options
	}
	if n > gitv1.MaxPushOptions+1 {
		n = gitv1.MaxPushOptions + 1 // one too many: the worker refuses the request
	}
	for i := 0; i < n; i++ {
		options = append(options, envValue(h.env, "GIT_PUSH_OPTION_"+strconv.Itoa(i)))
	}
	return options
}

func envValue(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], key+"="); ok {
			return v
		}
	}
	return ""
}

func refusalText(err error) string {
	var r *hookRefusal
	if errors.As(err, &r) {
		return r.Message + " (" + r.Code + ")"
	}
	return err.Error()
}

// ---- reference-transaction ----

func (h *hook) referenceTransaction(args []string) int {
	state := ""
	if len(args) > 0 {
		state = args[0]
	}
	switch state {
	case "prepared":
		return h.prepared()
	case "committed":
		h.committed()
		return 0
	case "aborted":
		h.outcome(gitv1.JournalAborted)
		return 0
	}
	return 0 // preparing: nothing is locked yet
}

// prepared is the two-phase fence. With the refs locked by Git it appends the
// prepared entries (with the values observed now) to the fsynced journal and
// only then, for a receive, asks the control plane for commit intent. Any
// failure exits non-zero and Git aborts the transaction.
func (h *hook) prepared() int {
	a, err := h.attribution()
	if err != nil {
		return h.failf("ref update refused: %v", err)
	}
	updates, err := h.readUpdates()
	if err != nil {
		return h.failf("ref update refused: %v", err)
	}
	if len(updates) == 0 {
		return 0
	}
	observed := make([]string, len(updates))
	for i, u := range updates {
		v, err := h.refValue(u.Ref)
		if err != nil {
			return h.failf("ref update refused: %v", err)
		}
		if v == "" {
			v = gitv1.ZeroObjectID(len(u.Old))
		}
		observed[i] = v
	}
	if _, err := journalAt(h.dir).appendPrepared(a, updates, observed); err != nil {
		return h.failf("ref update refused: the ref journal could not be written: %v", err)
	}
	if h.kind != kindReceive {
		return 0 // a server operation needs no commit intent
	}
	if err := h.call("/commit-intent", hookCommitRequest{Updates: updates}, nil); err != nil {
		return h.failf("ref update refused: no commit intent: %v", refusalText(err))
	}
	return 0
}

// committed journals the outcome of a transaction Git reports as committed.
// Git also runs this state for refs it rejected inside a non-atomic receive (a
// directory/file ref conflict, for example), and other receives run
// concurrently on the same refs (they hold the mutation gate shared and Git has
// released its ref locks before this hook), so neither Git's report nor the
// ref's value read at some moment proves what this transaction did. The journal
// decides under its exclusive append lock (journal.appendVerifiedOutcome): a
// ref that a later prepared entry of any transaction followed gets no outcome,
// and the settlement decides from that entry's observed value; otherwise the
// exact current value is read while no prepared entry can be appended and is
// journaled committed (the new value), aborted (the old value) or not at all.
func (h *hook) committed() {
	a, err := h.attribution()
	if err != nil {
		return
	}
	updates, err := h.readUpdates()
	if err != nil || len(updates) == 0 {
		return
	}
	// The read runs under the journal's append lock: bound it.
	ctx, cancel := context.WithTimeout(h.ctx, h.timeout)
	defer cancel()
	rd := *h
	rd.ctx = ctx
	_, err = journalAt(h.dir).appendVerifiedOutcome(a, updates, func(ref string) (string, error) {
		v, err := rd.refValue(ref)
		if err != nil {
			h.diagf("the ref journal could not verify %s: %v", ref, err)
		}
		return v, err
	})
	if err != nil {
		h.diagf("the ref journal could not record %s: %v", gitv1.JournalCommitted, err)
	}
}

// outcome appends committed or aborted for the transaction's own pending
// prepared entries. The exit status of these states is ignored by Git, so a
// failure is only reported: the settlement then finds a prepared entry without
// an outcome and decides from the next prepared entry or the locked ref value.
func (h *hook) outcome(state gitv1.JournalState) {
	a, err := h.attribution()
	if err != nil {
		return
	}
	updates, err := h.readUpdates()
	if err != nil || len(updates) == 0 {
		return
	}
	if _, err := journalAt(h.dir).appendOutcome(a, state, updates); err != nil {
		h.diagf("the ref journal could not record %s: %v", state, err)
	}
}

// ---- post-receive ----

func (h *hook) postReceive() int {
	if _, err := h.attribution(); err != nil || h.kind != kindReceive {
		return 0
	}
	applied, err := h.readUpdates()
	if err != nil {
		h.diagf("cannot report the applied refs: %v", err)
		return 0
	}
	if applied == nil {
		applied = []gitv1.RefUpdate{}
	}
	// The worker retries the report in the background; reconciliation settles
	// from the journal if it never arrives. The pusher's push already succeeded.
	var resp hookOutcomeResponse
	if err := h.call("/outcome", hookOutcomeRequest{Applied: applied}, &resp); err != nil {
		h.diagf("the push is applied; its confirmation is delayed: %v", refusalText(err))
		return 0
	}
	if resp.Warning != "" {
		h.diagf("the push is applied; its confirmation was refused: %s", resp.Warning)
	}
	// Client-visible: Git relays this line as `remote: kodama-push-id: <pushId>`.
	// The workload entrypoint parses it (the record exists whatever happened to
	// the confirmation). The worker returns the ID once, and only for a receive
	// that applied at least one update.
	if resp.PushID != "" {
		h.printPushID(resp.PushID)
	}
	return 0
}

// ---- hook listener wire ----

type hookAuthorizeRequest struct {
	Updates     []gitv1.RefUpdate `json:"updates"`
	PushOptions []string          `json:"pushOptions"`
	GitFacts    []gitv1.GitFact   `json:"gitFacts"`
}

type hookCommitRequest struct {
	Updates []gitv1.RefUpdate `json:"updates"`
}

type hookOutcomeRequest struct {
	Applied []gitv1.RefUpdate `json:"applied"`
}

// hookOutcomeResponse answers post-receive: PushID is set (once, and only for a
// receive that applied an update) for the line the client sees; Warning carries
// a final refusal of the confirmation.
type hookOutcomeResponse struct {
	PushID  string `json:"pushId"`
	Warning string `json:"warning"`
}

// PushIDLinePrefix starts the one line post-receive prints to stderr for a
// receive that applied at least one update; Git relays it to the pusher as
// `remote: kodama-push-id: <pushId>`.
const PushIDLinePrefix = "kodama-push-id: "
