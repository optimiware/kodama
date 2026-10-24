package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/optimiware/kodama/internal/domain"
	"github.com/optimiware/kodama/internal/logs/contract"
)

func newDebugLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// rawCapture freezes a bounded capture of the given writes.
func rawCapture(t *testing.T, limit int64, writes ...string) (string, captureState) {
	t.Helper()
	b, err := newBoundedLog(t.TempDir(), "raw-*", limit)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range writes {
		b.Write([]byte(w))
	}
	path, _, st := b.freeze()
	t.Cleanup(func() { os.Remove(path) })
	return path, st
}

func sanitized(t *testing.T, path string, needles [][]byte, capacity int64, ev captureEvidence) (*frozenLog, string) {
	t.Helper()
	dir := t.TempDir()
	fl, err := sanitizeCapture(path, dir, needles, capacity, ev)
	if err != nil {
		t.Fatalf("sanitizeCapture: %v", err)
	}
	data, err := os.ReadFile(fl.path)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(data)) != fl.size || fl.size > capacity {
		t.Fatalf("size = %d (file %d), capacity %d", fl.size, len(data), capacity)
	}
	return fl, string(data)
}

func wantDeclaration(t *testing.T, fl *frozenLog, c contract.Completeness, reasons ...contract.Reason) {
	t.Helper()
	if fl.completeness != c || !reflect.DeepEqual(append([]contract.Reason{}, fl.reasons...), append([]contract.Reason{}, reasons...)) {
		t.Fatalf("declaration = %s %v, want %s %v", fl.completeness, fl.reasons, c, reasons)
	}
	if _, err := commandLogMetadata("art_x", digestOfBytes(nil), fl); err != nil {
		t.Fatalf("declaration does not encode: %v", err)
	}
}

func TestLogCaptureFailures(t *testing.T) {
	t.Run("a whole capture is complete, an empty one too", func(t *testing.T) {
		path, st := rawCapture(t, 1024, "hello\n")
		fl, got := sanitized(t, path, nil, 1024, captureEvidence{state: st})
		wantDeclaration(t, fl, contract.Complete)
		if got != "hello\n" {
			t.Fatalf("log = %q", got)
		}
		path, st = rawCapture(t, 1024)
		fl, got = sanitized(t, path, nil, 1024, captureEvidence{state: st})
		wantDeclaration(t, fl, contract.Complete)
		if got != "" {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("truncation is partial and marked within the bound", func(t *testing.T) {
		path, st := rawCapture(t, 200, strings.Repeat("x", 5000))
		fl, got := sanitized(t, path, nil, 200, captureEvidence{state: st})
		wantDeclaration(t, fl, contract.Partial, contract.ReasonTruncated)
		if !strings.HasSuffix(got, truncationMarker) {
			t.Fatalf("no marker: %q", got[max(0, len(got)-40):])
		}
	})
	t.Run("a failed write cannot be called complete", func(t *testing.T) {
		b, _ := newBoundedLog(t.TempDir(), "raw-*", 1024)
		b.Write([]byte("before"))
		b.f.Close() // the next write fails
		if n, err := b.Write([]byte("after")); n != 5 || err != nil {
			t.Fatalf("the pipe must keep draining: %d %v", n, err)
		}
		path, _, st := b.freeze()
		defer os.Remove(path)
		if st.writeErr == nil || !st.truncated {
			t.Fatalf("state = %+v", st)
		}
		fl, got := sanitized(t, path, nil, 1024, captureEvidence{state: st})
		// The test closed the file itself, so freeze's close failed too.
		wantDeclaration(t, fl, contract.Partial, contract.ReasonTruncated, contract.ReasonInterrupted)
		if !strings.HasPrefix(got, "before") {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("a failed close is not complete", func(t *testing.T) {
		b, _ := newBoundedLog(t.TempDir(), "raw-*", 1024)
		b.Write([]byte("data"))
		b.f.Close() // freeze closes again and fails
		path, _, st := b.freeze()
		defer os.Remove(path)
		if st.closeErr == nil {
			t.Fatal("close error not recorded")
		}
		fl, _ := sanitized(t, path, nil, 1024, captureEvidence{state: st})
		wantDeclaration(t, fl, contract.Partial, contract.ReasonInterrupted)
	})
	t.Run("writes after the freeze are dropped and counted", func(t *testing.T) {
		b, _ := newBoundedLog(t.TempDir(), "raw-*", 1024)
		b.Write([]byte("kept"))
		path, size, _ := b.freeze()
		defer os.Remove(path)
		b.Write([]byte("late"))
		if data, _ := os.ReadFile(path); string(data) != "kept" || size != 4 {
			t.Fatalf("frozen capture changed: %q", data)
		}
		if !b.truncated {
			t.Fatal("a late write must count as dropped output")
		}
	})
	t.Run("stopped command, pipe error and abandoned drain are partial", func(t *testing.T) {
		for name, ev := range map[string]captureEvidence{
			"stopped":    {stopped: true},
			"drain err":  {drainErr: errors.New("read: input/output error")},
			"incomplete": {incomplete: true},
		} {
			path, st := rawCapture(t, 1024, "some output\n")
			ev.state = st
			fl, _ := sanitized(t, path, nil, 1024, ev)
			if fl.completeness == contract.Complete {
				t.Fatalf("%s: claimed complete", name)
			}
			wantDeclaration(t, fl, contract.Partial, contract.ReasonInterrupted)
		}
	})
	t.Run("redaction expansion stays within the bound", func(t *testing.T) {
		path, st := rawCapture(t, 4096, strings.Repeat("a", 1000))
		fl, got := sanitized(t, path, [][]byte{[]byte("a")}, 300, captureEvidence{state: st})
		wantDeclaration(t, fl, contract.Partial, contract.ReasonTruncated)
		if strings.Contains(got, "a") && !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("log = %q", got)
		}
		if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(got, "[REDACTED]", ""), truncationMarker, ""), "a") {
			t.Fatal("redaction left the value behind")
		}
	})
	t.Run("zero and tiny budgets never overclaim or overflow", func(t *testing.T) {
		for _, budget := range []int64{0, 1, 2, 10, 639, 1279, 1280, 4096, 64 << 10} {
			logCap := commandLogCapacity(budget)
			if logCap < 0 || logCap > budget {
				t.Fatalf("budget %d: capacity %d", budget, logCap)
			}
			if budget >= 2*sidecarReserve && logCap+sidecarReserve+min(budget/16, notesReserveMax) > budget {
				t.Fatalf("budget %d: capacity %d leaves no room for sidecar and notes", budget, logCap)
			}
			path, st := rawCapture(t, logCap, strings.Repeat("y", 200000))
			fl, got := sanitized(t, path, nil, logCap, captureEvidence{state: st})
			if fl.completeness == contract.Complete || int64(len(got)) > logCap {
				t.Fatalf("budget %d: %s, %d bytes", budget, fl.completeness, len(got))
			}
		}
	})
	t.Run("the sidecar reserve covers the worst-case document", func(t *testing.T) {
		id := domain.ArtifactID("a" + strings.Repeat("b", 127))
		b, err := contract.Encode(contract.Metadata{
			Version: contract.Version, Source: contract.SourceWrapper, LogName: contract.CommandLogName,
			Log:          contract.Ref{ArtifactID: id, Digest: "sha256:" + strings.Repeat("f", 64), SizeBytes: 1 << 40},
			Completeness: contract.Partial, Reasons: []contract.Reason{contract.ReasonTruncated, contract.ReasonInterrupted},
		})
		if err != nil || len(b) > sidecarReserve {
			t.Fatalf("worst-case sidecar is %d bytes (%v), reserve %d", len(b), err, sidecarReserve)
		}
	})
}

func TestRunProcessAbandonedDrainIsReported(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid is not installed")
	}
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	b, _ := newBoundedLog(dir, "raw-*", 1024)
	spec := procSpec{argv: sh(`setsid sh -c 'echo $$ > ` + pidFile + `; exec sleep 30' & while [ ! -s ` + pidFile + ` ]; do sleep 0.01; done; echo started; exit 0`), dir: dir, env: []string{"PATH=/usr/bin:/bin"}, out: b, grace: 300 * time.Millisecond}
	res := runProcess(context.Background(), spec)
	t.Cleanup(func() {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	path, _, st := b.freeze()
	defer os.Remove(path)
	if !res.drainIncomplete {
		t.Fatalf("an escaped descendant holding the pipe was not reported: %+v", res)
	}
	ev := captureEvidence{state: st, drainErr: res.drainErr, incomplete: res.drainIncomplete}
	if ev.whole() {
		t.Fatal("an abandoned drain must not be whole")
	}
}

func TestLogRedactionBoundaries(t *testing.T) {
	const secret = "abc-SECRETVALUE-xyz"
	needles := [][]byte{[]byte(secret)}
	t.Run("a value split across writes is removed", func(t *testing.T) {
		path, st := rawCapture(t, 4096, "pre abc-SEC", "RETVAL", "UE-xyz post")
		_, got := sanitized(t, path, needles, 4096, captureEvidence{state: st})
		if got != "pre [REDACTED] post" {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("a value larger than a chunk boundary is removed", func(t *testing.T) {
		pad := strings.Repeat("p", sanitizeChunk-5)
		path, st := rawCapture(t, 3*sanitizeChunk, pad+secret+pad)
		_, got := sanitized(t, path, needles, 3*sanitizeChunk, captureEvidence{state: st})
		if strings.Contains(got, "SECRET") || !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("value crossed a chunk boundary unredacted")
		}
	})
	t.Run("overlapping values merge into one marker", func(t *testing.T) {
		path, st := rawCapture(t, 4096, "abcdefxyz")
		_, got := sanitized(t, path, [][]byte{[]byte("abcdef"), []byte("defxyz")}, 4096, captureEvidence{state: st})
		if got != "[REDACTED]" {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("short values are not exempt", func(t *testing.T) {
		path, st := rawCapture(t, 4096, "x pw=ab y")
		_, got := sanitized(t, path, [][]byte{[]byte("ab")}, 4096, captureEvidence{state: st})
		if got != "x pw=[REDACTED] y" {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("an interrupted tail that may start a value is discarded", func(t *testing.T) {
		path, st := rawCapture(t, 4096, "line one\nabc-SECRETVA")
		_, got := sanitized(t, path, needles, 4096, captureEvidence{state: st, stopped: true})
		if got != "line one\n" {
			t.Fatalf("log = %q", got)
		}
		// A truncated capture is cut arbitrarily: same rule.
		path, st = rawCapture(t, 40, "line one\nabc-SECRETVALUE-xyz and a lot more output to drop")
		_, got = sanitized(t, path, needles, 4096, captureEvidence{state: st})
		if strings.Contains(got, "abc-") {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("a whole capture releases an innocent tail", func(t *testing.T) {
		path, st := rawCapture(t, 4096, "ends with abc-")
		_, got := sanitized(t, path, needles, 4096, captureEvidence{state: st})
		if got != "ends with abc-" {
			t.Fatalf("log = %q", got)
		}
	})
	t.Run("an unusable redaction set fails closed", func(t *testing.T) {
		path, _ := rawCapture(t, 4096, "contains "+secret)
		many := make([][]byte, 0, 300)
		for i := 0; i < 300; i++ {
			many = append(many, []byte(fmt.Sprintf("value-%03d", i)))
		}
		for name, set := range map[string][][]byte{
			"too long":  {[]byte(strings.Repeat("k", 9<<10))},
			"too many":  many,
			"all empty": nil,
		} {
			dir := t.TempDir()
			fl, err := sanitizeCapture(path, dir, set, 4096, captureEvidence{})
			if name == "all empty" {
				// Nothing to remove is a valid set.
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				fl.cleanup()
				continue
			}
			if !errors.Is(err, errRedactionSet) || fl != nil {
				t.Fatalf("%s: err = %v", name, err)
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Fatalf("%s: left %v behind", name, entries)
			}
		}
		if _, err := sanitizeNotes("n", many, 100); !errors.Is(err, errRedactionSet) {
			t.Fatalf("notes: %v", err)
		}
	})
	t.Run("notes are sanitized and bounded", func(t *testing.T) {
		b, err := sanitizeNotes("failed with "+secret+"\nmore\n", needles, 4096)
		if err != nil || strings.Contains(string(b), "SECRET") || !strings.Contains(string(b), "[REDACTED]") {
			t.Fatalf("notes = %q (%v)", b, err)
		}
		b, _ = sanitizeNotes(strings.Repeat("é", 500), needles, 200)
		if len(b) > 200 || !utf8.Valid(b) {
			t.Fatalf("notes are %d bytes, valid=%v", len(b), utf8.Valid(b))
		}
		if b, _ := sanitizeNotes("x", needles, 0); len(b) != 0 {
			t.Fatalf("a zero budget kept %q", b)
		}
	})
}
