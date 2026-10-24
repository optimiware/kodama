package main

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
)

// commitWorkspace commits whatever the agent changed on top of the pinned
// checkout and returns the candidate commit (HEAD). The conformance report
// must name the candidate, and the candidate exists only once it is committed;
// the entrypoint later proposes it through the workspace protocol and checks
// that it descends from the pinned commit. The wrapper never pushes and holds
// no Git credential.
func commitWorkspace(ctx context.Context, workspace, summary string, redact func(string) string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	git := func(args ...string) (string, error) {
		full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "core.fsmonitor=false"}, args...)
		cmd := exec.CommandContext(ctx, "git", full...)
		cmd.Dir = workspace
		cmd.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GIT_AUTHOR_NAME=Kodama", "GIT_AUTHOR_EMAIL=step@kodama.invalid", "GIT_COMMITTER_NAME=Kodama", "GIT_COMMITTER_EMAIL=step@kodama.invalid"}
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return "", errors.New("git " + args[0] + " failed")
		}
		return strings.TrimSpace(out.String()), nil
	}
	status, err := git("status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		if _, err := git("add", "-A"); err != nil {
			return "", err
		}
		msg := firstLine(redact(summary))
		if msg == "" {
			msg = "agent change"
		}
		if _, err := git("commit", "--no-verify", "--no-gpg-sign", "-m", msg); err != nil {
			return "", err
		}
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil || len(head) != 40 {
		return "", errors.New("no candidate commit")
	}
	return head, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
