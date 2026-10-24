package app

import (
	"bytes"
	"context"
	"github.com/optimiware/kodama/internal/cli/root"
	"strings"
	"testing"
)

func run(args []string, env map[string]string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Main(context.Background(), args, func(k string) string { return env[k] }, &out, &errb)
	return code, out.String(), errb.String()
}

func TestCommandsWithoutTheirInputsAreUsageErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		code int
	}{
		"migrate without database files": {[]string{"migrate"}, map[string]string{"KODAMA_SECRETS_DIR": t.TempDir()}, 2},
		"migrate with arguments":         {[]string{"migrate", "extra"}, nil, 2},
		"migrate help":                   {[]string{"migrate", "--help"}, nil, 0},
		"bootstrap without parameters":   {[]string{"bootstrap"}, nil, 2},
		"bootstrap without a token file": {[]string{"bootstrap", "--tenant-id", "t_a", "--tenant-key", "a", "--display-name", "A", "--operator-id", "op", "--eligible-subject", "s"}, nil, 2},
		"bootstrap help":                 {[]string{"bootstrap", "--help"}, nil, 0},
		"core without configuration":     {[]string{"server", "--assembly", "core"}, map[string]string{"KODAMA_SECRETS_DIR": t.TempDir()}, 2},
		"an unknown assembly":            {[]string{"server", "--assembly", "everything"}, nil, 2},
		"an unknown command":             {[]string{"deploy"}, nil, 2},
	} {
		code, out, errs := run(tc.args, tc.env)
		if code != tc.code {
			t.Errorf("%s: exit %d, want %d (%s %s)", name, code, tc.code, out, errs)
		}
	}
}

func TestSupportedCommandGroupsAreListed(t *testing.T) {
	code, out, _ := run([]string{"--help"}, nil)
	if code != 0 {
		t.Fatal(code)
	}
	for _, want := range []string{"server", "migrate", "bootstrap", "context, auth, work, workspace, pipeline, run, variable, secret, agent, gate, environment, deployment"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not mention %q: %s", want, out)
		}
	}
	// T-072 serves the routes of profiles, gates, environments and deployments.
	for _, want := range []string{"agent", "gate", "environment", "deployment"} {
		if !strings.Contains(out, want) {
			t.Errorf("help does not offer %q though its routes are served: %s", want, out)
		}
	}
	for _, banned := range []string{"model gateway", "chatops", "webhook"} {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Errorf("help offers %q though it is not served", banned)
		}
	}
}

func TestCLIOnlyOffersWhatTheCoreSliceServes(t *testing.T) {
	var out bytes.Buffer
	runner, err := NewCLI(CLIConfig{Streams: streams(&out), Getenv: func(k string) string {
		if k == "KODAMA_CONTEXT_FILE" {
			return t.TempDir() + "/c.json"
		}
		return ""
	}})
	if err != nil {
		t.Fatal(err)
	}
	runner.Run(context.Background(), []string{"run", "help"})
	if !strings.Contains(out.String(), "start") || !strings.Contains(out.String(), "logs") {
		t.Fatalf("run group: %s", out.String())
	}
	for _, g := range cliGroups {
		if !isCLIGroup(g) {
			t.Errorf("%s", g)
		}
	}
	for _, g := range []string{"gate", "agent", "environment", "deployment"} {
		if !isCLIGroup(g) {
			t.Errorf("the %s group is served by the manual delivery slice and must be offered", g)
		}
	}
	if isCLIGroup("chatops") || isCLIGroup("webhook") {
		t.Fatal("capabilities that are not served must not be offered")
	}
}

func streams(out *bytes.Buffer) root.Streams { return root.Streams{Out: out, Err: out} }
