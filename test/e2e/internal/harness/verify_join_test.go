//go:build e2e

package harness

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHarnessVerifyGoTestJoinsTeeBeforeVerifying proves the verifier never
// reads its result while tee is still flushing. A slow stdout consumer plus a
// trailing skipped or failed event must always produce a non-zero exit and the
// matching message.
func TestHarnessVerifyGoTestJoinsTeeBeforeVerifying(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to run the verifier")
	}
	verifier := filepath.Join(repoRootForTest(t), "test", "e2e", "internal", "harness", "verify-go-test.sh")
	for _, action := range []string{"skip", "fail"} {
		t.Run(action, func(t *testing.T) {
			events := trailingEventStream(action)
			eventsPath := filepath.Join(t.TempDir(), "events.jsonl")
			if err := os.WriteFile(eventsPath, []byte(events), 0o600); err != nil {
				t.Fatalf("write events: %v", err)
			}
			emitter := filepath.Join(t.TempDir(), "emit.sh")
			if err := os.WriteFile(emitter, []byte("#!/bin/sh\ncat \"$EVENTS\"\n"), 0o700); err != nil {
				t.Fatalf("write emitter: %v", err)
			}
			// A tee whose last line lands late makes the flush race
			// deterministic instead of scheduler-dependent: the verifier must
			// wait for tee before reading the result file.
			bin := t.TempDir()
			slowTee := filepath.Join(bin, "tee")
			if err := os.WriteFile(slowTee, []byte(`#!/usr/bin/env bash
set -euo pipefail
file=$1
buffer=$(mktemp)
trap 'rm -f "$buffer"' EXIT
cat >"$buffer"
head -n -1 "$buffer" >"$file"
head -n -1 "$buffer"
sleep 1
tail -n 1 "$buffer" >>"$file"
tail -n 1 "$buffer"
`), 0o700); err != nil {
				t.Fatalf("write slow tee: %v", err)
			}
			// Repeat to catch any residual race rather than one lucky run.
			for iteration := range 3 {
				command := exec.Command(verifier,
					"--expect", "TestA", "--expect", "TestB",
					"--", emitter,
				)
				command.Env = append(os.Environ(),
					"EVENTS="+eventsPath,
					"PATH="+bin+":"+os.Getenv("PATH"),
				)
				stdout, err := command.StdoutPipe()
				if err != nil {
					t.Fatalf("stdout pipe: %v", err)
				}
				var stderr bytes.Buffer
				command.Stderr = &stderr
				if err := command.Start(); err != nil {
					t.Fatalf("start verifier: %v", err)
				}
				// A slow consumer: do not read the wrapper's stdout at all for a
				// while, so a tee process still holds the tail of the result.
				time.Sleep(1500 * time.Millisecond)
				if _, err := io.Copy(io.Discard, stdout); err != nil {
					t.Fatalf("drain stdout: %v", err)
				}
				err = command.Wait()
				if err == nil {
					t.Fatalf("iteration %d: a trailing %s was accepted: %s", iteration, action, stderr.String())
				}
				wanted := "skipped"
				if action == "fail" {
					wanted = "failed"
				}
				if !strings.Contains(strings.ToLower(stderr.String()), wanted) {
					t.Fatalf("iteration %d: verifier output %q does not name %q", iteration, stderr.String(), wanted)
				}
			}
		})
	}
}

// trailingEventStream emits more output than a pipe buffers, then a final
// skipped or failed event for TestB.
func trailingEventStream(action string) string {
	var stream strings.Builder
	stream.WriteString(`{"Action":"run","Test":"TestA"}` + "\n")
	filler := `{"Action":"output","Test":"TestA","Output":"` + strings.Repeat("x", 60) + `"}` + "\n"
	for range 3000 {
		stream.WriteString(filler)
	}
	stream.WriteString(`{"Action":"pass","Test":"TestA"}` + "\n")
	stream.WriteString(`{"Action":"run","Test":"TestB"}` + "\n")
	stream.WriteString(`{"Action":"` + action + `","Test":"TestB"}` + "\n")
	return stream.String()
}
