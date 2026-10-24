package assemblyagentdelivery_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/internal/domain"
	gw "github.com/optimiware/kodama/internal/integrations/gitwebhook"
	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
	gitfx "github.com/optimiware/kodama/test/support/git"
)

// The native Git trigger is mounted by the assembled core (T-100's PostgreSQL
// trigger store, delivery claims and admission ledger, the secrets-backed hook
// secret, T-101's mount behind AssembleTriggers); the test installs a trigger
// through the core's own management seam and talks to the core's own endpoint.
// Real: the Git host (git-http-backend over TLS, a bare repository whose
// post-receive hook is the shipped example script), the Git reader proving the
// exact commit with the read-only credential, the identity authority, the hook
// secret as a project secret, the durable ledger, run admission and Podman
// execution. Test-owned: only a recording proxy between the hook and the core,
// so a delivery can be replayed byte for byte.

// recorder keeps every exchange the endpoint answered.
type recorder struct {
	mu    sync.Mutex
	items []recorded
}

type recorded struct {
	header http.Header
	body   []byte
	status int
	reply  []byte
}

func (r *recorder) all() []recorded {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]recorded(nil), r.items...)
}

// hookWorld is one trigger on the real remote.
type hookWorld struct {
	t      *testing.T
	e      *kit.Env
	item   client.WorkItem
	branch string
	def    client.PipelineDefinition

	secret    []byte
	secretRef string
	repoID    string
	scope     domain.Scope
	trigger   gw.Trigger

	proxy *httptest.Server
	rec   *recorder
}

// newHookWorld provisions the item, the registered definition, the hook
// secret, the installer's two grants, the trigger and the real hook.
func newHookWorld(t *testing.T, e *kit.Env) *hookWorld {
	t.Helper()
	h := &hookWorld{t: t, e: e, secretRef: "secret://project/git-hook", secret: []byte(strings.Repeat("native-hook-secret-", 3)), repoID: "repo_" + kit.Prefix, rec: &recorder{}}
	h.scope = domain.Scope{TenantID: domain.TenantID(kit.TenantA), ProjectID: domain.ProjectID(e.World.Project)}
	// The installer needs project:settings (administrator) and run:manage
	// (engineer): the role matrix keeps them apart, so the administrator is also
	// bound as an engineer of the project.
	e.AddMember("admin", "engineer")
	e.MustDo("admin", "POST", e.ProjectPath()+"/secrets", map[string]any{"name": "git-hook", "kind": "generic", "value": base64.StdEncoding.EncodeToString(h.secret)},
		map[string]string{"Idempotency-Key": "t038-git-hook-secret"}, http.StatusCreated)
	h.item = e.Item("Native trigger", "# native trigger")
	h.branch = "kodama/work/" + h.item.Id
	h.def = e.Register("hook-run", map[string]any{}, kit.ContainerStep("work", `printf '{"outcome":"completed","outputs":{"ran":"yes"}}' > "$KODAMA_RESULTS_FILE"`,
		map[string]any{"ran": map[string]any{"type": "string"}}))

	h.install()
	h.startProxy()
	h.installHook()
	return h
}

func (h *hookWorld) install() {
	h.t.Helper()
	svc := h.e.Server().Core().Triggers
	if svc == nil {
		h.t.Fatalf("the core does not mount the Git trigger (disabled: %v)", h.e.Server().Core().Routes.Disabled)
	}
	trig, err := svc.Service.Install(context.Background(), gw.InstallCommand{
		Scope: h.scope, Principal: h.e.UserID("admin"), DefinitionID: domain.DefinitionID(h.def.Id), WorkItemID: domain.WorkItemID(h.item.Id),
		Branches: []string{"kodama/work/*"}, SecretRef: h.secretRef,
	})
	if err != nil {
		h.t.Fatalf("install the trigger: %v", err)
	}
	h.trigger = trig
}

// startProxy records every exchange between the hook and the core's endpoint.
func (h *hookWorld) startProxy() {
	h.t.Helper()
	h.proxy = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		req, _ := http.NewRequest(r.Method, h.e.Base()+r.URL.RequestURI(), bytes.NewReader(body))
		req.Header = r.Header.Clone()
		resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		h.rec.mu.Lock()
		h.rec.items = append(h.rec.items, recorded{header: r.Header.Clone(), body: body, status: resp.StatusCode, reply: out})
		h.rec.mu.Unlock()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
	}))
	h.t.Cleanup(h.proxy.Close)
}

func (h *hookWorld) url() string { return h.proxy.URL + "/api/v1/git-hooks/" + string(h.trigger.ID) }

// coreURL is the core's own endpoint for the trigger.
func (h *hookWorld) coreURL() string { return h.e.Base() + "/api/v1/git-hooks/" + string(h.trigger.ID) }

// installHook installs the shipped example as the bare repository's
// post-receive hook, with the environment its header documents.
func (h *hookWorld) installHook() {
	h.t.Helper()
	example, err := filepath.Abs("../../../internal/integrations/gitwebhook/example/post-receive")
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := os.Stat(example); err != nil {
		h.t.Fatalf("the shipped hook script: %v", err)
	}
	hook := fmt.Sprintf("#!/bin/sh\nexport KODAMA_HOOK_URL='%s'\nexport KODAMA_HOOK_SECRET='%s'\nexport KODAMA_REPOSITORY='%s'\nexport KODAMA_HOOK_ATTEMPTS=1\nexec /bin/sh '%s'\n",
		h.url(), h.secret, h.repoID, example)
	dir := filepath.Join(h.e.Repo.Path, "hooks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "post-receive"), []byte(hook), 0o755); err != nil {
		h.t.Fatal(err)
	}
}

// push commits a file on top of the branch tip and pushes it to the real
// remote: the hook runs as part of the receive and notifies the endpoint
// before the push returns.
func (h *hookWorld) push(branch, msg string) string {
	h.t.Helper()
	return h.e.Repo.HumanPush(h.t, "refs/heads/"+branch, msg, gitfx.File{Path: "change-" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".txt", Content: msg + "\n"})
}

// pushNew pushes a commit on top of base to a branch that does not exist yet.
func (h *hookWorld) pushNew(branch, base, msg string) string {
	h.t.Helper()
	commit, work := h.e.Repo.Commit(h.t, base, msg, gitfx.File{Path: "other.txt", Content: msg + "\n"})
	cmd := exec.Command("git", "push", "-q", h.e.Repo.Path, commit+":refs/heads/"+branch)
	cmd.Dir = work
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		h.t.Fatalf("push: %v\n%s", err, out)
	}
	return commit
}

// signed builds a correctly signed delivery for payload with the trigger's secret.
func (h *hookWorld) signed(deliveryID string, p gw.Payload) (http.Header, []byte) {
	body, _ := json.Marshal(p)
	ts := time.Now().Unix()
	hd := http.Header{}
	hd.Set("Content-Type", "application/json")
	hd.Set(gw.HeaderDeliveryID, deliveryID)
	hd.Set(gw.HeaderTimestamp, strconv.FormatInt(ts, 10))
	hd.Set(gw.HeaderSignature, gw.Sign(h.secret, h.trigger.ID, deliveryID, ts, body))
	return hd, body
}

func (h *hookWorld) post(url string, header http.Header, body []byte) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
	req.Header = header.Clone()
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(b, &out)
	return resp.StatusCode, out
}

// runsOf lists the work item's runs through the public API.
func (h *hookWorld) runsOf() []client.Run {
	h.t.Helper()
	code, b := h.e.Do("engineer", "GET", h.e.ProjectPath()+"/runs?workItemId="+h.item.Id, nil, nil)
	if code != http.StatusOK {
		h.t.Fatalf("list runs: %d %s", code, b)
	}
	var page client.RunPage
	if err := json.Unmarshal(b, &page); err != nil {
		h.t.Fatal(err)
	}
	var out []client.Run
	for _, r := range page.Items {
		if r.WorkItemId != nil && *r.WorkItemId == h.item.Id {
			out = append(out, r)
		}
	}
	return out
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// recordOf is the exchange that announced the commit. The platform's own pushes
// (a run's retained workspace refs) reach the same hook, so "the last exchange"
// is not necessarily the test's push.
func (h *hookWorld) recordOf(commit string) recorded {
	h.t.Helper()
	var found recorded
	kit.WaitFor(h.t, "the hook exchange of "+commit, 30*time.Second, func() bool {
		for _, r := range h.rec.all() {
			if strings.Contains(string(r.body), commit) {
				found = r
				return true
			}
		}
		return false
	})
	return found
}
