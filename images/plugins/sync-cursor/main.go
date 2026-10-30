// Command sync-cursor is a non-deployment reference plugin. It fetches one
// page, checkpoints the cursor through the generic state API and writes the
// page for the entrypoint to upload as a declared artifact.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	statev1 "github.com/optimiware/kodama/contracts/state/v1"
	pluginstate "github.com/optimiware/kodama/pkg/pluginruntime/state"
)

type cursorDoc struct {
	Cursor string `json:"cursor,omitempty"`
	Digest string `json:"digest,omitempty"`
}

func main() {
	fs := flag.NewFlagSet("sync-cursor", flag.ExitOnError)
	stateURL := fs.String("state-url", firstEnv("KODAMA_STATE_URL", "KODAMA_RUNTIME_URL", "KODAMA_SERVER_URL"), "state API base URL")
	attempt := fs.String("state-attempt", os.Getenv("KODAMA_ATTEMPT_ID"), "runtime attempt ID")
	binding := fs.String("state-binding", "", "declared state binding alias")
	tokenFile := fs.String("state-token-file", os.Getenv("KODAMA_TOKEN_FILE"), "runtime token file")
	source := fs.String("source-url", "", "fixture service URL")
	key := fs.String("state-key", "cursor", "cursor key")
	output := fs.String("output", "/kodama/outputs/page.json", "page output file")
	resultFile := fs.String("result-file", os.Getenv("KODAMA_RESULTS_FILE"), "result manifest")
	timeout := fs.Duration("timeout", time.Minute, "HTTP timeout")
	if err := fs.Parse(os.Args[1:]); err != nil || *source == "" || *binding == "" {
		fmt.Fprintln(os.Stderr, "usage: sync-cursor --source-url URL --state-binding NAME")
		os.Exit(2)
	}
	client, err := pluginstate.New(*stateURL, *attempt, *binding, *tokenFile)
	if err != nil {
		fail(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	entry, etag, err := client.Read(ctx, *key)
	var prior cursorDoc
	if err == nil {
		if err := json.Unmarshal(entry.Value, &prior); err != nil {
			fail(fmt.Errorf("decode cursor: %w", err))
		}
	} else if !errors.Is(err, pluginstate.ErrNotFound) {
		fail(err)
	}
	page, next, err := fetch(ctx, *source, prior.Cursor, *timeout)
	if err != nil {
		fail(err)
	}
	if err := os.MkdirAll(filepath.Dir(*output), 0o755); err != nil {
		fail(err)
	}
	if err := os.WriteFile(*output, page, 0o644); err != nil {
		fail(err)
	}
	body, _ := json.Marshal(cursorDoc{Cursor: next, Digest: statev1.DigestValue(json.RawMessage(fmt.Sprintf("%q", page)), nil)})
	req := statev1.WriteRequest{Value: body}
	if etag == "" {
		_, _, err = client.Create(ctx, *key, req)
	} else {
		_, _, err = client.Update(ctx, *key, etag, req)
	}
	if err != nil {
		fail(err)
	}
	if *resultFile != "" {
		raw, _ := json.Marshal(map[string]any{"outcome": "completed", "outputs": map[string]any{}})
		if err := os.MkdirAll(filepath.Dir(*resultFile), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(*resultFile, append(raw, '\n'), 0o644); err != nil {
			fail(err)
		}
	}
}

func fetch(ctx context.Context, source, cursor string, timeout time.Duration) ([]byte, string, error) {
	u, err := url.Parse(source)
	if err != nil {
		return nil, "", err
	}
	q := u.Query()
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("source returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, "", err
	}
	return data, resp.Header.Get("X-Next-Cursor"), nil
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	return ""
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sync-cursor:", err)
	os.Exit(1)
}
