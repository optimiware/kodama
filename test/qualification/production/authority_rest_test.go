//go:build qualification

package production

import (
	"context"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestStageAuthorityRESTReads is the live REST authority probe for slice 2: the
// admin token (the only onboarded project member on the stage) lists
// executions, starts a task-free inline execution, inspects exactly that
// execution and reads the project inbox, while no token and a garbage token are
// refused as unauthenticated on the same read set. The runner may still capture
// Q02_DEVELOPER_TOKEN; this probe never uses it.
func TestStageAuthorityRESTReads(t *testing.T) {
	stageWave1Parallel(t)
	config := loadStageConfig(t)
	requireStageInputs(t, config,
		InputAPIURL, InputCAPEM, InputTLSServerName,
		InputAdminToken, InputProjectID, InputImage, InputEvidenceDir,
	)
	client := newStageClient(t, config)
	ctx := context.Background()
	started := time.Now().UTC()

	listResult, err := client.Get(ctx, config.AdminToken, "execution.list",
		api.ExecutionListRequest{ProjectID: config.ProjectID}, "")
	if err != nil {
		t.Fatalf("the authenticated execution.list was refused: %v", err)
	}
	if _, err := decodeStageResult[api.ExecutionListResult](listResult, "execution.list"); err != nil {
		t.Fatalf("the authenticated execution.list returned an invalid result: %v", err)
	}

	document, err := singleActionPipeline(config.Image)
	if err != nil {
		t.Fatalf("build the inline probe document: %v", err)
	}
	startKey, err := probeIdempotencyKey("q02-rest-start")
	if err != nil {
		t.Fatalf("generate the inline start key: %v", err)
	}
	executionID, err := startInlineExecution(ctx, client, config.AdminToken, config.ProjectID, document, startKey)
	if err != nil {
		t.Fatalf("the inline pipeline.start was refused: %v", err)
	}

	inspectResult, err := client.Get(ctx, config.AdminToken, "execution.inspect",
		api.ExecutionInspectRequest{ExecutionID: executionID}, "")
	if err != nil {
		t.Fatalf("the authenticated execution.inspect was refused: %v", err)
	}
	inspect, err := decodeStageResult[api.ExecutionInspectResult](inspectResult, "execution.inspect")
	if err != nil {
		t.Fatalf("the authenticated execution.inspect returned an invalid result: %v", err)
	}
	if inspect.ExecutionID != executionID {
		t.Fatalf("execution.inspect returned execution %q, want the freshly started %q", inspect.ExecutionID, executionID)
	}
	if len(inspect.Actions) != 1 || inspect.Actions[0].Name != "probe" {
		t.Fatalf("the inline execution actions = %+v, want the one probe action", inspect.Actions)
	}

	inboxResult, err := client.Get(ctx, config.AdminToken, "task.inbox",
		api.TaskInboxRequest{ProjectID: config.ProjectID}, "")
	if err != nil {
		t.Fatalf("the authenticated task.inbox was refused: %v", err)
	}
	if _, err := decodeStageResult[api.TaskInboxResult](inboxResult, "task.inbox"); err != nil {
		t.Fatalf("the authenticated task.inbox returned an invalid result: %v", err)
	}

	reads := []struct {
		name string
		call func(Secret) error
	}{
		{"execution.list", func(token Secret) error {
			_, err := client.Get(ctx, token, "execution.list", api.ExecutionListRequest{ProjectID: config.ProjectID}, "")
			return err
		}},
		{"execution.inspect", func(token Secret) error {
			_, err := client.Get(ctx, token, "execution.inspect", api.ExecutionInspectRequest{ExecutionID: executionID}, "")
			return err
		}},
		{"task.inbox", func(token Secret) error {
			_, err := client.Get(ctx, token, "task.inbox", api.TaskInboxRequest{ProjectID: config.ProjectID}, "")
			return err
		}},
	}
	for _, read := range reads {
		for _, token := range []Secret{"", "garbage-token"} {
			expectPublicRefusal(t, read.call(token), 401, api.CodeUnauthenticated)
		}
	}

	finished := time.Now().UTC()
	writeStageEvidence(t, config, stageEvidence{
		Name:        "stage-authority-rest",
		Probe:       "TestStageAuthorityRESTReads",
		Started:     started,
		Finished:    finished,
		ExecutionID: executionID,
	})
}
