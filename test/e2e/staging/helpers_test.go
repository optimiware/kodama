package staging_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/optimiware/kodama/api/client"
	"github.com/optimiware/kodama/test/support/stage"
)

// The staging suite runs one session through the lifecycle in source order: install from a clean namespace
// set, use it, restart it, search its logs, check what it refuses, then uninstall it. Go keeps running after a
// failure, but each test re-establishes what it needs (EnsureReady) and the gate requires every one to pass,
// so a failed step never lets a later one claim a pass on its own.

func suffix(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal("entropy failed")
	}
	return hex.EncodeToString(b)
}

// stageContract is the deployed contract the gate bound this session to (KODAMA_STAGE_CONTRACT).
type stageContract struct {
	KMS struct {
		Provider  string            `json:"provider"`
		Resources map[string]string `json:"resources"`
	} `json:"kms"`
	ObjectStorage struct {
		Endpoint        string `json:"endpoint"`
		Region          string `json:"region"`
		ArtifactsBucket string `json:"artifacts_bucket"`
		ArtifactsPrefix string `json:"artifacts_prefix"`
	} `json:"object_storage"`
}

func loadContract(t *testing.T) stageContract {
	t.Helper()
	path := os.Getenv("KODAMA_STAGE_CONTRACT")
	if path == "" {
		t.Fatal("KODAMA_STAGE_CONTRACT must name the session contract")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c stageContract
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal("the stage contract is not valid JSON")
	}
	return c
}

// held is a one-step pipeline that waits at a post-checkout barrier, so a test decides when it ends.
func held(w *stage.World, name, tail string) client.PipelineDefinition {
	return w.Definition(name, stage.Step{Script: stage.CommandBarrier + tail, Timeout: 600})
}

func runHeld(t *testing.T, i *stage.Installation, w *stage.World, def client.PipelineDefinition, label string) (client.Run, string, string, string) {
	t.Helper()
	item := w.WorkItem(label, "# "+label)
	w.Provision(item.Id)
	run := w.Run(label, item, def.Id)
	attempt := w.WaitAttempt(run.Id, time.Minute)
	ns := w.Namespace(w.TenantA)
	job := i.WaitJob(ns, attempt, true, 2*time.Minute)
	pod := i.WaitPod(ns, job.Name, 2*time.Minute)
	i.WaitCommandBarrier(ns, pod.Name)
	return run, attempt, ns, pod.Name
}

// downloadArtifact reads a run output artifact through its presigned download.
func downloadArtifact(t *testing.T, i *stage.Installation, w *stage.World, id string) []byte {
	t.Helper()
	download := true
	art, err := i.API("engineer").GetArtifactWithResponse(context.Background(), w.TenantA, w.Project, id, &client.GetArtifactParams{Download: &download})
	if err != nil || art.StatusCode() != http.StatusOK {
		t.Fatalf("artifact %s: %v", id, err)
	}
	var transfer struct {
		Download struct {
			URL     string            `json:"url"`
			Headers map[string]string `json:"headers"`
		} `json:"download"`
	}
	if err := json.Unmarshal(art.Body, &transfer); err != nil || transfer.Download.URL == "" {
		t.Fatal("the artifact record carries no download instruction")
	}
	req, _ := http.NewRequest(http.MethodGet, transfer.Download.URL, nil)
	for k, v := range transfer.Download.Headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal("artifact download failed (URL suppressed)")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("artifact download: HTTP %d", resp.StatusCode)
	}
	return data
}

// ---- Loki through the operator-authenticated Grafana ------------------------------------------------------

const observability = "kodama-observability"

type grafana struct {
	base, user, pass string
}

func openGrafana(t *testing.T, i *stage.Installation) grafana {
	t.Helper()
	ctx := i.Context(time.Minute)
	host, err := i.Kube.Run(ctx, "-n", observability, "get", "ingress", "kodama-grafana", "-o", "jsonpath={.spec.rules[0].host}")
	if err != nil || strings.TrimSpace(host) == "" {
		t.Fatalf("the Grafana ingress is not deployed: %v", err)
	}
	user, err := i.Kube.SecretValue(ctx, observability, "kodama-grafana-admin", "admin-user")
	if err != nil {
		t.Fatalf("Grafana operator credential: %v", err)
	}
	pass, err := i.Kube.SecretValue(ctx, observability, "kodama-grafana-admin", "admin-password")
	if err != nil {
		t.Fatalf("Grafana operator credential: %v", err)
	}
	return grafana{base: "https://" + strings.TrimSpace(host), user: user, pass: pass}
}

func (g grafana) get(path string, authenticated bool) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, g.base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if authenticated {
		req.SetBasicAuth(g.user, g.pass)
	}
	// Do not follow the login redirect: an unauthenticated caller must see a refusal, not a page.
	client := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, body, nil
}

// lines runs a LogQL range query against the Loki datasource and returns the log lines found.
func (g grafana) lines(query string, since time.Duration) (int, []string, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", fmt.Sprint(time.Now().Add(-since).UnixNano()))
	q.Set("end", fmt.Sprint(time.Now().Add(time.Minute).UnixNano()))
	q.Set("limit", "200")
	code, body, err := g.get("/api/datasources/proxy/uid/kodama-loki/loki/api/v1/query_range?"+q.Encode(), true)
	if err != nil || code != http.StatusOK {
		return code, nil, err
	}
	var out struct {
		Data struct {
			Result []struct {
				Values [][]string `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return code, nil, fmt.Errorf("loki answer is not valid JSON")
	}
	var lines []string
	for _, r := range out.Data.Result {
		for _, v := range r.Values {
			if len(v) == 2 {
				lines = append(lines, v[1])
			}
		}
	}
	return code, lines, nil
}

// deployedEnv returns the value of an environment variable of the server container in a Deployment's JSON,
// so an assertion is about the variable's value and not about a substring of the whole document.
func deployedEnv(t *testing.T, deployJSON, name string) (string, bool) {
	t.Helper()
	var d struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string `json:"name"`
						Env  []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal([]byte(deployJSON), &d); err != nil {
		t.Fatal("the Deployment is not valid JSON")
	}
	for _, c := range d.Spec.Template.Spec.Containers {
		if c.Name != "server" {
			continue
		}
		for _, e := range c.Env {
			if e.Name == name {
				return e.Value, true
			}
		}
	}
	return "", false
}
