package installation

// Pod-security and single-replica broker hardening checks for the kodama-stack
// chart. Inspected as text and via the bounded JSON-Schema subset without
// invoking helm or external YAML decoders.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func checkStackNATSStatefulSetHardening(body string) error {
	if !regexp.MustCompile(`(?m)^[ \t]*automountServiceAccountToken:[ \t]*false[ \t]*$`).MatchString(body) {
		return fmt.Errorf("nats StatefulSet pod spec does not set automountServiceAccountToken: false")
	}
	if !regexp.MustCompile(`(?ms)^[ \t]*securityContext:[ \t]*\r?\n[ \t]+runAsNonRoot:[ \t]*true[ \t]*\r?\n[ \t]+runAsUser:[ \t]*65532[ \t]*\r?\n[ \t]+fsGroup:[ \t]*65532[ \t]*\r?\n[ \t]+seccompProfile:[ \t]*\r?\n[ \t]+type:[ \t]*RuntimeDefault[ \t]*$`).MatchString(body) {
		return fmt.Errorf("nats StatefulSet pod securityContext lacks seccompProfile: { type: RuntimeDefault }")
	}
	if !strings.Contains(body, `hasKey .Values.nats "replicaCount"`) || !strings.Contains(body, `ne (int .Values.nats.replicaCount) 1`) || !strings.Contains(body, "nats.replicaCount must be 1") {
		return fmt.Errorf("nats StatefulSet lacks a render-time fail guard when replicaCount != 1")
	}
	return nil
}

func TestStackNATSStatefulSetHardeningAndReplicaGuard(t *testing.T) {
	body := readText(t, stackChartPath(t, "templates", "nats-statefulset.yaml"))
	if err := checkStackNATSStatefulSetHardening(body); err != nil {
		t.Fatalf("nats-statefulset.yaml: %v", err)
	}

	for _, mutation := range []struct {
		name   string
		mutate func(string) string
	}{
		{
			name: "automount enabled",
			mutate: func(src string) string {
				return strings.Replace(src, "automountServiceAccountToken: false", "automountServiceAccountToken: true", 1)
			},
		},
		{
			name: "pod seccompProfile removed",
			mutate: func(src string) string {
				return strings.Replace(src, "        fsGroup: 65532\n        seccompProfile:\n          type: RuntimeDefault", "        fsGroup: 65532", 1)
			},
		},
		{
			name: "replicaCount guard removed",
			mutate: func(src string) string {
				return strings.Replace(src, "nats.replicaCount must be 1", "nats.replicaCount ignored", 1)
			},
		},
	} {
		t.Run(mutation.name, func(t *testing.T) {
			mutated := mutation.mutate(body)
			if mutated == body {
				t.Fatalf("mutation %q did not change nats-statefulset.yaml", mutation.name)
			}
			if err := checkStackNATSStatefulSetHardening(mutated); err == nil {
				t.Fatalf("checker accepted %s", mutation.name)
			}
		})
	}
}

func TestStackNATSSchemaCapsReplicaCountAtOne(t *testing.T) {
	schema := objectAt(t, loadJSON(t, stackChartPath(t, "values.schema.json")), "values.schema.json")
	properties := objectAt(t, schema["properties"], "values.schema.json properties")
	nats := objectAt(t, properties["nats"], "values.schema.json nats")
	natsProps := objectAt(t, nats["properties"], "values.schema.json nats.properties")
	replicas := objectAt(t, natsProps["replicaCount"], "nats.properties.replicaCount")

	if replicas["type"] != "integer" {
		t.Fatalf("nats.replicaCount type = %v, want integer", replicas["type"])
	}
	if minimum, ok := schemaInt(replicas, "minimum"); !ok || minimum != 1 {
		t.Errorf("nats.replicaCount minimum = %v, want 1", replicas["minimum"])
	}
	if maximum, ok := schemaInt(replicas, "maximum"); !ok || maximum != 1 {
		t.Errorf("nats.replicaCount maximum = %v, want 1", replicas["maximum"])
	}

	if err := validateAgainstSchema(replicas, float64(1), "nats.replicaCount"); err != nil {
		t.Fatalf("schema refused nats.replicaCount=1: %v", err)
	}
	for _, invalid := range []any{float64(0), float64(2), float64(3), float64(-1), "1"} {
		if err := validateAgainstSchema(replicas, invalid, "nats.replicaCount"); err == nil {
			t.Errorf("schema accepted invalid nats.replicaCount=%v", invalid)
		}
	}
}
