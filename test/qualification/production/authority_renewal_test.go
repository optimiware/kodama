//go:build qualification

package production

import (
	"context"
	"testing"
	"time"
)

// TestStageAuthorityCredentialRenewal proves genuine mounted-token renewal on
// the live stage using the ADMIN token, the only onboarded project member. The
// two actions run on the digest-pinned curl image as its non-root user, so the
// first action never writes the workspace: it inspects its own execution and
// submits only its credential's sha256 fingerprint through the public output
// operation, and the second action reads the settled read-only output mount.
// The second action first requires the still-unexpired credential to be reused
// at activation (S05 "Consecutive container | Reuse still-valid token"), then
// waits past the first action's deadline and requires the mounted file to have
// been replaced by the renewal loop, and finally requires the replacement to
// authenticate against the public API. Nothing beyond those spec sentences is
// asserted. The whole probe is bounded below the 10-minute default go test
// timeout used by run-stage.sh (two-minute first action, 135 s wait, 200 s poll;
// 9-minute hard cap), so the live command may keep the default or pass a larger
// -timeout without changing the probe's proof.
func TestStageAuthorityCredentialRenewal(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config,
		InputAPIURL, InputCAPEM, InputTLSServerName,
		InputAdminToken, InputProjectID, InputImage, InputEvidenceDir,
	)
	probe := stageRenewalProbe(config.Image)
	if err := probe.validateLive(); err != nil {
		t.Fatalf("the live renewal probe is not well formed: %v", err)
	}
	document, err := probe.document()
	if err != nil {
		t.Fatalf("build the renewal pipeline document: %v", err)
	}
	client := newStageClient(t, config)
	ctx, cancel := context.WithTimeout(context.Background(), renewalProbeTimeout)
	defer cancel()
	started := time.Now().UTC()

	startKey, err := probeIdempotencyKey("q02-renewal-start")
	if err != nil {
		t.Fatalf("generate the renewal start key: %v", err)
	}
	executionID, err := startInlineExecution(ctx, client, config.AdminToken, config.ProjectID, document, startKey)
	if err != nil {
		t.Fatalf("the renewal pipeline.start was refused: %v", err)
	}

	inspect, err := waitExecutionTerminal(ctx, client, config.AdminToken, executionID, 5*time.Second, renewalWaitTimeout)
	if err != nil {
		t.Fatalf("the renewal execution did not reach a terminal state: %v", err)
	}
	if inspect.State != "succeeded" {
		detail := "no failure was recorded"
		if inspect.Failure != nil {
			detail = inspect.Failure.Kind + ": " + inspect.Failure.Reason + " " + inspect.Failure.Detail
		}
		t.Fatalf("the renewal execution state = %q (%s); its workload assertions failed, read the execution logs with execution.logs.read", inspect.State, detail)
	}
	if len(inspect.Actions) != 2 {
		t.Fatalf("the renewal execution has %d actions, want 2", len(inspect.Actions))
	}
	if inspect.Actions[0].TimeoutSeconds != int64(probe.FirstTimeout/time.Second) {
		t.Fatalf("the first action timeout = %d s, want the configured short timeout", inspect.Actions[0].TimeoutSeconds)
	}
	if inspect.Actions[1].TimeoutSeconds != int64(probe.SecondTimeout/time.Second) {
		t.Fatalf("the second action timeout = %d s, want the configured long timeout", inspect.Actions[1].TimeoutSeconds)
	}
	for index, action := range inspect.Actions {
		if action.State != "succeeded" {
			t.Fatalf("renewal action %d (%s) state = %q, want succeeded", index, action.Name, action.State)
		}
	}
	if inspect.Actions[0].Outputs["fingerprint"] == "" {
		t.Fatal("the first action's settled fingerprint output is missing; the second action had no reuse proof to read")
	}

	finished := time.Now().UTC()
	writeStageEvidence(t, config, stageEvidence{
		Name:        "stage-authority-renewal",
		Probe:       "TestStageAuthorityCredentialRenewal",
		Started:     started,
		Finished:    finished,
		ExecutionID: executionID,
	})
}
