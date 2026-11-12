package main

import (
	"bytes"
	"context"
	"runtime"
	"testing"
)

// TestVersionCommandPrintsBuildIdentity proves `kodama version` and
// `kodama --version` print the linked version and revision and start no
// subcommand.
func TestVersionCommandPrintsBuildIdentity(t *testing.T) {
	previousVersion, previousRevision := version, revision
	t.Cleanup(func() { version, revision = previousVersion, previousRevision })
	version, revision = "1.2.3", "0123456789abcdef0123456789abcdef01234567"
	want := "kodama 1.2.3 (revision 0123456789abcdef0123456789abcdef01234567, " +
		runtime.Version() + ", " + runtime.GOOS + "/" + runtime.GOARCH + ")\n"

	for _, argument := range []string{"version", "--version"} {
		t.Run(argument, func(t *testing.T) {
			recorder := &dispatchRecorder{}
			deps := deps{
				server:                 recorder.runner("server"),
				gitServe:               recorder.runner("gitServe"),
				gitHook:                recorder.runner("gitHook"),
				gitProvision:           recorder.runner("gitProvision"),
				gitProvisionRepository: recorder.runner("gitProvisionRepository"),
				agent:                  recorder.runner("agent"),
				services:               recorder.runner("services"),
				migrate:                recorder.runner("migrate"),
				bootstrap:              recorder.runner("bootstrap"),
			}
			var stdout, stderr bytes.Buffer
			if code := run(context.Background(), []string{argument}, &stdout, &stderr, deps); code != 0 {
				t.Fatalf("exit code = %d; stderr=%s", code, stderr.String())
			}
			if stdout.String() != want {
				t.Fatalf("stdout = %q; want %q", stdout.String(), want)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q; want it empty", stderr.String())
			}
			if len(recorder.calls) != 0 {
				t.Fatalf("version started %v", recorder.calls)
			}
		})
	}
}

// TestBuildRevisionNeverEmpty proves a build without a linked revision still
// names one: the embedded VCS revision or "unknown".
func TestBuildRevisionNeverEmpty(t *testing.T) {
	previous := revision
	t.Cleanup(func() { revision = previous })
	revision = ""
	if got := buildRevision(); got == "" {
		t.Fatal("buildRevision returned an empty revision")
	}
}
