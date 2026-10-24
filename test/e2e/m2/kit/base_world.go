package kit

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/domain"
)

// World is the seeded installation every acceptance test starts from: tenant A
// (an administrator, an engineer and a viewer of one project) and tenant B (its
// own administrator and project), all created through the public API after the
// operator bootstrap.
type World struct {
	S *Stack

	AdminID, EngineerID, ViewerID, OutsiderID, ForeignID string
	// ForeignEngineerID is tenant B's engineer (persona analyst): a caller of
	// another tenant who holds work and run permissions there.
	ForeignEngineerID string

	Project  string // tenant A's project
	ProjectB string // tenant B's project
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

// Seed bootstraps tenant A, provisions identities and a project each, tenant B
// and the memberships. With repo the project of tenant A names the Git
// repository, so workspaces and retained source references are real.
func (s *Stack) Seed(repo bool) *World {
	t := s.T
	t.Helper()
	ctx := context.Background()
	w := &World{S: s}
	s.Bootstrap(domain.TenantID(TenantA), "acme", "admin")

	me := func(persona string) string {
		r, err := s.API(persona).GetMeWithResponse(ctx)
		if err != nil {
			t.Fatal(err)
		}
		Expect(t, r, r.Body, http.StatusOK, "me "+persona)
		return r.JSON200.Id
	}
	w.AdminID, w.EngineerID, w.ViewerID, w.OutsiderID, w.ForeignID = me("admin"), me("engineer"), me("viewer"), me("outsider"), me("foreign")

	// The bootstrap is once per installation (its completion record is durable),
	// so the second tenant is provisioned the way an operator-controlled import
	// would: as the owner identity, which the serving role cannot be. Its
	// administrator then works only through the public API.
	for _, sql := range []string{
		`INSERT INTO tenants (id, key) VALUES ('` + TenantB + `', 'globex')`,
		`INSERT INTO memberships (tenant_id, id, user_id, project_id, role) VALUES ('` + TenantB + `', 'mb_` + Prefix + `_globex_admin', '` + w.ForeignID + `', NULL, 'admin')`,
		`INSERT INTO identity_authz_revisions (tenant_id, user_id, revision) VALUES ('` + TenantB + `', '` + w.ForeignID + `', 1)`,
	} {
		if _, err := s.Owner.Exec(ctx, sql); err != nil {
			t.Fatalf("seed tenant B: %v", err)
		}
	}

	settings := map[string]any{}
	if repo {
		settings["repository"] = map[string]any{
			"id": "repo_" + Prefix, "url": s.Repo.URL, "baseRef": s.Repo.BaseRef,
			"checkoutSecretRef": "secret://installation/checkout", "publisherSecretRef": "secret://installation/publisher",
		}
	}
	proj := func(persona, tenant, key string, settings map[string]any) string {
		r, err := s.API(persona).CreateProjectWithResponse(ctx, tenant, client.CreateProject{Key: key, Settings: &settings})
		if err != nil {
			t.Fatal(err)
		}
		Expect(t, r, r.Body, http.StatusCreated, "create project "+key)
		return r.JSON201.Id
	}
	w.Project = proj("admin", TenantA, "inventory", settings)
	w.ProjectB = proj("foreign", TenantB, "billing", map[string]any{})

	member := func(user, role string) {
		pid := w.Project
		r, err := s.API("admin").CreateMembershipWithResponse(ctx, TenantA, client.CreateMembership{UserId: user, Role: client.Role(role), ProjectId: &pid})
		if err != nil {
			t.Fatal(err)
		}
		Expect(t, r, r.Body, http.StatusCreated, "membership "+role)
	}
	member(w.EngineerID, "engineer")
	member(w.ViewerID, "viewer")
	w.ForeignEngineerID = me("analyst")
	pb := w.ProjectB
	r, err := s.API("foreign").CreateMembershipWithResponse(ctx, TenantB, client.CreateMembership{UserId: w.ForeignEngineerID, Role: "engineer", ProjectId: &pb})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusCreated, "tenant B engineer")
	return w
}

// WorkItem creates a story as the engineer.
func (w *World) WorkItem(title, spec string) client.WorkItem {
	t := w.S.T
	t.Helper()
	r, err := w.S.API("engineer").CreateWorkItemWithResponse(context.Background(), TenantA, w.Project, client.CreateWorkItem{Kind: "story", Title: title, Spec: &spec})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusCreated, "create work item")
	return *r.JSON201
}

// Step is the one container step of a test pipeline.
type Step struct {
	// Script runs under `/bin/sh -ec` in the pinned image.
	Script string
	// Outputs declares string outputs by name.
	Outputs []string
	// Artifacts declares file outputs by name and path under /kodama/outputs.
	Artifacts map[string]string
	// Timeout is the step timeout in seconds (default 120).
	Timeout int
	// Access is the workspace access ("none" unless a test needs another).
	Access string
}

// PodmanDefinition registers a one-step container pipeline as the engineer. The
// step runs in the pinned image on the Podman runtime; it reads no source (a
// container workload cannot check out source outside Kubernetes, ErrNoCheckout).
func (w *World) PodmanDefinition(name string, st Step) client.PipelineDefinition {
	t := w.S.T
	t.Helper()
	if st.Timeout == 0 {
		st.Timeout = 120
	}
	if st.Access == "" {
		st.Access = "none"
	}
	step := map[string]any{
		"id": "work", "kind": "container", "timeoutSeconds": st.Timeout,
		"image": PodmanImage, "command": []string{"/bin/sh", "-ec"}, "args": []string{st.Script},
		"workspace": map[string]any{"access": st.Access},
	}
	if st.Access != "none" {
		step["workspace"] = map[string]any{"from": "run", "access": st.Access}
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
	return w.RegisterDefinition("engineer", name, []any{step})
}

// RegisterDefinition registers a pipeline of the given steps as persona.
func (w *World) RegisterDefinition(persona, name string, steps []any) client.PipelineDefinition {
	t := w.S.T
	t.Helper()
	doc := map[string]any{
		"apiVersion": "kodama/v1alpha1", "kind": "Pipeline", "metadata": map[string]any{"name": name},
		"spec": map[string]any{"inputs": map[string]any{}, "steps": steps},
	}
	r, err := w.S.API(persona).RegisterPipelineDefinitionWithBodyWithResponse(context.Background(), TenantA, w.Project, "application/json", JSONBody(t, doc))
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
	t := w.S.T
	t.Helper()
	r, err := w.S.API("engineer").ProvisionWorkspaceWithResponse(context.Background(), TenantA, w.Project, item, client.EmptyRequest{})
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

// CreateRun admits a run at the work item's pinned content revision and the
// repository's initial commit.
func (w *World) CreateRun(persona, key string, item client.WorkItem, def string) *client.CreateRunResponse {
	t := w.S.T
	t.Helper()
	r, err := w.S.API(persona).CreateRunWithResponse(context.Background(), TenantA, w.Project, &client.CreateRunParams{IdempotencyKey: key},
		client.CreateRunRequest{WorkItemId: item.Id, DefinitionId: def, ContentRevision: int64(item.ContentRevision), SourceCommit: w.S.Repo.Initial})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Run creates a run and requires admission.
func (w *World) Run(key string, item client.WorkItem, def string) client.Run {
	t := w.S.T
	t.Helper()
	r := w.CreateRun("engineer", key, item, def)
	if r.StatusCode() != http.StatusCreated {
		t.Fatalf("admit run: HTTP %s: %s", r.Status(), r.Body)
	}
	return *r.JSON201
}

// GetRun reads the run as the engineer.
func (w *World) GetRun(id string) client.Run {
	t := w.S.T
	t.Helper()
	r, err := w.S.API("engineer").GetRunWithResponse(context.Background(), TenantA, w.Project, id)
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusOK, "get run")
	return *r.JSON200
}

// RunStatus is the run's status and whether a cancellation was requested.
func (w *World) RunStatus(id string) (string, bool) {
	r, err := w.S.API("engineer").GetRunWithResponse(context.Background(), TenantA, w.Project, id)
	if err != nil || r.StatusCode() != 200 {
		return "", false
	}
	return string(r.JSON200.Status), r.JSON200.CancelRequestedAt != nil
}

// WaitRun waits until the run is terminal and returns it.
func (w *World) WaitRun(id string, within time.Duration) client.Run {
	t := w.S.T
	t.Helper()
	var last client.Run
	WaitFor(t, "run "+id+" to be terminal", within, func() bool {
		r, err := w.S.API("engineer").GetRunWithResponse(context.Background(), TenantA, w.Project, id)
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
	w.S.T.Helper()
	WaitFor(w.S.T, "the run's source retention", 30*time.Second, func() bool {
		return w.GetRun(id).SourceRetention == "confirmed"
	})
}

// Steps lists the run's steps.
func (w *World) Steps(id string) []client.Step {
	t := w.S.T
	t.Helper()
	r, err := w.S.API("engineer").ListRunStepsWithResponse(context.Background(), TenantA, w.Project, id, &client.ListRunStepsParams{})
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

// Logs returns the retained or live log lines of the run's only attempt.
func (w *World) Logs(persona, id string) (int, []string) {
	t := w.S.T
	t.Helper()
	resp, body := w.S.Raw("GET", "/api/v1/tenants/"+TenantA+"/projects/"+w.Project+"/runs/"+id+"/logs?tail=100", w.S.Token(persona), nil, nil)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, nil
	}
	var lines []string
	sc := bufio.NewScanner(strings.NewReader(string(body)))
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

// CancelRun requests cancellation as the engineer.
func (w *World) CancelRun(id string) *client.CancelRunResponse {
	t := w.S.T
	t.Helper()
	r, err := w.S.API("engineer").CancelRunWithResponse(context.Background(), TenantA, w.Project, id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// CancelItem cancels the work item as the engineer.
func (w *World) CancelItem(item client.WorkItem) {
	t := w.S.T
	t.Helper()
	reason := Prefix + " cancellation"
	r, err := w.S.API("engineer").TransitionWorkItemWithResponse(context.Background(), TenantA, w.Project, item.Id,
		&client.TransitionWorkItemParams{IfMatch: fmt.Sprintf(`"%d"`, item.Version)}, client.TransitionRequest{To: "cancelled", Reason: &reason})
	if err != nil {
		t.Fatal(err)
	}
	Expect(t, r, r.Body, http.StatusOK, "cancel work item")
}

// ---- observation -----------------------------------------------------------

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

// OpenEvents opens the project (or, with a path, a narrower) event stream.
func (s *Stack) OpenEvents(persona, path, lastEventID string) *Stream {
	s.T.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", s.Base()+path, nil)
	if err != nil {
		cancel()
		s.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token(persona))
	req.Header.Set("Accept", "text/event-stream")
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		cancel()
		s.T.Fatal(err)
	}
	st := &Stream{Frames: make(chan Frame, 256), Cancel: cancel, Status: resp.StatusCode}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return st
	}
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

// ProjectEvents is the project's event stream path.
func (w *World) ProjectEvents() string {
	return "/api/v1/tenants/" + TenantA + "/projects/" + w.Project + "/events"
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

// OfType matches an event frame of the type.
func OfType(typ string) func(Frame) bool {
	return func(f Frame) bool { return f.Event == typ && f.Type() == typ }
}

// WaitLogs waits for the retained log of the run's attempt and returns its
// lines. The wrapper's upload is verified and retained before the workload is
// removed, so the log outlives the container.
func (w *World) WaitLogs(persona, run string) []string {
	t := w.S.T
	t.Helper()
	var lines []string
	WaitFor(t, "the retained log of "+run, 60*time.Second, func() bool {
		code, l := w.Logs(persona, run)
		lines = l
		return code == http.StatusOK
	})
	return lines
}

// WaitRemoved waits until no container of the attempt exists and the removal is
// durably recorded.
func (w *World) WaitRemoved(attempt string) {
	t := w.S.T
	t.Helper()
	WaitFor(t, "the workload of "+attempt+" to be removed", 60*time.Second, func() bool {
		return len(w.S.Containers(TenantA, attempt)) == 0 &&
			w.S.OwnerCount(`SELECT count(*) FROM attempt_controls WHERE attempt_id = $1 AND workload_removed_at IS NOT NULL`, attempt) == 1
	})
}

// Scripted creates a provisioned work item, registers a one-step Podman
// pipeline running the script and admits a run of it.
func (w *World) Scripted(name string, st Step) (client.WorkItem, client.Run) {
	t := w.S.T
	t.Helper()
	item := w.WorkItem(name, "# "+name)
	w.Provision(item.Id)
	def := w.PodmanDefinition(name, st)
	return item, w.Run(name+"-key", item, def.Id)
}

var requestIDPattern = regexp.MustCompile(`"requestId":"[^"]*"`)

// Anonymize removes the per-request identifier from an error body, so two
// refusals can be compared byte for byte.
func Anonymize(b []byte) string {
	return requestIDPattern.ReplaceAllString(string(b), `"requestId":"-"`)
}
