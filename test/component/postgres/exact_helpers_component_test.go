//go:build component

package postgrescomponent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/id"
	"github.com/optimiware/kodama/internal/kernel/definition"
	"github.com/optimiware/kodama/internal/shell/pipeline"
	"github.com/optimiware/kodama/internal/shell/store/units/execution/snapshotdoc"
	pgtest "github.com/optimiware/kodama/test/support/infrastructure/postgres"
)

// admittedActionWant is one expected admitted action of a stored snapshot.
type admittedActionWant struct {
	Name    id.ActionName
	Kind    definition.ActionKind
	Timeout time.Duration
}

// admittedSnapshotWant states the fields of an executions.snapshot document a
// test can name exactly. The document as a whole is product-built, so it is
// asserted through the product decoder, field by field, never by presence.
type admittedSnapshotWant struct {
	PipelineID      pipeline.PipelineID
	PipelineVersion uint64
	YAML            []byte
	Actions         []admittedActionWant
}

// admittedSnapshot matches a stored execution snapshot against want: the
// pipeline revision, the exact admitted YAML and its digest, and the ordered
// admitted actions.
func admittedSnapshot(want admittedSnapshotWant) pgtest.GoldenMatcher {
	return pgtest.GoldenJSONSatisfies("admitted execution snapshot", func(raw []byte) error {
		admitted, err := snapshotdoc.Decode(raw)
		if err != nil {
			return fmt.Errorf("decode: %w", err)
		}
		if admitted.PipelineID != want.PipelineID {
			return fmt.Errorf("pipeline id = %q; want %q", admitted.PipelineID, want.PipelineID)
		}
		if admitted.PipelineVersion != want.PipelineVersion {
			return fmt.Errorf("pipeline version = %d; want %d", admitted.PipelineVersion, want.PipelineVersion)
		}
		if !bytes.Equal(admitted.Snapshot.YAMLBytes(), want.YAML) {
			return fmt.Errorf("admitted yaml = %q; want %q", admitted.Snapshot.YAMLBytes(), want.YAML)
		}
		if got, wantDigest := admitted.Snapshot.YAMLSHA256[:], pgtest.SHA256(want.YAML); !bytes.Equal(got, wantDigest) {
			return fmt.Errorf("snapshot yaml digest = %x; want %x", got, wantDigest)
		}
		if len(admitted.Actions) != len(want.Actions) {
			return fmt.Errorf("admitted %d actions; want %d", len(admitted.Actions), len(want.Actions))
		}
		for index, action := range admitted.Actions {
			expected := want.Actions[index]
			if action.Index != index || action.Name != expected.Name || action.Kind != expected.Kind || action.Timeout != expected.Timeout {
				return fmt.Errorf("action %d = {%d %q %q %s}; want {%d %q %q %s}", index,
					action.Index, action.Name, action.Kind, action.Timeout,
					index, expected.Name, expected.Kind, expected.Timeout)
			}
		}
		return nil
	})
}

// rawJSON returns document as a value GoldenJSONOf can compare exactly. It is
// for documents the test built itself (seeded rows).
func rawJSON(document []byte) json.RawMessage { return json.RawMessage(document) }

// requireReplayOf proves a retry returned the original result: the replayed
// value must equal the first value in every field except that Replayed is true.
func requireReplayOf[T any](t testing.TB, label string, first, replayed T) {
	t.Helper()
	want := first
	value := reflect.ValueOf(&want).Elem()
	if value.Kind() != reflect.Struct {
		t.Fatalf("%s: result type %T is not a struct", label, first)
	}
	flag := value.FieldByName("Replayed")
	if !flag.IsValid() || flag.Kind() != reflect.Bool || !flag.CanSet() {
		t.Fatalf("%s: result type %T has no settable Replayed bool", label, first)
	}
	flag.SetBool(true)
	if !reflect.DeepEqual(want, replayed) {
		t.Fatalf("%s: replay = %+v; want the original result %+v with Replayed=true", label, replayed, want)
	}
}
