package pgtest

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func mustExpected(t *testing.T, value any, columnType string) any {
	t.Helper()
	normalized, err := normalizeGoldenExpected(value, columnType)
	if err != nil {
		t.Fatalf("normalize expectation: %v", err)
	}
	return normalized
}

func TestGoldenNullNeverEqualsARenderedValue(t *testing.T) {
	cases := []struct {
		name     string
		actual   any
		expected any
		wantErr  bool
	}{
		{"expected NULL vs actual empty JSON", normalizeGoldenValue([]byte("{}"), "jsonb"), mustExpected(t, NullValue, "jsonb"), true},
		{"expected empty JSON vs actual NULL", normalizeGoldenValue(nil, "jsonb"), mustExpected(t, "{}", "jsonb"), true},
		{"expected empty string vs actual NULL", normalizeGoldenValue(nil, "text"), mustExpected(t, "", "text"), true},
		{"expected zero vs actual NULL", normalizeGoldenValue(nil, "bigint"), mustExpected(t, int64(0), "bigint"), true},
		{"expected NULL vs actual zero", normalizeGoldenValue(int64(0), "bigint"), mustExpected(t, NullValue, "bigint"), true},
		{"matching text", normalizeGoldenValue("x", "text"), mustExpected(t, "x", "text"), false},
		{"matching NULL", normalizeGoldenValue(nil, "text"), mustExpected(t, NullValue, "text"), false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := goldenCompare(testCase.actual, testCase.expected)
			if (err != nil) != testCase.wantErr {
				t.Fatalf("goldenCompare() error = %v; wantErr=%v", err, testCase.wantErr)
			}
		})
	}
}

func TestGoldenJSONIgnoresKeyOrderButNotValues(t *testing.T) {
	actual := normalizeGoldenValue([]byte(`{"a":1,"b":2}`), "jsonb")
	if err := goldenCompare(actual, mustExpected(t, `{"b":2,"a":1}`, "jsonb")); err != nil {
		t.Fatalf("key-order-only difference failed: %v", err)
	}
	if err := goldenCompare(actual, mustExpected(t, `{"a":1,"b":3}`, "jsonb")); err == nil {
		t.Fatal("jsonb value mismatch passed")
	}
	if err := goldenCompare(actual, mustExpected(t, `{"a":1}`, "jsonb")); err == nil {
		t.Fatal("jsonb missing-key mismatch passed")
	}
}

func TestGoldenTimestampsAreNanosecondExact(t *testing.T) {
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	actual := normalizeGoldenValue(base, "timestamptz")
	if err := goldenCompare(actual, mustExpected(t, base, "timestamptz")); err != nil {
		t.Fatalf("identical timestamp failed: %v", err)
	}
	if err := goldenCompare(actual, mustExpected(t, base.Add(time.Nanosecond), "timestamptz")); err == nil {
		t.Fatal("nanosecond difference passed")
	}
	if err := goldenCompare(actual, mustExpected(t, base.Add(time.Second), "timestamptz")); err == nil {
		t.Fatal("second difference passed")
	}
}

func TestGoldenScalarMismatchesFail(t *testing.T) {
	cases := []struct {
		actual   any
		expected any
		kind     string
	}{
		{normalizeGoldenValue([]byte{1, 2}, "bytea"), mustExpected(t, []byte{1, 3}, "bytea"), "bytea"},
		{normalizeGoldenValue(int64(1), "bigint"), mustExpected(t, int64(2), "bigint"), "bigint"},
		{normalizeGoldenValue(true, "boolean"), mustExpected(t, false, "boolean"), "boolean"},
		{normalizeGoldenValue("a", "text"), mustExpected(t, "b", "text"), "text"},
	}
	for _, testCase := range cases {
		if err := goldenCompare(testCase.actual, testCase.expected); err == nil {
			t.Fatalf("%s mismatch passed", testCase.kind)
		}
	}
}

func TestGoldenColumnSetValidation(t *testing.T) {
	declared := []string{"a", "b"}
	if err := validateGoldenColumns(declared, map[string]any{"a": 1, "b": 2}); err != nil {
		t.Fatalf("valid set failed: %v", err)
	}
	if err := validateGoldenColumns(declared, map[string]any{"a": 1, "b": 2, "c": 3}); err == nil {
		t.Fatal("undeclared column passed")
	}
	if err := validateGoldenColumns(declared, map[string]any{"a": 1}); err == nil {
		t.Fatal("omitted column passed")
	}
	if err := validateGoldenColumns([]string{"a", "a"}, map[string]any{"a": 1}); err == nil {
		t.Fatal("duplicate declared column passed")
	}
}

func TestGoldenRegistrationRequiresASuccessfulComparison(t *testing.T) {
	savedTables := goldenAsserted
	goldenAsserted = map[string]string{}
	defer func() { goldenAsserted = savedTables }()
	types := map[string]string{"a": "text"}
	if err := applyGoldenRow("t", map[string]any{"a": normalizeGoldenValue("x", "text")}, map[string]any{"a": "y"}, types); err == nil {
		t.Fatal("mismatched row passed")
	}
	if _, ok := goldenAsserted["t"]; ok {
		t.Fatal("failed comparison registered the table")
	}
	if err := applyGoldenRow("t", map[string]any{"a": normalizeGoldenValue("x", "text")}, map[string]any{"a": "x"}, types); err != nil {
		t.Fatalf("matching row failed: %v", err)
	}
	if _, ok := goldenAsserted["t"]; !ok {
		t.Fatal("successful comparison did not register the table")
	}
}

func TestGoldenEmptyRowFails(t *testing.T) {
	types := map[string]string{"a": "text"}
	if err := applyGoldenRow("t", map[string]any{}, map[string]any{"a": "x"}, types); err == nil {
		t.Fatal("empty actual row passed")
	}
}

func TestUnitStateMatrixRenderAndCompare(t *testing.T) {
	states := map[string][]string{
		"task.start":     {"tasks", "audit"},
		"ingress.replay": nil,
	}
	rendered := RenderUnitStateMatrix(states)
	want := "| unit×state | changed tables |\n|---|---|\n| `ingress.replay` | none |\n| `task.start` | audit, tasks |\n"
	if rendered != want {
		t.Fatalf("rendered matrix = %q; want %q", rendered, want)
	}
	path := filepath.Join(t.TempDir(), "README.md")
	body := "before\n" + UnitStateMatrixBegin + "\n" + rendered + UnitStateMatrixEnd + "\nafter\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if err := unitStateMatrixError(path, states); err != nil {
		t.Fatalf("matching README rejected: %v", err)
	}
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if err := unitStateMatrixError(path, states); err == nil {
		t.Fatal("README without markers passed")
	}
}

func TestGoldenJSONNumbersAreExact(t *testing.T) {
	actual := normalizeGoldenValue(`{"n":9007199254740993,"f":0.5}`, "jsonb")
	if err := goldenCompare(actual, mustExpected(t, `{"f":0.5,"n":9007199254740993}`, "jsonb")); err != nil {
		t.Fatalf("exact large integer rejected: %v", err)
	}
	if err := goldenCompare(actual, mustExpected(t, `{"f":0.5,"n":9007199254740992}`, "jsonb")); err == nil {
		t.Fatal("integer differing beyond float64 precision passed")
	}
	if err := goldenCompare(normalizeGoldenValue(`{"n":1}`, "jsonb"), mustExpected(t, `{"n":1.0}`, "jsonb")); err == nil {
		t.Fatal("1 and 1.0 compared equal")
	}
	if err := goldenCompare(normalizeGoldenValue([]byte(`{"a":[1,2]}`), "jsonb"), mustExpected(t, `{"a":[1,2]}`, "jsonb")); err != nil {
		t.Fatalf("byte-slice document rejected: %v", err)
	}
}
