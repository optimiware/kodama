package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

type dispatchRecorder struct {
	calls []string
	code  int
}

func (recorder *dispatchRecorder) runner(name string) runner {
	return func(context.Context, []string, io.Writer, io.Writer) int {
		recorder.calls = append(recorder.calls, name)
		return recorder.code
	}
}

func TestDispatchTable(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"dev", []string{"dev"}, "dev"},
		{"up", []string{"up"}, "up"},
		{"down", []string{"down"}, "down"},
		{"status", []string{"status"}, "status"},
		{"server", []string{"server"}, "server"},
		{"git serve", []string{"git", "serve"}, "gitServe"},
		{"auth serve", []string{"auth", "serve"}, "authServe"},
		{"auth principal-id", []string{"auth", "principal-id"}, "authPrincipalID"},
		{"agent", []string{"agent"}, "agent"},
		{"services", []string{"services"}, "services"},
		{"migrate", []string{"migrate"}, "migrate"},
		{"bootstrap-admin", []string{"bootstrap-admin"}, "bootstrap"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := &dispatchRecorder{}
			deps := deps{
				dev:             recorder.runner("dev"),
				up:              recorder.runner("up"),
				down:            recorder.runner("down"),
				status:          recorder.runner("status"),
				server:          recorder.runner("server"),
				gitServe:        recorder.runner("gitServe"),
				authServe:       recorder.runner("authServe"),
				authPrincipalID: recorder.runner("authPrincipalID"),
				agent:           recorder.runner("agent"),
				services:        recorder.runner("services"),
				migrate:         recorder.runner("migrate"),
				bootstrap:       recorder.runner("bootstrap"),
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), testCase.args, &stdout, &stderr, deps)
			if code != 0 {
				t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
			}
			if len(recorder.calls) != 1 || recorder.calls[0] != testCase.want {
				t.Fatalf("calls = %v; want [%s]", recorder.calls, testCase.want)
			}
		})
	}
}

func TestDefaultDepsRouteEveryCommandToItsOwnEntry(t *testing.T) {
	// Every entry in the defaultDeps table is easy to swap: a dispatch test
	// that injects its own runners cannot catch a wrong default. Drive the real
	// default table with arguments each entry refuses in its own words, so a
	// swapped entry is caught by the message the wrong entry prints.
	for _, testCase := range []struct {
		name string
		args []string
		env  []string
		want string
	}{
		{
			name: "dev reaches the dev entry",
			args: []string{"dev", "--state-dir", "relative/path"},
			want: "--state-dir \"relative/path\" must be an absolute path",
		},
		{
			name: "up reaches the up entry",
			args: []string{"up", "extra"},
			want: "up: unexpected argument \"extra\"",
		},
		{
			name: "down reaches the down entry",
			args: []string{"down", "extra"},
			want: "down: unexpected argument \"extra\"",
		},
		{
			name: "status reaches the status entry",
			args: []string{"status", "extra"},
			want: "status: unexpected argument \"extra\"",
		},
		{
			name: "migrate reaches the migrate entry",
			args: []string{"migrate", "--force"},
			want: "migrate takes no arguments",
		},
		{
			name: "bootstrap-admin reaches the bootstrap entry",
			args: []string{"bootstrap-admin"},
			want: "bootstrap-admin requires --tenant-name and --token-file",
		},
		{
			name: "agent reaches the agent entry",
			args: []string{"agent"},
			env:  []string{"KODAMA_TASK", "KODAMA_EXECUTION", "KODAMA_MODEL", "KODAMA_PROJECT", "KODAMA_CHECKPOINT_DIR"},
			want: "agent requires --task, --execution, --model and at least one --message",
		},
		{
			name: "services reaches the services entry",
			args: []string{"services"},
			env:  []string{"KODAMA_SERVICE_KIND", "KODAMA_SERVICE_ID", "KODAMA_SERVICE_TENANT", "KODAMA_SERVICE_PROJECT", "KODAMA_EMAIL_MESSAGE"},
			want: "services requires --kind, --service, --tenant and --project",
		},
		{
			name: "git serve reaches the git serve entry",
			args: []string{"git", "serve"},
			env:  []string{"KODAMA_GIT_TENANT", "KODAMA_GIT_STORAGE_ROOT", "KODAMA_GIT_JOURNAL_DIR"},
			want: "git serve requires --tenant, --storage-root and --journal-dir",
		},
		{
			name: "git hook reaches the git hook entry",
			args: []string{"git", "hook"},
			want: "git hook requires a phase",
		},
		{
			name: "auth serve reaches the auth serve entry",
			args: []string{"auth", "serve"},
			env:  []string{"KODAMA_AUTH_ISSUER"},
			want: "auth serve requires --issuer",
		},
		{
			name: "auth principal-id reaches the auth principal-id entry",
			args: []string{"auth", "principal-id"},
			env:  []string{"KODAMA_AUTH_ISSUER"},
			want: "auth principal-id requires --issuer and exactly one of --subject, --github, --email or --local",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			for _, name := range testCase.env {
				t.Setenv(name, "")
			}
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), testCase.args, &stdout, &stderr, defaultDeps())
			if code != 2 {
				t.Fatalf("exit code = %d; want 2 (stderr=%q)", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), testCase.want) {
				t.Fatalf("stderr = %q; want the dispatched entry's own refusal %q", stderr.String(), testCase.want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("a refused command wrote stdout: %q", stdout.String())
			}
		})
	}
}

func TestUnknownCommandRefused(t *testing.T) {
	recorder := &dispatchRecorder{}
	deps := deps{
		server:    recorder.runner("server"),
		gitServe:  recorder.runner("gitServe"),
		agent:     recorder.runner("agent"),
		services:  recorder.runner("services"),
		migrate:   recorder.runner("migrate"),
		bootstrap: recorder.runner("bootstrap"),
	}
	for _, args := range [][]string{
		{},
		{"unknown"},
		{"git"},
		{"git", "push"},
		{"auth"},
		{"auth", "unknown"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), args, &stdout, &stderr, deps)
		if code != 2 {
			t.Fatalf("args %v exit code = %d; want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "usage: kodama") {
			t.Fatalf("args %v stderr = %q; want usage", args, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("args %v wrote usage to stdout", args)
		}
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("a refused command started %v", recorder.calls)
	}
}

func TestHelpWritesUsageToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"help"}, &stdout, &stderr, deps{})
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if !strings.Contains(stdout.String(), "usage: kodama") {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestDispatchIsolation(t *testing.T) {
	// Each subcommand must call only its own runner. A runner that records any
	// unexpected invocation fails the test.
	unexpected := func(name string) runner {
		return func(context.Context, []string, io.Writer, io.Writer) int {
			t.Errorf("%s started an unrelated runner", name)
			return 1
		}
	}
	for _, testCase := range []struct {
		args []string
		want string
	}{
		{[]string{"dev"}, "dev"},
		{[]string{"up"}, "up"},
		{[]string{"down"}, "down"},
		{[]string{"status"}, "status"},
		{[]string{"server"}, "server"},
		{[]string{"git", "serve"}, "gitServe"},
		{[]string{"auth", "serve"}, "authServe"},
		{[]string{"auth", "principal-id"}, "authPrincipalID"},
		{[]string{"agent"}, "agent"},
		{[]string{"services"}, "services"},
		{[]string{"migrate"}, "migrate"},
		{[]string{"bootstrap-admin"}, "bootstrap"},
	} {
		var started []string
		makeRunner := func(name string) runner {
			return func(context.Context, []string, io.Writer, io.Writer) int {
				started = append(started, name)
				return 0
			}
		}
		deps := deps{
			dev:             makeRunner("dev"),
			up:              makeRunner("up"),
			down:            makeRunner("down"),
			status:          makeRunner("status"),
			server:          makeRunner("server"),
			gitServe:        makeRunner("gitServe"),
			authServe:       makeRunner("authServe"),
			authPrincipalID: makeRunner("authPrincipalID"),
			agent:           makeRunner("agent"),
			services:        makeRunner("services"),
			migrate:         makeRunner("migrate"),
			bootstrap:       makeRunner("bootstrap"),
		}
		// Replace every other runner with a failure.
		for name := range map[string]struct{}{
			"dev": {}, "up": {}, "down": {}, "status": {},
			"server": {}, "gitServe": {}, "authServe": {}, "authPrincipalID": {},
			"agent": {}, "services": {}, "migrate": {}, "bootstrap": {},
		} {
			if name == testCase.want {
				continue
			}
			switch name {
			case "dev":
				deps.dev = unexpected(name)
			case "up":
				deps.up = unexpected(name)
			case "down":
				deps.down = unexpected(name)
			case "status":
				deps.status = unexpected(name)
			case "server":
				deps.server = unexpected(name)
			case "gitServe":
				deps.gitServe = unexpected(name)
			case "authServe":
				deps.authServe = unexpected(name)
			case "authPrincipalID":
				deps.authPrincipalID = unexpected(name)
			case "agent":
				deps.agent = unexpected(name)
			case "services":
				deps.services = unexpected(name)
			case "migrate":
				deps.migrate = unexpected(name)
			case "bootstrap":
				deps.bootstrap = unexpected(name)
			}
		}
		var stdout, stderr bytes.Buffer
		if code := run(context.Background(), testCase.args, &stdout, &stderr, deps); code != 0 {
			t.Fatalf("args %v exit code = %d; stderr=%s", testCase.args, code, stderr.String())
		}
		if len(started) != 1 || started[0] != testCase.want {
			t.Fatalf("args %v started %v; want [%s]", testCase.args, started, testCase.want)
		}
	}
}
