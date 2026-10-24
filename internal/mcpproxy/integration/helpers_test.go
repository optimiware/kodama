package integration_test

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	mcpfixture "github.com/optimiware/kodama/test/support/mcp"
)

func mcpfixtureOptions(extra, page int) mcpfixture.Options {
	return mcpfixture.Options{ExtraTools: extra, PageSize: page}
}

func mcpfixtureProtocols(p ...string) mcpfixture.Options { return mcpfixture.Options{Protocols: p} }

// rawPost sends one JSON-RPC POST to the proxy, optionally with a session.
func rawPost(t *testing.T, url, token, session, body string) (int, string) {
	t.Helper()
	return raw(t, http.MethodPost, url, token, session, body)
}

func raw(t *testing.T, method, url, token, session, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func mcpfixtureSink(t *testing.T) *mcpfixture.Sink { return mcpfixture.NewSink(t) }
