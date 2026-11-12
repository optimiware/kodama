//go:build qualification

package production

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/optimiware/kodama/api"
)

// TestStageAuthenticatedAndUnauthenticated is the first live Q02 probe. An
// authenticated execution list of the admin's own project must succeed, while
// the same request without a token and with a garbage token must be refused
// with the public unauthenticated error. Without the stage inputs the test
// skips with every missing input named; a configured but malformed input fails.
func TestStageAuthenticatedAndUnauthenticated(t *testing.T) {
	stageWave1Parallel(t)
	config, err := LoadConfig(os.Getenv)
	if err != nil {
		t.Fatalf("the configured Q02 stage inputs are malformed: %v", err)
	}
	required := []Input{
		InputAPIURL,
		InputCAPEM,
		InputTLSServerName,
		InputAdminToken,
		InputProjectID,
		InputEvidenceDir,
	}
	if missing := config.Missing(required...); !missing.Empty() {
		if os.Getenv("Q02_REQUIRE_STAGE") == "1" {
			t.Fatalf("required Q02 stage input(s) are missing: %s", missing)
		}
		t.Skipf("unconfigured Q02 stage input(s): %s", missing)
	}

	client, err := NewAPI(config)
	if err != nil {
		t.Fatalf("build the stage API helper: %v", err)
	}
	started := time.Now().UTC()
	result, err := client.Get(
		context.Background(),
		config.AdminToken,
		"execution.list",
		api.ExecutionListRequest{ProjectID: config.ProjectID},
		"",
	)
	if err != nil {
		t.Fatalf("the authenticated execution.list was refused: %v", err)
	}
	if result.Operation != "execution.list" {
		t.Fatalf("authenticated result operation = %q, want execution.list", result.Operation)
	}
	if len(bytes.TrimSpace(result.Body)) == 0 || string(bytes.TrimSpace(result.Body)) == "null" {
		t.Fatal("the authenticated execution.list returned no result body")
	}
	var list api.ExecutionListResult
	if err := json.Unmarshal(result.Body, &list); err != nil {
		t.Fatalf("the execution.list result body is not the public DTO: %v", err)
	}
	if err := list.Validate(); err != nil {
		t.Fatalf("the execution.list result body is invalid: %v", err)
	}

	for _, token := range []Secret{"", "garbage-token"} {
		_, err := client.Get(
			context.Background(),
			token,
			"execution.list",
			api.ExecutionListRequest{ProjectID: config.ProjectID},
			"",
		)
		var public *APIError
		if !errors.As(err, &public) {
			t.Fatalf("a request with an empty or garbage token was not refused with a public error: %v", err)
		}
		if public.Status != http.StatusUnauthorized {
			t.Fatalf("a request with an empty or garbage token returned status %d, want 401", public.Status)
		}
		if public.Code != api.CodeUnauthenticated {
			t.Fatalf("a request with an empty or garbage token returned code %q, want %q", public.Code, api.CodeUnauthenticated)
		}
	}
	finished := time.Now().UTC()

	writer, err := NewEvidenceWriter(config.EvidenceDir)
	if err != nil {
		t.Fatalf("the configured evidence directory is unusable: %v", err)
	}
	writer.Secrets = config.tokens()
	record := map[string]any{
		"probe":       "TestStageAuthenticatedAndUnauthenticated",
		"recorded_at": finished,
		"started_at":  started,
		"finished_at": finished,
		"duration_ms": finished.Sub(started).Milliseconds(),
		"outcome":     "passed",
	}
	if config.HarnessRevision != "" {
		record["harness_revision"] = config.HarnessRevision
	}
	if config.DeployedRevision != "" {
		record["deployed_revision"] = config.DeployedRevision
	}
	if digest := config.ImageDigest(); digest != "" {
		record["image_digest"] = digest
	}
	if _, err := writer.Write("stage-auth-probe", record); err != nil {
		t.Fatalf("write stage auth evidence: %v", err)
	}
}
