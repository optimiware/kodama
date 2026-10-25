package stage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Repo is a private repository of the stage Git server, created for one test and deleted with it.
type Repo struct {
	Name    string
	URL     string
	BaseRef string
	// Initial is the commit of the base ref when the repository was created.
	Initial string

	api  string
	user string
	pass string
}

// Git server coordinates of the stage (deploy/stage/scripts/deps-init.sh): the owner of the source repositories
// and the administrator Secret the repositories are created with.
const (
	gitDepsNamespace = "kodama-deps"
	gitAdminSecret   = "kodama-gitea-admin"
	gitOwner         = "optimi"
)

// NewRepo creates a private repository with a `main` branch holding one commit and removes it when the test ends.
func (i *Installation) NewRepo(ctx context.Context, prefix string) *Repo {
	i.T.Helper()
	u, err := url.Parse(i.S.GitOrigin)
	if err != nil {
		i.T.Fatal(err)
	}
	user, err := i.Kube.SecretValue(ctx, gitDepsNamespace, gitAdminSecret, "username")
	if err != nil {
		i.T.Fatalf("Git administrator: %v", err)
	}
	pass, err := i.Kube.SecretValue(ctx, gitDepsNamespace, gitAdminSecret, "password")
	if err != nil {
		i.T.Fatalf("Git administrator: %v", err)
	}
	r := &Repo{Name: prefix + "-" + random(4), BaseRef: "refs/heads/main", api: strings.TrimRight(i.S.GitOrigin, "/") + "/api/v1", user: user, pass: pass}
	r.URL = fmt.Sprintf("%s/%s/%s.git", strings.TrimRight(u.String(), "/"), gitOwner, r.Name)

	body, _ := json.Marshal(map[string]any{"name": r.Name, "private": true, "auto_init": true, "default_branch": "main", "readme": "Default", "description": "Kodama qualification fixture"})
	if code, out := r.call(ctx, http.MethodPost, "/admin/users/"+gitOwner+"/repos", body); code != http.StatusCreated {
		i.T.Fatalf("create repository: HTTP %d: %s", code, out)
	}
	i.T.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		r.call(c, http.MethodDelete, "/repos/"+gitOwner+"/"+r.Name, nil)
	})
	r.Initial = r.Commit(ctx, "main")
	return r
}

// PutFile commits a file on main through the Git server API and returns the new tip.
// It is the human push a publication conflict is raced against. Credentials are not logged.
func (r *Repo) PutFile(ctx context.Context, path, content, message string) string {
	return r.PutFileOn(ctx, "main", path, content, message)
}

// PutFileOn commits a file on branch (a short name or a refs/heads/... ref) and returns the new tip.
func (r *Repo) PutFileOn(ctx context.Context, branch, path, content, message string) string {
	branch = strings.TrimPrefix(branch, "refs/heads/")
	encoded, err := json.Marshal(map[string]any{
		"content": base64.StdEncoding.EncodeToString([]byte(content)), "message": message, "branch": branch,
	})
	if err != nil {
		return ""
	}
	code, out := r.call(ctx, http.MethodPost, "/repos/"+gitOwner+"/"+r.Name+"/contents/"+url.PathEscape(path), encoded)
	if code != http.StatusCreated && code != http.StatusOK {
		return ""
	}
	var created struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if json.Unmarshal([]byte(out), &created) == nil && created.Commit.SHA != "" {
		return created.Commit.SHA
	}
	return r.Commit(ctx, branch)
}

// Commit returns the commit a branch (or ref) of the repository points at, or "" when it does not exist.
func (r *Repo) Commit(ctx context.Context, branch string) string {
	code, out := r.call(ctx, http.MethodGet, "/repos/"+gitOwner+"/"+r.Name+"/branches/"+url.PathEscape(branch), nil)
	if code != http.StatusOK {
		return ""
	}
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if json.Unmarshal([]byte(out), &b) != nil {
		return ""
	}
	return b.Commit.ID
}

// Refs lists the repository's refs (full names) through the Git protocol view of the server.
func (r *Repo) Refs(ctx context.Context) []string {
	code, out := r.call(ctx, http.MethodGet, "/repos/"+gitOwner+"/"+r.Name+"/git/refs", nil)
	if code != http.StatusOK {
		return nil
	}
	var refs []struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if json.Unmarshal([]byte(out), &refs) != nil {
		return nil
	}
	var names []string
	for _, ref := range refs {
		names = append(names, ref.Ref+" "+ref.Object.SHA)
	}
	return names
}

// File returns the content of a file at a ref ("" and false when absent).
func (r *Repo) File(ctx context.Context, ref, path string) (string, bool) {
	code, out := r.call(ctx, http.MethodGet, "/repos/"+gitOwner+"/"+r.Name+"/raw/"+path+"?ref="+url.QueryEscape(ref), nil)
	return out, code == http.StatusOK
}

func (r *Repo) call(ctx context.Context, method, path string, body []byte) (int, string) {
	req, err := http.NewRequestWithContext(ctx, method, r.api+path, bytes.NewReader(body))
	if err != nil {
		return 0, err.Error()
	}
	req.SetBasicAuth(r.user, r.pass)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}
