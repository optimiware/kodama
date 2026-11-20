package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/optimiware/kodema/internal/docs"
	"github.com/optimiware/kodema/internal/userland/cli"
	"github.com/optimiware/kodema/internal/userland/skill"
)

func TestEmbeddedDocsCarryNoCredentialMaterial(t *testing.T) {
	if skill.ContainsSecret([]byte(docs.MCPInstructions())) {
		t.Fatal("embedded MCPInstructions() triggered skill.ContainsSecret")
	}
	if skill.ContainsSecret([]byte(docs.SkillDocument())) {
		t.Fatal("embedded SkillDocument() triggered skill.ContainsSecret")
	}
	for _, topic := range docs.Topics() {
		if skill.ContainsSecret([]byte(topic.Content)) {
			t.Fatalf("embedded doc topic %q triggered skill.ContainsSecret", topic.Slug)
		}
	}
}

func TestDocsCommandOfflineReadListAllInstructionsAndInstall(t *testing.T) {
	// Default listing works with an empty Config (strictly offline).
	t.Run("list table and json", func(t *testing.T) {
		ports, out, errOut := testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs exit = %d; stderr=%q", code, errOut.String())
		}
		for _, topic := range docs.Topics() {
			if !strings.Contains(out.String(), topic.Slug) || !strings.Contains(out.String(), topic.URI) {
				t.Fatalf("docs table missing %q / %q:\n%s", topic.Slug, topic.URI, out.String())
			}
		}

		ports, out, errOut = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "--list", "--output", "json"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs --list --output json exit = %d; stderr=%q", code, errOut.String())
		}
		var listed struct {
			Topics []struct {
				Slug string `json:"slug"`
				URI  string `json:"uri"`
			} `json:"topics"`
		}
		if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
			t.Fatalf("decode docs json: %v\n%s", err, out.String())
		}
		if len(listed.Topics) != len(docs.Topics()) {
			t.Fatalf("docs json count = %d; want %d", len(listed.Topics), len(docs.Topics()))
		}
	})

	t.Run("single topic, all, instructions, and errors", func(t *testing.T) {
		ports, out, errOut := testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "workload"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs workload exit = %d; stderr=%q", code, errOut.String())
		}
		workload, _ := docs.Lookup("workload")
		if out.String() != workload.Content {
			t.Fatalf("docs workload content mismatch")
		}

		ports, out, errOut = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "workload", "--output", "json"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs workload --output json exit = %d; stderr=%q", code, errOut.String())
		}
		var topic docs.Topic
		if err := json.Unmarshal(out.Bytes(), &topic); err != nil || topic.Slug != "workload" || topic.Content != workload.Content {
			t.Fatalf("docs workload json = %+v (err=%v)", topic, err)
		}

		ports, out, errOut = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "--all"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs --all exit = %d; stderr=%q", code, errOut.String())
		}
		if out.String() != docs.AllMarkdown() {
			t.Fatal("docs --all content mismatch")
		}

		ports, out, errOut = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "--instructions"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs --instructions exit = %d; stderr=%q", code, errOut.String())
		}
		if strings.TrimSpace(out.String()) != docs.MCPInstructions() {
			t.Fatal("docs --instructions mismatch")
		}

		// Trailing --help flags show docs help instead of failing as unknown flags.
		for _, helpArgs := range [][]string{
			{"docs", "workload", "--help"},
			{"docs", "--list", "--help"},
		} {
			ports, out, errOut = testPorts(nil)
			if code := cli.RunWithDeps(context.Background(), cli.Config{}, helpArgs, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
				t.Fatalf("%v exit = %d; stderr=%q", helpArgs, code, errOut.String())
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("%v missing usage help:\n%s", helpArgs, out.String())
			}
		}

		// Unknown topic and mutually exclusive flags refuse with ExitUsage.
		ports, _, _ = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "missing"}, ports, cli.CommandDeps{}); code != cli.ExitUsage {
			t.Fatalf("docs missing exit = %d; want %d", code, cli.ExitUsage)
		}
		ports, _, _ = testPorts(nil)
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "--all", "--instructions"}, ports, cli.CommandDeps{}); code != cli.ExitUsage {
			t.Fatalf("docs --all --instructions exit = %d; want %d", code, cli.ExitUsage)
		}
	})

	t.Run("docs install materializes Agent Skill files", func(t *testing.T) {
		fs := newFakeFS()
		var out, errOut bytes.Buffer
		ports := cli.Ports{Files: fs, Out: &out, Err: &errOut}
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "install", "--target", "all", "--dest", "/work"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
			t.Fatalf("docs install exit = %d; stderr=%q", code, errOut.String())
		}
		for _, base := range []string{"/work/.agents/skills/kodema", "/work/.claude/skills/kodema", "/work/.opencode/skills/kodema"} {
			entry, err := fs.ReadFile(base + "/SKILL.md")
			if err != nil {
				t.Fatalf("missing %s/SKILL.md: %v", base, err)
			}
			if string(entry) != docs.SkillDocument() {
				t.Fatalf("%s/SKILL.md content mismatch", base)
			}
			for _, topic := range docs.Topics() {
				ref, err := fs.ReadFile(base + "/references/" + topic.Slug + ".md")
				if err != nil {
					t.Fatalf("missing %s/references/%s.md: %v", base, topic.Slug, err)
				}
				if string(ref) != topic.Content {
					t.Fatalf("%s/references/%s.md content mismatch", base, topic.Slug)
				}
			}
		}

		// Invalid target refuses with ExitUsage.
		if code := cli.RunWithDeps(context.Background(), cli.Config{}, []string{"docs", "install", "--target", "bogus"}, ports, cli.CommandDeps{}); code != cli.ExitUsage {
			t.Fatalf("docs install bogus exit = %d; want %d", code, cli.ExitUsage)
		}
	})
}

func TestMCPInstallOpenCodeTarget(t *testing.T) {
	cfg := cli.Config{
		ServerURL: "https://kodema.example.com",
		CAFile:    "/etc/kodema/ca.pem",
		Output:    cli.OutputTable,
	}

	// Print mode emits valid OpenCode V2 mcp.servers.<name> local entry.
	ports, out, errOut := testPorts(nil)
	if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", "opencode", "--print"}, ports, cli.CommandDeps{}); code != cli.ExitSuccess {
		t.Fatalf("mcp install opencode --print exit = %d; stderr=%q", code, errOut.String())
	}
	var snippet map[string]any
	if err := json.Unmarshal(out.Bytes(), &snippet); err != nil {
		t.Fatalf("decode opencode snippet: %v\n%s", err, out.String())
	}
	if snippet["$schema"] != "https://opencode.ai/config.json" {
		t.Fatalf("$schema = %v; want https://opencode.ai/config.json", snippet["$schema"])
	}
	mcpObj := snippet["mcp"].(map[string]any)
	servers := mcpObj["servers"].(map[string]any)
	kodema := servers["kodema"].(map[string]any)
	if kodema["type"] != "local" {
		t.Fatalf("opencode mcp entry type = %v; want local", kodema["type"])
	}
	cmdList := kodema["command"].([]any)
	wantCmd := []string{"kodema", "--url", "https://kodema.example.com", "--ca-file", "/etc/kodema/ca.pem", "mcp", "stdio"}
	if len(cmdList) != len(wantCmd) {
		t.Fatalf("command = %v; want %v", cmdList, wantCmd)
	}
	for i, want := range wantCmd {
		if cmdList[i] != want {
			t.Fatalf("command[%d] = %v; want %q", i, cmdList[i], want)
		}
	}

	// Global write merges into ~/.config/opencode/opencode.json preserving existing keys/servers.
	fakeHome := "/fake/home/dev"
	fs := newFakeFS()
	fs.files[fakeHome+"/.config/opencode/opencode.json"] = []byte(`{"model":"anthropic/claude-sonnet-4","mcp":{"servers":{"other":{"type":"remote","url":"https://other.example/mcp"}}}}`)
	deps := cli.CommandDeps{HomeDir: func() (string, error) { return fakeHome, nil }}
	var stdout, stderr bytes.Buffer
	p := cli.Ports{Files: fs, Out: &stdout, Err: &stderr}
	if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", "opencode"}, p, deps); code != cli.ExitSuccess {
		t.Fatalf("mcp install opencode exit = %d; stderr=%q", code, stderr.String())
	}
	mergedBytes, err := fs.ReadFile(fakeHome + "/.config/opencode/opencode.json")
	if err != nil {
		t.Fatalf("read opencode.json: %v", err)
	}
	var merged map[string]any
	if err := json.Unmarshal(mergedBytes, &merged); err != nil {
		t.Fatalf("unmarshal merged opencode.json: %v", err)
	}
	if merged["model"] != "anthropic/claude-sonnet-4" {
		t.Fatalf("model not preserved: %v", merged["model"])
	}
	mergedServers := merged["mcp"].(map[string]any)["servers"].(map[string]any)
	if mergedServers["other"] == nil || mergedServers["kodema"] == nil {
		t.Fatalf("mcp.servers = %v; want both other and kodema", mergedServers)
	}

	// --local writes ./opencode.json.
	stdout.Reset()
	stderr.Reset()
	if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", "opencode", "--local"}, p, deps); code != cli.ExitSuccess {
		t.Fatalf("mcp install opencode --local exit = %d; stderr=%q", code, stderr.String())
	}
	if _, err := fs.ReadFile("opencode.json"); err != nil {
		t.Fatalf("opencode.json not written by --local: %v", err)
	}

	// Existing opencode.jsonc (with comments and trailing commas) is updated in
	// place rather than creating a conflicting sibling opencode.json.
	jsoncFS := newFakeFS()
	jsoncPath := fakeHome + "/.config/opencode/opencode.jsonc"
	jsoncFS.files[jsoncPath] = []byte("{\n  // user comment with https://example.com\n  \"model\": \"anthropic/claude-sonnet-4\",\n  /* block comment */\n  \"mcp\": {\n    \"servers\": {\n      \"other\": { \"type\": \"remote\", \"url\": \"https://other.example/mcp\", },\n    },\n  },\n}\n")
	stdout.Reset()
	stderr.Reset()
	pJSONC := cli.Ports{Files: jsoncFS, Out: &stdout, Err: &stderr}
	if code := cli.RunWithDeps(context.Background(), cfg, []string{"mcp", "install", "opencode"}, pJSONC, deps); code != cli.ExitSuccess {
		t.Fatalf("mcp install opencode with jsonc exit = %d; stderr=%q", code, stderr.String())
	}
	if _, err := jsoncFS.ReadFile(fakeHome + "/.config/opencode/opencode.json"); err == nil {
		t.Fatal("created sibling opencode.json despite existing opencode.jsonc")
	}
	updatedJSONC, err := jsoncFS.ReadFile(jsoncPath)
	if err != nil {
		t.Fatalf("read updated opencode.jsonc: %v", err)
	}
	var parsedJSONC map[string]any
	if err := json.Unmarshal(updatedJSONC, &parsedJSONC); err != nil {
		t.Fatalf("unmarshal updated opencode.jsonc: %v\n%s", err, string(updatedJSONC))
	}
	if parsedJSONC["model"] != "anthropic/claude-sonnet-4" {
		t.Fatalf("jsonc model not preserved: %v", parsedJSONC["model"])
	}
	jsoncServers := parsedJSONC["mcp"].(map[string]any)["servers"].(map[string]any)
	if jsoncServers["other"] == nil || jsoncServers["kodema"] == nil {
		t.Fatalf("jsonc mcp.servers = %v; want both other and kodema", jsoncServers)
	}
}
