package logs_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	runtimev1 "github.com/optimiware/kodama/contracts/runtime/v1"
	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/execution"
	"github.com/optimiware/kodama/internal/execution/executiontest"
	"github.com/optimiware/kodama/internal/kube"
	"github.com/optimiware/kodama/internal/kube/kubernetes"
	"github.com/optimiware/kodama/internal/logs"
	"github.com/optimiware/kodama/internal/logs/contract"
	"github.com/optimiware/kodama/internal/pipeline"
	"github.com/optimiware/kodama/internal/pipeline/pipelinetest"
	"github.com/optimiware/kodama/internal/secrets"
	secretspg "github.com/optimiware/kodama/internal/secrets/postgres"
	"github.com/optimiware/kodama/internal/storage"
)

// stepSpec describes the one container step of a test run.
type stepSpec struct {
	Script  string
	Timeout int
	Outputs []string
	// NoHold starts the command at once. By default the command first waits
	// holdSeconds, so that the reconciler persists the observed Job UID (what
	// every log read and capture keys on) before the attempt can finish.
	NoHold bool
	// Secrets are real secrets sealed in the scope, pinned for the run and
	// delivered by the entrypoint as a file under $KODAMA_SECRETS_DIR (File) or
	// as an environment value (Env). The plaintext is Value.
	Secrets []secretUse
}

type secretUse struct {
	File, Env string
	Value     string
	name      string // the sealed secret's name
}

const holdSeconds = 6

// definition registers a single-step pipeline running the script through the
// entrypoint in the workload image.
func (s *stack) definition(scope domain.Scope, name string, st stepSpec) domain.DefinitionID {
	s.t.Helper()
	if st.Timeout == 0 {
		st.Timeout = 300
	}
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: kodama/v1alpha1\nkind: Pipeline\nmetadata: {name: %s}\nspec:\n  inputs: {}\n  steps:\n", name)
	fmt.Fprintf(&b, "    - id: work\n      kind: container\n      image: %s\n      workspace: {access: none}\n      command: [\"/bin/sh\", \"-ec\"]\n", s.c.workload)
	b.WriteString("      args:\n        - |\n")
	script := st.Script
	if !st.NoHold {
		script = fmt.Sprintf("sleep %d\n", holdSeconds) + script
	}
	for _, l := range strings.Split(strings.TrimRight(script, "\n"), "\n") {
		b.WriteString("          " + l + "\n")
	}
	fmt.Fprintf(&b, "      timeoutSeconds: %d\n", st.Timeout)
	var env, files []string
	for _, u := range st.Secrets {
		if u.Env != "" {
			env = append(env, fmt.Sprintf("          %s: {secretRef: \"secret://project/%s\"}\n", u.Env, u.name))
		}
		if u.File != "" {
			files = append(files, fmt.Sprintf("          %s: \"secret://project/%s\"\n", u.File, u.name))
		}
	}
	if len(env) > 0 {
		b.WriteString("      env:\n" + strings.Join(env, ""))
	}
	if len(files) > 0 {
		b.WriteString("      secretFiles:\n" + strings.Join(files, ""))
	}
	if len(st.Outputs) > 0 {
		b.WriteString("      outputs:\n")
		for _, o := range st.Outputs {
			fmt.Fprintf(&b, "        %s: {type: string}\n", o)
		}
	}
	return s.define(scope, b.String()).ID
}

// att is one admitted attempt of a one-step run.
type att struct {
	Scope  domain.Scope
	Run    domain.RunID
	ID     domain.AttemptID
	Job    string
	Intent *execution.AttemptSpec
	// Dispatched is false for an attempt claimed but never handed to the executor.
	Dispatched bool
}

// admit admits an internal cleanup run (project-scoped, workspace access none,
// bound to a recorded deployment: the entrypoint needs no Git checkout) and
// claims its step. Nothing else in execution, runtime admission or the executor
// differs from a work run. Faults can be armed before dispatch.
func (s *stack) admit(scope domain.Scope, st stepSpec) *att {
	s.t.Helper()
	n := s.ids.Add(1)
	st.Secrets = append([]secretUse(nil), st.Secrets...)
	for i := range st.Secrets {
		st.Secrets[i].name = fmt.Sprintf("sec-%s-%d", Prefix, s.ids.Add(1))
		s.vault.create(s.t, s, scope, st.Secrets[i].name, st.Secrets[i].Value)
		s.extraMu.Lock()
		if s.sealed == nil {
			s.sealed = map[string]bool{}
		}
		s.sealed[st.Secrets[i].name] = true
		s.extraMu.Unlock()
	}
	id := s.definition(scope, fmt.Sprintf("%s-%d", Prefix, n), st)
	run, err := s.plain.AdmitCleanup(context.Background(), execution.CleanupCommand{Scope: scope, DeploymentID: fmt.Sprintf("dep_%s_%d", Prefix, n), DefinitionID: id})
	must(s.t, err)
	in, err := s.plain.ClaimRunnable(context.Background(), scope.TenantID)
	if err != nil {
		s.t.Fatalf("claim: %v", err)
	}
	if in.RunID != run.ID {
		s.t.Fatalf("claimed run %s, want %s (another run is runnable)", in.RunID, run.ID)
	}
	a := &att{Scope: scope, Run: run.ID, ID: in.AttemptID, Job: in.WorkloadName, Intent: in}
	if len(st.Secrets) > 0 {
		s.pinSecrets(a)
	}
	for _, u := range st.Secrets {
		s.addDelivered(in.AttemptID, u.Value) // what the attempt holds
	}
	return a
}

// addDelivered records a value the attempt was given, for the redaction source.
func (s *stack) addDelivered(attempt domain.AttemptID, v string) {
	s.extraMu.Lock()
	defer s.extraMu.Unlock()
	if s.extra == nil {
		s.extra = map[domain.AttemptID][][]byte{}
	}
	s.extra[attempt] = append(s.extra[attempt], []byte(v))
}

// dispatch hands the claimed attempt to the real executor and records the
// confirmation, as execution's own dispatcher does.
func (s *stack) dispatch(a *att) {
	s.t.Helper()
	ctx := context.Background()
	must(s.t, startErr(s.runtime.Start(ctx, *a.Intent)))
	must(s.t, s.plain.ObserveAttempt(ctx, a.Scope, a.ID, execution.Observation{
		Kind: execution.ObservedDispatched, WorkloadName: a.Job, SpecFingerprint: a.Intent.SpecFingerprint, ObservedAt: time.Now(),
	}))
	a.Dispatched = true
}

// start admits and dispatches. Faults are armed first, so they are in place
// before the Pod's first request.
func (s *stack) start(scope domain.Scope, st stepSpec, faults ...faultSpec) *att {
	s.t.Helper()
	a := s.admit(scope, st)
	for _, f := range faults {
		f.Attempt = a.ID
		s.srv.inject(f)
	}
	s.dispatch(a)
	if !st.NoHold {
		s.waitObserved(a, 5*time.Minute)
	}
	return a
}

// waitObserved drives the reconciliation loops until execution persisted the
// attempt's observed Job UID.
func (s *stack) waitObserved(a *att, timeout time.Duration) {
	s.t.Helper()
	s.until(timeout, "execution to observe the Job", func() (bool, string) {
		at := s.persisted(a)
		return at.WorkloadUID != "" || at.Status.Terminal(), "Job not observed: " + string(at.Status)
	})
	if s.persisted(a).WorkloadUID == "" {
		at := s.persisted(a)
		if pods, err := s.c.client.ListPods(context.Background(), s.ns(a.Scope.TenantID), kube.LabelJob+"="+a.Job); err == nil && len(pods) == 1 {
			l, _ := s.c.api.rawLog(context.Background(), s.ns(a.Scope.TenantID), pods[0].Metadata.Name, kube.PrimaryContainer)
			s.t.Logf("container log:\n%s", l)
		}
		s.t.Fatalf("the attempt finished (%s %s/%s) before its Job was observed (hold too short, or it failed early): notes %q events %+v", at.Status, at.Class, at.Reason, s.notes(a), s.srv.eventsOf(a.ID, ""))
	}
}

// tick is one iteration of the periodic loops the root composition schedules:
// execution's own reconciliation and the Kubernetes reconciliation pass.
func (s *stack) tick() {
	s.t.Helper()
	ctx := context.Background()
	if _, err := s.plain.Reconcile(ctx); err != nil {
		s.t.Logf("execution reconcile: %v", err)
	}
	if _, err := s.rec.Pass(ctx); err != nil {
		s.t.Logf("kube pass: %v", err)
	}
}

// until ticks the loops until cond holds.
func (s *stack) until(timeout time.Duration, what string, cond func() (bool, string)) {
	s.t.Helper()
	eventually(s.t, timeout, what, func() (bool, string) {
		if ok, _ := cond(); ok {
			return true, ""
		}
		s.tick()
		return cond()
	})
}

func (s *stack) persisted(a *att) *execution.Attempt {
	s.t.Helper()
	agg, err := s.x.Store.Get(context.Background(), a.Scope, a.Run)
	must(s.t, err)
	at := agg.Attempt(a.ID)
	if at == nil {
		s.t.Fatal("attempt not found")
	}
	return at
}

func (s *stack) aggregate(a *att) *execution.Aggregate {
	s.t.Helper()
	agg, err := s.x.Store.Get(context.Background(), a.Scope, a.Run)
	must(s.t, err)
	return agg
}

// waitTerminal waits until the attempt's result was accepted (or it ended
// otherwise) and returns it. Settlement may follow; the finalization barrier the
// log join needs (the wrapper's upload and the result both happened) is the
// attempt's terminal status.
func (s *stack) waitTerminal(a *att, timeout time.Duration) *execution.Attempt {
	s.t.Helper()
	s.until(timeout, "the attempt to finish", func() (bool, string) {
		at := s.persisted(a)
		return at.Status.Terminal(), fmt.Sprintf("status %s (%s/%s)", at.Status, at.Class, at.Reason)
	})
	return s.persisted(a)
}

// waitRunning waits until execution observed the Job (its UID is persisted) and
// the primary container of the Pod runs.
func (s *stack) waitRunning(a *att, timeout time.Duration) {
	s.t.Helper()
	s.until(timeout, "the workload to run", func() (bool, string) {
		at := s.persisted(a)
		if at.WorkloadUID == "" {
			return false, "Job not observed"
		}
		pods, err := s.c.client.ListPods(context.Background(), s.ns(a.Scope.TenantID), kube.LabelJob+"="+a.Job)
		if err != nil || len(pods) != 1 {
			return false, fmt.Sprintf("pods %d %v", len(pods), err)
		}
		for _, cs := range pods[0].Status.ContainerStatuses {
			if cs.Name == kube.PrimaryContainer && cs.State.Running != nil {
				return true, ""
			}
		}
		return false, "container not running: " + pods[0].Status.Phase
	})
}

func (s *stack) pod(a *att) kubernetes.Pod {
	s.t.Helper()
	pods, err := s.c.client.ListPods(context.Background(), s.ns(a.Scope.TenantID), kube.LabelJob+"="+a.Job)
	must(s.t, err)
	if len(pods) != 1 {
		s.t.Fatalf("%d Pods for %s", len(pods), a.Job)
	}
	return pods[0]
}

// rawLog is the container log exactly as Kubernetes holds it (not through the
// log module).
func (s *stack) rawLog(a *att) string {
	s.t.Helper()
	p := s.pod(a)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := s.c.api.rawLog(ctx, s.ns(a.Scope.TenantID), p.Metadata.Name, kube.PrimaryContainer)
	must(s.t, err)
	return b
}

// waitRawLog waits until the container log contains marker.
func (s *stack) waitRawLog(a *att, marker string, timeout time.Duration) string {
	s.t.Helper()
	var last string
	eventually(s.t, timeout, "the container log to contain "+marker, func() (bool, string) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		pods, err := s.c.client.ListPods(ctx, s.ns(a.Scope.TenantID), kube.LabelJob+"="+a.Job)
		if err != nil || len(pods) != 1 {
			return false, "no pod"
		}
		l, err := s.c.api.rawLog(ctx, s.ns(a.Scope.TenantID), pods[0].Metadata.Name, kube.PrimaryContainer)
		if err != nil {
			return false, err.Error()
		}
		last = l
		return strings.Contains(l, marker), truncate(l, 200)
	})
	return last
}

// ---- reading through the service and the HTTP route ----

// sinkBuf collects the records of a service read.
type sinkBuf struct {
	Meta    logs.Meta
	Lines   []string
	Cut     []bool
	Summary *logs.Summary
}

func (b *sinkBuf) Begin(m logs.Meta) error { b.Meta = m; return nil }
func (b *sinkBuf) Chunk(c logs.Chunk) error {
	b.Lines = append(b.Lines, strings.TrimSuffix(c.Text, "\n"))
	b.Cut = append(b.Cut, c.LineTruncated)
	return nil
}
func (b *sinkBuf) End(s logs.Summary) error { b.Summary = &s; return nil }

// readService reads through a log service directly.
func readService(svc *logs.Service, req logs.Request) (*sinkBuf, error) {
	sess, err := svc.Open(context.Background(), req)
	if err != nil {
		return nil, err
	}
	b := &sinkBuf{}
	if err := sess.Run(context.Background(), b); err != nil {
		return b, err
	}
	return b, nil
}

func (b *sinkBuf) joined() string { return strings.Join(b.Lines, "\n") }

// httpResult is a fully read response of the protected route.
type httpResult struct {
	status  int
	raw     string
	records []map[string]any
}

func (k *logsKit) get(user string, scope domain.Scope, run domain.RunID, query string) httpResult {
	k.s.t.Helper()
	u := k.HTTP.URL + "/api/v1/tenants/" + string(scope.TenantID) + "/projects/" + string(scope.ProjectID) + "/runs/" + string(run) + "/logs"
	if query != "" {
		u += "?" + query
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if user != "" {
		req.Header.Set("Authorization", "Bearer "+user)
	}
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	if err != nil {
		k.s.t.Fatalf("GET %s: %v", query, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	res := httpResult{status: resp.StatusCode, raw: string(b)}
	if resp.StatusCode == http.StatusOK {
		sc := bufio.NewScanner(strings.NewReader(res.raw))
		sc.Buffer(make([]byte, 1<<20), 1<<22)
		for sc.Scan() {
			var rec map[string]any
			if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
				k.s.t.Fatalf("a response line is not JSON: %q", sc.Text())
			}
			res.records = append(res.records, rec)
		}
	}
	return res
}

func (r httpResult) meta(t testing.TB) map[string]any {
	t.Helper()
	if len(r.records) == 0 || r.records[0]["type"] != "meta" {
		t.Fatalf("the first record must be meta: %s", truncate(r.raw, 300))
	}
	return r.records[0]
}

func (r httpResult) summary(t testing.TB) map[string]any {
	t.Helper()
	if len(r.records) == 0 || r.records[len(r.records)-1]["type"] != "summary" {
		t.Fatalf("the last record must be the summary (its absence means interruption): %s", truncate(r.raw, 300))
	}
	return r.records[len(r.records)-1]
}

func (r httpResult) lines() (out []string) {
	for _, rec := range r.records {
		if rec["type"] == "chunk" {
			out = append(out, strings.TrimSuffix(fmt.Sprint(rec["text"]), "\n"))
		}
	}
	return
}

func (r httpResult) errorCode(t testing.TB) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.raw), &e); err != nil {
		t.Fatalf("not an error envelope (%d): %s", r.status, truncate(r.raw, 300))
	}
	return e.Error.Code
}

// ---- retained evidence ----

// artifactNames lists the attempt's diagnostic artifacts by name (owner role).
func (s *stack) artifactNames(a *att, like string) []string {
	s.t.Helper()
	rows, err := s.x.Migration.Query(context.Background(), `SELECT name FROM artifacts WHERE attempt_id = $1 AND name LIKE $2 ORDER BY name`, string(a.ID), like)
	must(s.t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		must(s.t, rows.Scan(&n))
		out = append(out, n)
	}
	return out
}

func (k *logsKit) readArtifact(a *att, ref logs.ArtifactRef) string {
	k.s.t.Helper()
	rc, err := k.Reader.Open(context.Background(), a.Scope, a.ID, ref)
	if err != nil {
		k.s.t.Fatalf("open %s: %v", ref.ID, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	must(k.s.t, err)
	return string(b)
}

func (k *logsKit) sidecar(a *att, ref logs.ArtifactRef) contract.Metadata {
	k.s.t.Helper()
	m, err := contract.Decode([]byte(k.readArtifact(a, ref)))
	if err != nil {
		k.s.t.Fatalf("sidecar: %v", err)
	}
	return m
}

func (k *logsKit) records(a *att) []*logs.Record {
	k.s.t.Helper()
	recs, err := k.Store.ForAttempt(context.Background(), a.Scope, a.ID)
	must(k.s.t, err)
	return recs
}

func (k *logsKit) record(a *att, src logs.Source) *logs.Record {
	k.s.t.Helper()
	var out *logs.Record
	for _, r := range k.records(a) {
		if r.Source == src && (out == nil || r.Generation > out.Generation) {
			out = r
		}
	}
	return out
}

// snapshot is what execution recorded about the attempt; log collection must
// never change it.
type snapshot struct {
	Status execution.AttemptStatus
	Reason execution.Reason
	Hash   string
	Ver    domain.Version
}

func (s *stack) snapshot(a *att) snapshot {
	at := s.persisted(a)
	return snapshot{Status: at.Status, Reason: at.Reason, Hash: at.ResultHash, Ver: at.Version}
}

// noLeak fails when any of the values appear in the text.
func noLeak(t testing.TB, what, text string, values ...string) {
	t.Helper()
	for _, v := range values {
		if v != "" && strings.Contains(text, v) {
			t.Fatalf("%s leaks a delivered value (%d bytes of %q...)", what, len(v), v[:min(len(v), 6)])
		}
	}
}

// deliveredOf lists every credential and token the attempt held.
func (s *stack) deliveredOf(a *att) []string {
	out := append([]string(nil), s.srv.tokensOf(a.ID)...)
	if c := s.srv.credentialOf(a.ID); c != "" {
		out = append(out, c)
	}
	s.extraMu.Lock()
	defer s.extraMu.Unlock()
	for _, v := range s.extra[a.ID] {
		out = append(out, string(v))
	}
	return out
}

// removeWorkload deletes the attempt's fixture-owned Job (foreground, with its
// UID as precondition) and waits until the Job and its Pods are gone: the
// workload a node loss or an eviction leaves behind. Nodes are never touched.
func (s *stack) removeWorkload(a *att) {
	s.t.Helper()
	ctx := context.Background()
	ns := s.ns(a.Scope.TenantID)
	job, err := s.c.client.GetJob(ctx, ns, a.Job)
	must(s.t, err)
	must(s.t, s.c.client.DeleteJob(ctx, ns, a.Job, kubernetes.DeleteOptions{UID: job.Metadata.UID, Propagation: "Foreground"}))
	eventually(s.t, 3*time.Minute, "the workload to disappear", func() (bool, string) {
		_, err := s.c.client.GetJob(ctx, ns, a.Job)
		if !errors.Is(err, kubernetes.ErrNotFound) {
			return false, fmt.Sprintf("job: %v", err)
		}
		pods, err := s.c.client.ListPods(ctx, ns, kube.LabelJob+"="+a.Job)
		return err == nil && len(pods) == 0, fmt.Sprintf("%d pods", len(pods))
	})
}

func isErr(err, target error) bool { return errors.Is(err, target) }

// secretRefs is the pipeline's reference policy over the secrets this stack
// sealed (the admission-time pin of the real secret is execution's own).
type secretRefs struct{ s *stack }

func (r secretRefs) CheckSecret(_ context.Context, _ domain.Scope, _ pipeline.Actor, name string, _ pipeline.SecretUse) error {
	r.s.extraMu.Lock()
	defer r.s.extraMu.Unlock()
	if !r.s.sealed[name] {
		return pipeline.ErrReferenceNotFound
	}
	return nil
}

func (secretRefs) CheckEnvironment(context.Context, domain.Scope, pipeline.Actor, domain.EnvironmentID) error {
	return pipeline.ErrReferenceNotFound
}

// define validates and registers a definition as the engineer.
func (s *stack) define(scope domain.Scope, yaml string) pipeline.Definition {
	s.t.Helper()
	if s.validator == nil {
		v, err := pipeline.NewValidator(pipeline.Config{
			Capabilities: pipeline.EnabledKinds{pipeline.KindContainer, pipeline.KindAgent, pipeline.KindGate},
			Profiles:     pipelinetest.Profiles{"coding-v1": true},
			References:   secretRefs{s},
		})
		must(s.t, err)
		s.validator = v
	}
	actor := pipeline.Actor{ID: executiontest.Engineer, Type: pipeline.ActorUser}
	v, err := s.validator.ValidateYAML(context.Background(), scope, actor, []byte(yaml))
	if err != nil {
		s.t.Fatalf("validate definition: %v", err)
	}
	def, _, err := s.x.Pipeline.RegisterOrGet(context.Background(), pipeline.Candidate{
		Scope: scope, ID: domain.DefinitionID(s.x.ID("pd")), Content: v, CreatedBy: actor, CreatedAt: s.x.Clock.Now(),
	})
	must(s.t, err)
	return def
}

// notes returns the entrypoint's own diagnostic notes (entrypoint.log), when
// the attempt uploaded them: the explanation of a step that failed before or
// around its command.
func (s *stack) notes(a *att) string {
	art, err := s.kit.ArtRepo.FindByName(context.Background(), a.Scope, a.ID, "entrypoint.log")
	if err != nil {
		return "(no entrypoint.log: " + err.Error() + ")"
	}
	size, _ := art.Size()
	return s.kit.readArtifact(a, logs.ArtifactRef{ID: art.ID(), Digest: string(art.Digest()), Size: size})
}

// pinSecrets registers the run's secret pins through the secrets owner's own
// API. Work-run admission does this in its admission transaction; the internal
// cleanup run this fixture uses (no Git checkout) pins nothing, so the pin that
// a work run would hold is registered here, from the attempt's immutable pinned
// context, before the Pod can resolve anything.
func (s *stack) pinSecrets(a *att) {
	s.t.Helper()
	at := s.persisted(a)
	pinned, err := runtimev1.DecodeRequest[runtimev1.Context](at.Context)
	must(s.t, err)
	if len(pinned.Secrets) == 0 {
		s.t.Fatal("the attempt's pinned context declares no secrets")
	}
	must(s.t, storage.WithTenant(context.Background(), s.x.Runtime, a.Scope.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return s.x.Secrets.RegisterPins(ctx, tx, secretspg.PinRequest{
			Scope: a.Scope, Consumer: secrets.ConsumerRef{Kind: secrets.ConsumerKindRun, ID: string(a.Run)}, Bindings: pinned.Secrets,
		})
	}))
}
