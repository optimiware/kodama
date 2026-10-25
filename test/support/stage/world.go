package stage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
)

// ---- API access ----------------------------------------------------------------------------------------------

// Token returns the persona's access token.
func (i *Installation) Token(persona string) string {
	i.T.Helper()
	tok, err := i.Auth.Token(i.Context(2*time.Minute), persona)
	if err != nil {
		i.T.Fatalf("sign in %s: %v", persona, err)
	}
	return tok
}

// API returns a generated client authenticated as the persona; tokens are renewed as they expire.
func (i *Installation) API(persona string) *client.ClientWithResponses {
	i.T.Helper()
	editor := func(ctx context.Context, req *http.Request) error {
		tok, err := i.Auth.Token(ctx, persona)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return nil
	}
	c, err := client.NewClientWithResponses(i.S.URL+"/api/v1", client.WithRequestEditorFn(editor), client.WithHTTPClient(&http.Client{Timeout: 30 * time.Second}))
	if err != nil {
		i.T.Fatal(err)
	}
	return c
}

// Raw issues one HTTP request against the public origin.
func (i *Installation) Raw(method, path, token string, body io.Reader, headers map[string]string) (*http.Response, []byte) {
	i.T.Helper()
	req, err := http.NewRequest(method, i.S.URL+path, body)
	if err != nil {
		i.T.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		i.T.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// TryRaw is Raw for polling during an outage: a transport failure is returned.
func (i *Installation) TryRaw(method, path, token string, body io.Reader, headers map[string]string) (*http.Response, []byte, error) {
	req, err := http.NewRequest(method, i.S.URL+path, body)
	if err != nil {
		return nil, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b, nil
}

// JSONBody marshals v as a request body.
func JSONBody(t testing.TB, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(b)
}

// ErrorCode is the stable error code of an error envelope.
func ErrorCode(body []byte) string {
	var env struct {
		Error struct{ Code string } `json:"error"`
	}
	_ = json.Unmarshal(body, &env)
	return env.Error.Code
}

// WaitFor polls cond until it holds or the deadline passes.
func WaitFor(t testing.TB, what string, within time.Duration, cond func() bool) {
	t.Helper()
	WaitForEvery(t, what, within, 250*time.Millisecond, cond)
}

// WaitForEvery is WaitFor with an explicit polling interval.
func WaitForEvery(t testing.TB, what string, within, every time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", within, what)
		}
		time.Sleep(every)
	}
}

// StatusResponse is what every generated response answers.
type StatusResponse interface {
	StatusCode() int
	Status() string
}

// Expect fails unless the response has the wanted status.
func Expect(t testing.TB, r StatusResponse, body []byte, want int, what string) {
	t.Helper()
	if r.StatusCode() != want {
		t.Fatalf("%s: HTTP %s, want %d: %s", what, r.Status(), want, body)
	}
}

// ---- the seeded world -------------------------------------------------------------------------------------------

// World is a seeded start of one test: tenant A (an administrator, an engineer and a viewer of one fresh project
// whose source is a fresh private repository) and tenant B (its own administrator and project).
type World struct {
	I *Installation

	TenantA, TenantB string

	AdminID, EngineerID, ViewerID, OutsiderID, ForeignID string
	ForeignEngineerID                                    string

	Project  string
	ProjectB string
	Repo     *Repo
}

// Seed makes the installation ready, bootstraps tenant A once per installation and provisions a fresh project,
// repository and memberships through the public API. Tenant B is provisioned the way an operator-controlled
// import would be: as the owner identity, because the bootstrap is once per installation.
func (i *Installation) Seed() *World {
	t := i.T
	t.Helper()
	ctx := i.Context(10 * time.Minute)
	i.EnsureReady(ctx)
	w := &World{I: i, TenantA: i.S.TenantA().TenantID, TenantB: i.S.TenantB().TenantID}

	if i.OwnerCount(`SELECT count(*) FROM tenants WHERE id = $1`, w.TenantA) == 0 {
		if out, err := i.Bootstrap(ctx, i.S.TenantA(), "acme", "admin"); err != nil {
			t.Fatalf("bootstrap %s: %v", w.TenantA, err)
		} else {
			t.Logf("bootstrap: %s", strings.TrimSpace(out))
		}
	}

	me := func(persona string) string {
		r, err := i.API(persona).GetMeWithResponse(ctx)
		if err != nil {
			t.Fatal(err)
		}
		Expect(t, r, r.Body, http.StatusOK, "me "+persona)
		return r.JSON200.Id
	}
	w.AdminID, w.EngineerID, w.ViewerID, w.OutsiderID, w.ForeignID = me("admin"), me("engineer"), me("viewer"), me("outsider"), me("foreign")
	w.ForeignEngineerID = me("analyst")

	if i.OwnerCount(`SELECT count(*) FROM tenants WHERE id = $1`, w.TenantB) == 0 {
		for _, sql := range []string{
			`INSERT INTO tenants (id, key) VALUES ('` + w.TenantB + `', 'globex')`,
			`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('` + w.TenantB + `', 'mb_` + i.S.Name + `_globex_admin', '` + w.ForeignID + `', NULL, 'admin')`,
			`INSERT INTO identity_authz_revisions (tenant_id, user_id, revision) VALUES ('` + w.TenantB + `', '` + w.ForeignID + `', 1)`,
		} {
			i.OwnerExec(sql)
		}
	}

	w.Repo = i.NewRepo(ctx, "q-"+i.S.Name)
	settings := map[string]any{"repository": map[string]any{
		"id": "repo_" + w.Repo.Name, "url": w.Repo.URL, "baseRef": w.Repo.BaseRef,
		"checkoutSecretRef": "secret://installation/checkout", "publisherSecretRef": "secret://installation/publisher",
	}}
	proj := func(persona, tenant, key string, settings map[string]any) string {
		r, err := i.API(persona).CreateProjectWithResponse(ctx, tenant, client.CreateProject{Key: key, Settings: &settings})
		if err != nil {
			t.Fatal(err)
		}
		Expect(t, r, r.Body, http.StatusCreated, "create project "+key)
		return r.JSON201.Id
	}
	suffix := random(3)
	w.Project = proj("admin", w.TenantA, "inventory-"+suffix, settings)
	w.ProjectB = proj("foreign", w.TenantB, "billing-"+suffix, map[string]any{})

	member := func(user, role string) {
		pid := w.Project
		r, err := i.API("admin").CreateMembershipWithResponse(ctx, w.TenantA, client.CreateMembership{UserId: user, Role: client.Role(role), ProjectId: &pid})
		if err != nil {
			t.Fatal(err)
		}
		Expect(t, r, r.Body, http.StatusCreated, "membership "+role)
	}
	member(w.EngineerID, "engineer")
	member(w.ViewerID, "viewer")
	pb := w.ProjectB
	r, err := i.API("foreign").CreateMembershipWithResponse(ctx, w.TenantB, client.CreateMembership{UserId: w.ForeignEngineerID, Role: "engineer", ProjectId: &pb})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusCreated, "tenant B engineer")
	return w
}

// WorkItem creates a story as the engineer.
func (w *World) WorkItem(title, spec string) client.WorkItem {
	t := w.I.T
	t.Helper()
	r, err := w.I.API("engineer").CreateWorkItemWithResponse(context.Background(), w.TenantA, w.Project, client.CreateWorkItem{Kind: "story", Title: title, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusCreated, "create work item")
	return *r.JSON201
}

// Step is the one container step of a test pipeline.
type Step struct {
	// Script runs under `/bin/sh -ec` in the pinned workload image.
	Script string
	// Outputs declares string outputs by name; Artifacts file outputs by name and path under /kodama/outputs.
	Outputs   []string
	Artifacts map[string]string
	// Timeout is the step timeout in seconds (default 120).
	Timeout int
	// Access is the workspace access ("none" unless a test needs another).
	Access string
	// ID names the step (default "work"); Needs lists the steps it depends on.
	ID    string
	Needs []string
	// From selects the workspace source ("run", or a writer's step ID); default "run".
	From string
	// Env binds environment variables to secret references.
	Env map[string]string
}

// StepDocument renders the step of a pipeline definition.
func (w *World) StepDocument(st Step) map[string]any {
	if st.Timeout == 0 {
		st.Timeout = 120
	}
	if st.Access == "" {
		st.Access = "none"
	}
	if st.ID == "" {
		st.ID = "work"
	}
	step := map[string]any{
		"id": st.ID, "kind": "container", "timeoutSeconds": st.Timeout,
		"image": w.I.WorkloadImage(), "command": []string{"/bin/sh", "-ec"}, "args": []string{st.Script},
		"workspace": map[string]any{"access": st.Access},
	}
	if st.Access != "none" {
		from := st.From
		if from == "" {
			from = "run"
		}
		step["workspace"] = map[string]any{"from": from, "access": st.Access}
	}
	if len(st.Needs) > 0 {
		step["needs"] = st.Needs
	}
	if len(st.Env) > 0 {
		env := map[string]any{}
		for k, ref := range st.Env {
			env[k] = map[string]any{"secretRef": ref}
		}
		step["env"] = env
	}
	if len(st.Outputs) > 0 || len(st.Artifacts) > 0 {
		outs := map[string]any{}
		for _, o := range st.Outputs {
			outs[o] = map[string]any{"type": "string"}
		}
		for name, path := range st.Artifacts {
			outs[name] = map[string]any{"type": "artifact", "path": path}
		}
		step["outputs"] = outs
	}
	return step
}

// Definition registers a one-step container pipeline as the engineer.
func (w *World) Definition(name string, st Step) client.PipelineDefinition {
	return w.RegisterDefinition("engineer", name, []any{w.StepDocument(st)})
}

// RegisterDefinition registers a pipeline of the given steps as persona.
func (w *World) RegisterDefinition(persona, name string, steps []any) client.PipelineDefinition {
	t := w.I.T
	t.Helper()
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": steps},
	}
	r, err := w.I.API(persona).RegisterPipelineDefinitionWithBodyWithResponse(context.Background(), w.TenantA, w.Project, "application/json", JSONBody(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
		t.Fatalf("register definition: HTTP %s: %s", r.Status(), r.Body)
	}
	if r.JSON201 != nil {
		return *r.JSON201
	}
	return *r.JSON200
}

// Provision provisions the work item's workspace through the real Git server.
func (w *World) Provision(item string) client.Workspace {
	t := w.I.T
	t.Helper()
	r, err := w.I.API("engineer").ProvisionWorkspaceWithResponse(context.Background(), w.TenantA, w.Project, item, client.EmptyRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode() != http.StatusCreated && r.StatusCode() != http.StatusOK {
		t.Fatalf("provision workspace: HTTP %s: %s", r.Status(), r.Body)
	}
	ws := r.JSON201
	if ws == nil {
		ws = r.JSON200
	}
	if ws.State != "available" {
		t.Fatalf("workspace is %s (%v)", ws.State, ws.LastError)
	}
	return *ws
}

// CreateRun admits a run at the work item's pinned content revision and the repository's initial commit.
func (w *World) CreateRun(persona, key string, item client.WorkItem, def string) *client.CreateRunResponse {
	t := w.I.T
	t.Helper()
	r, err := w.I.API(persona).CreateRunWithResponse(context.Background(), w.TenantA, w.Project, &client.CreateRunParams{IdempotencyKey: key},
		client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: def, ContentRevision: int64(item.ContentRevision), SourceCommit: w.Repo.Initial})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Run creates a run and requires admission.
func (w *World) Run(key string, item client.WorkItem, def string) client.Run {
	t := w.I.T
	t.Helper()
	r := w.CreateRun("engineer", key, item, def)
	if r.StatusCode() != http.StatusCreated {
		t.Fatalf("admit run: HTTP %s: %s", r.Status(), r.Body)
	}
	return *r.JSON201
}

// GetRun reads the run as the engineer.
func (w *World) GetRun(id string) client.Run {
	t := w.I.T
	t.Helper()
	r, err := w.I.API("engineer").GetRunWithResponse(context.Background(), w.TenantA, w.Project, id)
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusOK, "get run")
	return *r.JSON200
}

// RunStatus is the run's status and whether a cancellation was requested ("" when it could not be read).
func (w *World) RunStatus(id string) (string, bool) {
	r, err := w.I.API("engineer").GetRunWithResponse(context.Background(), w.TenantA, w.Project, id)
	if err != nil || r.StatusCode() != 200 {
		return "", false
	}
	return string(r.JSON200.Status), r.JSON200.CancelRequestedAt != nil
}

// WaitRun waits until the run is terminal and returns it.
func (w *World) WaitRun(id string, within time.Duration) client.Run {
	t := w.I.T
	t.Helper()
	var last client.Run
	WaitFor(t, "run "+id+" to be terminal", within, func() bool {
		r, err := w.I.API("engineer").GetRunWithResponse(context.Background(), w.TenantA, w.Project, id)
		if err != nil || r.StatusCode() != 200 {
			return false
		}
		last = *r.JSON200
		switch last.Status {
		case client.RunStatusSucceeded, client.RunStatusFailed, client.RunStatusCancelled:
			return true
		}
		return false
	})
	return last
}

// WaitSourceRetained waits for the run's retained source reference.
func (w *World) WaitSourceRetained(id string) {
	w.I.T.Helper()
	WaitFor(w.I.T, "the run's source retention", time.Minute, func() bool { return w.GetRun(id).SourceRetention == "confirmed" })
}

// Steps lists the run's steps.
func (w *World) Steps(id string) []client.Step {
	t := w.I.T
	t.Helper()
	r, err := w.I.API("engineer").ListRunStepsWithResponse(context.Background(), w.TenantA, w.Project, id, &client.ListRunStepsParams{})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusOK, "list steps")
	return r.JSON200.Items
}

// Attempts lists the attempts of the run's first step.
func (w *World) Attempts(id string) []client.Attempt {
	steps := w.Steps(id)
	if len(steps) == 0 {
		return nil
	}
	return steps[0].Attempts
}

// WaitAttempt waits until the run's first step has an attempt and returns its ID.
func (w *World) WaitAttempt(run string, within time.Duration) string {
	w.I.T.Helper()
	var id string
	WaitFor(w.I.T, "an attempt of "+run, within, func() bool {
		if a := w.Attempts(run); len(a) > 0 {
			id = a[0].Id
			return true
		}
		return false
	})
	return id
}

// Logs returns the retained or live log lines of the run's only attempt.
func (w *World) Logs(persona, id string) (int, []string) {
	t := w.I.T
	t.Helper()
	resp, body := w.I.Raw("GET", "/api/v1/tenants/"+w.TenantA+"/projects/"+w.Project+"/runs/"+id+"/logs?tail=200", w.I.Token(persona), nil, nil)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(body)))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var rec struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			t.Fatalf("log line %q: %v", sc.Text(), err)
		}
		if rec.Type == "chunk" {
			lines = append(lines, rec.Text)
		}
	}
	return resp.StatusCode, lines
}

// WaitLogs waits for the retained log of the run's attempt and returns its lines.
func (w *World) WaitLogs(persona, run string) []string {
	t := w.I.T
	t.Helper()
	var lines []string
	WaitFor(t, "the retained log of "+run, 2*time.Minute, func() bool {
		code, l := w.Logs(persona, run)
		lines = l
		return code == http.StatusOK
	})
	return lines
}

// CancelRun requests cancellation as the engineer.
func (w *World) CancelRun(id string) *client.CancelRunResponse {
	t := w.I.T
	t.Helper()
	r, err := w.I.API("engineer").CancelRunWithResponse(context.Background(), w.TenantA, w.Project, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Scripted creates a provisioned work item, registers a one-step pipeline running the script and admits a run.
func (w *World) Scripted(name string, st Step) (client.WorkItem, client.Run) {
	w.I.T.Helper()
	item := w.WorkItem(name, "# "+name)
	w.Provision(item.Id)
	def := w.Definition(name, st)
	return item, w.Run(name+"-key", item, def.Id)
}

// ---- event streams -----------------------------------------------------------------------------------------------

// Frame is one server-sent event.
type Frame struct {
	Event string
	ID    string
	Data  string
}

// Type is the event type of an event frame (a control frame such as snapshot has none).
func (f Frame) Type() string {
	var ev struct{ Type string }
	_ = json.Unmarshal([]byte(f.Data), &ev)
	return ev.Type
}

// Stream is an open event stream read in the background.
type Stream struct {
	Frames chan Frame
	Cancel context.CancelFunc
	Status int
}

// ProjectEvents is the project's event stream path.
func (w *World) ProjectEvents() string {
	return "/api/v1/tenants/" + w.TenantA + "/projects/" + w.Project + "/events"
}

// OpenEvents opens an event stream as the persona.
func (i *Installation) OpenEvents(persona, path, lastEventID string) *Stream {
	i.T.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", i.S.URL+path, nil)
	if err != nil {
		cancel()
		i.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+i.Token(persona))
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		i.T.Fatal(err)
	}
	st := &Stream{Frames: make(chan Frame, 256), Cancel: cancel, Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return st
	}
	i.T.Cleanup(cancel)
	go func() {
		defer resp.Body.Close()
		defer close(st.Frames)
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var cur Frame
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if cur.Event != "" || cur.Data != "" {
					select {
					case st.Frames <- cur:
					case <-ctx.Done():
						return
					}
				}
				cur = Frame{}
			case strings.HasPrefix(line, "event:"):
				cur.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "id:"):
				cur.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
			case strings.HasPrefix(line, "data:"):
				cur.Data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
	}()
	return st
}

// Until reads frames until match accepts one.
func (st *Stream) Until(t testing.TB, what string, within time.Duration, match func(Frame) bool) Frame {
	t.Helper()
	timer := time.NewTimer(within)
	defer timer.Stop()
	for {
		select {
		case f, ok := <-st.Frames:
			if !ok {
				t.Fatalf("the stream ended while waiting for %s", what)
			}
			if match(f) {
				return f
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

var requestIDPattern = regexp.MustCompile(`"requestId":"[^"]*"`)

// Anonymize removes the per-request identifier from an error body, so two refusals can be compared.
func Anonymize(b []byte) string {
	return requestIDPattern.ReplaceAllString(string(b), `"requestId":"-"`)
}

var _ = fmt.Sprintf
