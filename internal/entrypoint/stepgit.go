package entrypoint

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// StepGitCommandName is the subcommand a step's command runs to obtain the two
// runtime-issued Git grants of runtime v3.
const StepGitCommandName = "step-git"

// The supervising entrypoint advertises its grant broker to the command through
// these two variables (only when the context advertises a runtime grant).
const (
	EnvGitSocket = "KODAMA_STEP_GIT_SOCKET"
	EnvGitKey    = "KODAMA_STEP_GIT_KEY"
)

// StepGitCommand returns the `kodama step-git` registration. The composition root
// registers it beside StepCommand().
func StepGitCommand() Command {
	return Command{
		Name:    StepGitCommandName,
		Summary: "Ask the supervising entrypoint to obtain the seed or integration Git grant of the running v3 attempt",
		Run:     RunStepGit,
	}
}

// grantRequest is the one message of the broker's local protocol: what the command
// asks the entrypoint to request. It carries no secret except the per-attempt key.
type grantRequest struct {
	Key          string `json:"key"`
	Kind         string `json:"kind"` // seed | integration
	RepositoryID string `json:"repositoryId"`
	URL          string `json:"url,omitempty"`
	Dir          string `json:"dir,omitempty"`
	GateID       string `json:"gateId,omitempty"`
}

// grantReply is the broker's answer: a summary or a refusal, never a credential.
type grantReply struct {
	OK      bool   `json:"ok"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
}

// RunStepGit runs `kodama step-git seed|integration`:
//
//	kodama step-git seed --repository <id> --url <cloneUrl> --dir <dir>
//	kodama step-git integration --repository <id> --gate <id>
//
// It does not call the control plane and never holds a credential: it asks the
// supervising entrypoint (EnvGitSocket, EnvGitKey) to make the route call with
// the attempt's token. The entrypoint registers the credential it receives in its
// redaction and disclosure set BEFORE it writes it to the file the context
// advertised (0600) and before the command can read it, and answers with the
// grant ID and ref only, which this command prints. It does not push: after the
// command succeeds the entrypoint pushes exactly the one update the grant allows
// (the seed: the commit at HEAD of --dir, which must have at least one commit, to
// the default branch; the integration: the approved candidate to the integration
// ref), scans it for injected values and reports the commit with its push record.
//
// When the control plane answers that the default branch is already at the approved
// candidate (409 publication_conflict, reason already_integrated and a well-formed
// observedTip: the control plane's own statement, the workspace is not consulted), no grant
// exists and none is needed: the command succeeds, printing that there is nothing to
// push, and the attempt has no push and no commit output for that repository. Any other
// 409 stays a refusal and the step fails as publication_conflict.
func RunStepGit(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "seed" && args[0] != "integration") {
		fmt.Fprintln(stderr, "usage: kodama step-git seed --repository ID --url URL --dir DIR | integration --repository ID --gate ID")
		return ExitUsage
	}
	kind := args[0]
	fs := flag.NewFlagSet("kodama step-git "+kind, flag.ContinueOnError)
	fs.SetOutput(stderr)
	req := grantRequest{Kind: kind}
	socket := fs.String("socket", getenv(EnvGitSocket), "the entrypoint's grant socket")
	fs.StringVar(&req.Key, "key", getenv(EnvGitKey), "the per-attempt key")
	fs.StringVar(&req.RepositoryID, "repository", "", "repository ID")
	if kind == "seed" {
		fs.StringVar(&req.URL, "url", "", "clone URL of the repository (beneath the Git base URL)")
		fs.StringVar(&req.Dir, "dir", "", "local repository directory inside the run workspace whose HEAD is the seed")
	} else {
		fs.StringVar(&req.GateID, "gate", "", "approving gate ID")
	}
	if err := fs.Parse(args[1:]); err != nil {
		return ExitUsage
	}
	if fs.NArg() != 0 || *socket == "" || req.Key == "" || req.RepositoryID == "" ||
		(kind == "seed" && (req.URL == "" || req.Dir == "")) || (kind == "integration" && req.GateID == "") {
		fmt.Fprintln(stderr, "kodama step-git: missing or unexpected arguments (or no grant capability in this attempt)")
		return ExitUsage
	}
	if req.Dir != "" {
		if abs, err := filepath.Abs(req.Dir); err == nil {
			req.Dir = abs
		}
	}
	reply, err := askBroker(ctx, *socket, req)
	if err != nil {
		fmt.Fprintln(stderr, "kodama step-git:", err)
		return ExitError
	}
	if !reply.OK {
		fmt.Fprintln(stderr, "kodama step-git:", reply.Error)
		return ExitError
	}
	_, _ = fmt.Fprintln(stdout, reply.Summary)
	return ExitOK
}

func askBroker(ctx context.Context, socket string, req grantRequest) (grantReply, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	conn, err := d.DialContext(dctx, "unix", socket)
	if err != nil {
		return grantReply{}, errors.New("the supervising entrypoint is not reachable")
	}
	defer conn.Close()
	if dl, ok := dctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return grantReply{}, errors.New("the request could not be sent")
	}
	var reply grantReply
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&reply); err != nil {
		return grantReply{}, errors.New("no answer from the supervising entrypoint")
	}
	return reply, nil
}

// writePrivateFile atomically replaces path with a file only its owner can read.
func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".kodama-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
