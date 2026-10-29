package mcpserver

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func TestCommandUsage(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		code int
		text string
	}{
		"no args":    {nil, 2, "kodama mcp serve"},
		"help":       {[]string{"--help"}, 0, "kodama mcp serve"},
		"serve help": {[]string{"serve", "--help"}, 0, "api-url"},
		"unknown":    {[]string{"run"}, 2, "unknown command"},
		"incomplete": {[]string{"serve"}, 2, "api-url"},
		"extra arg":  {[]string{"serve", "extra"}, 2, "flags only"},
	} {
		t.Run(name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := Command(context.Background(), tc.args, func(string) string { return "" }, &out, &errb)
			if code != tc.code {
				t.Fatalf("code = %d, want %d (out=%s err=%s)", code, tc.code, out.String(), errb.String())
			}
			if !strings.Contains(out.String()+errb.String(), tc.text) {
				t.Fatalf("output does not contain %q: %s%s", tc.text, out.String(), errb.String())
			}
		})
	}
}

func TestCommandServeRunsAndStops(t *testing.T) {
	// A complete configuration with an unreachable issuer is enough to start
	// the listener; the command must return when the context is cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	env := map[string]string{
		"KODAMA_MCP_API_URL":  "https://kodama.example/api/v1",
		"KODAMA_MCP_RESOURCE": "https://mcp.example/mcp",
		"KODAMA_MCP_AUDIENCE": "https://api.example",
		"KODAMA_MCP_ISSUER":   "https://id.example",
		"KODAMA_MCP_LISTEN":   "127.0.0.1:0",
	}
	var errb bytes.Buffer
	code := Command(ctx, []string{"serve"}, func(k string) string { return env[k] }, io.Discard, &errb)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %s", code, errb.String())
	}
}
