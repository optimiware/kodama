package runprofile

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Recorder is the receipt trail of one qualification run. Every external
// command is logged with its arguments (never its stdin), exit status, a
// bounded and redacted output excerpt, and a timestamp. Registered secret
// values are replaced wherever they would appear.
type Recorder struct {
	mu      sync.Mutex
	file    *os.File
	secrets []string
	quiet   bool
	n       int
}

// Receipt is one command execution.
type Receipt struct {
	Seq      int       `json:"seq"`
	At       time.Time `json:"at"`
	Argv     []string  `json:"argv"`
	StdinSHA string    `json:"stdinSha256,omitempty"`
	Exit     int       `json:"exit"`
	Millis   int64     `json:"millis"`
	Output   string    `json:"output"`
}

const outputExcerpt = 4096

// NewRecorder opens (or creates) receipts.jsonl in dir.
func NewRecorder(dir string) (*Recorder, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "receipts.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	return &Recorder{file: f}, nil
}

// Close flushes the receipt file.
func (r *Recorder) Close() error { return r.file.Close() }

// AddSecret registers a value that must never reach a receipt or report.
func (r *Recorder) AddSecret(v string) {
	if len(v) < 6 {
		return
	}
	r.mu.Lock()
	r.secrets = append(r.secrets, v)
	r.mu.Unlock()
}

// SetQuiet suppresses command output in receipts while a secret-bearing
// command runs.
func (r *Recorder) SetQuiet(q bool) {
	r.mu.Lock()
	r.quiet = q
	r.mu.Unlock()
}

// Redact replaces registered secrets and obvious bearer material.
func (r *Recorder) Redact(s string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.secrets {
		s = strings.ReplaceAll(s, v, "***")
	}
	return s
}

func (r *Recorder) record(argv []string, stdin []byte, exit int, d time.Duration, out string) {
	r.mu.Lock()
	r.n++
	seq := r.n
	r.mu.Unlock()
	red := make([]string, len(argv))
	for i, a := range argv {
		red[i] = r.Redact(a)
	}
	out = r.Redact(out)
	r.mu.Lock()
	if r.quiet {
		out = "[output suppressed: secret-bearing]"
	}
	r.mu.Unlock()
	if len(out) > outputExcerpt {
		out = out[:outputExcerpt/2] + "\n…[truncated]…\n" + out[len(out)-outputExcerpt/2:]
	}
	rc := Receipt{Seq: seq, At: time.Now().UTC(), Argv: red, Exit: exit, Millis: d.Milliseconds(), Output: out}
	if len(stdin) > 0 {
		sum := sha256.Sum256(stdin)
		rc.StdinSHA = hex.EncodeToString(sum[:])
	}
	line, _ := json.Marshal(rc)
	r.mu.Lock()
	_, _ = r.file.Write(append(line, '\n'))
	r.mu.Unlock()
}

// Kubectl runs the pinned client against exactly one explicit kubeconfig.
// It never reads ambient KUBECONFIG, never uses a shell and never passes
// stdin contents into a receipt.
type Kubectl struct {
	Binary     string
	Kubeconfig string
	Rec        *Recorder
}

// Run executes kubectl and returns combined output.
func (k *Kubectl) Run(ctx context.Context, stdin []byte, args ...string) (string, error) {
	return k.RunReader(ctx, bytes.NewReader(stdin), stdin, args...)
}

// RunReader streams stdin; sample is only hashed into the receipt.
func (k *Kubectl) RunReader(ctx context.Context, in io.Reader, sample []byte, args ...string) (string, error) {
	argv := append([]string{"--kubeconfig", k.Kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, k.Binary, argv...)
	cmd.Env = cleanEnv()
	if in != nil {
		cmd.Stdin = in
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	// Receipts name the subcommand, not the kubeconfig path.
	k.Rec.record(args, sample, exit, time.Since(start), out.String())
	res := k.Rec.Redact(out.String())
	if err != nil {
		return res, fmt.Errorf("kubectl %s: %w: %s", firstWords(args, 4), err, tail(res, 600))
	}
	return res, nil
}

func cleanEnv() []string {
	keep := []string{"PATH", "HOME", "LANG", "TMPDIR"}
	var env []string
	for _, k := range keep {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func firstWords(args []string, n int) string {
	if len(args) > n {
		args = args[:n]
	}
	return strings.Join(args, " ")
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// JSON runs a read and decodes the result.
func (k *Kubectl) JSON(ctx context.Context, into any, args ...string) error {
	out, err := k.Run(ctx, nil, append(args, "-o", "json")...)
	if err != nil {
		return err
	}
	if i := strings.Index(out, "{"); i > 0 {
		out = out[i:] // skip client warnings merged from stderr
	}
	if err := json.Unmarshal([]byte(out), into); err != nil {
		return fmt.Errorf("decode kubectl output: %w", err)
	}
	return nil
}
