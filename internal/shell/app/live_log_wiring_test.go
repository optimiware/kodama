package app

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	natsbroker "github.com/optimiware/kodama/internal/shell/integration/nats"
	shelllog "github.com/optimiware/kodama/internal/shell/log"
	"github.com/optimiware/kodama/internal/shell/tenancy"
)

// TestDurableLiveLogPublisherUsesNATSSubjectAndPayload proves the durable
// composition wires the bounded asynchronous live publisher to the same NATS
// broker adapter as committed events, and that a live chunk is published on the
// documented scope-qualified subject with its execution/action identity.
func TestDurableLiveLogPublisherUsesNATSSubjectAndPayload(t *testing.T) {
	installClosingPools(t)
	server := &fakeNATSServer{}
	config := durableTestConfig(t, server).resolved()
	builder := &builder{cfg: config, logger: config.logger()}
	environment, err := buildEnvironment(context.Background(), builder)
	if err != nil {
		t.Fatalf("buildEnvironment: %v", err)
	}
	t.Cleanup(func() { builder.cleanup(context.Background()) })

	if environment.liveLog == nil {
		t.Fatal("durable composition has no live log publisher")
	}
	if _, ok := environment.liveLog.(*shelllog.AsyncPublisher); !ok {
		t.Fatalf("live log publisher = %T; want the bounded asynchronous publisher", environment.liveLog)
	}
	if any(environment.liveLog) == any(environment.publisher) {
		t.Fatal("live log publisher is the raw broker instead of the bounded wrapper")
	}

	scope := tenancy.Scope{TenantID: id.TenantID("tenant-1"), ProjectID: id.ProjectID("project-1")}
	chunk := shelllog.Chunk{
		ExecutionID: id.ExecutionID("execution-1"),
		ActionID:    id.ActionID("action-1"),
		Stream:      shelllog.StreamStdout,
		Offset:      32,
		Sequence:    3,
		Data:        []byte("live output\n"),
	}
	if err := environment.liveLog.PublishLive(context.Background(), scope, chunk); err != nil {
		t.Fatalf("PublishLive: %v", err)
	}

	var subjects []natsbroker.Subject
	var payloads [][]byte
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		subjects, payloads = server.livePublications()
		if len(subjects) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(subjects) != 1 || len(payloads) != 1 {
		t.Fatalf("live publications = %d; want exactly one", len(subjects))
	}
	wantSubject := natsbroker.Subject(fmt.Sprintf("kodama.v1.%s.%s.logs.%s", scope.TenantID, scope.ProjectID, chunk.ExecutionID))
	if subjects[0] != wantSubject {
		t.Fatalf("live subject = %q; want %q", subjects[0], wantSubject)
	}
	var decoded shelllog.Chunk
	if err := json.Unmarshal(payloads[0], &decoded); err != nil {
		t.Fatalf("decode live payload: %v", err)
	}
	if decoded.ExecutionID != chunk.ExecutionID || decoded.ActionID != chunk.ActionID ||
		decoded.Stream != chunk.Stream || decoded.Offset != chunk.Offset ||
		decoded.Sequence != chunk.Sequence || string(decoded.Data) != string(chunk.Data) {
		t.Fatalf("live payload = %+v; want %+v", decoded, chunk)
	}
}
