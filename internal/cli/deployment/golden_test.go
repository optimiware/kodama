package deployment_test

import (
	"os"
	"path/filepath"
	"testing"
)

// goldenCase pins the deterministic JSON a script or reviewer receives:
// sorted keys, the server's members preserved, errors as the envelope on
// stderr with the server's request ID.
func TestJSONFixtures(t *testing.T) {
	f := newFactory()
	c := newCLI(t, f)
	f.dropNext = false
	cases := []struct {
		file   string
		stream func(result) string
		run    func() result
	}{
		{"get.json", func(r result) string { return r.out }, func() result { return c.run("get", depA, "--output", "json") }},
		{"list.json", func(r result) string { return r.out }, func() result { return c.run("list", "--status", "running", "--output", "json") }},
		{"create_receipt.json", func(r result) string { return r.out }, func() result { return c.run(createArgs("--output", "json")...) }},
		{"create_replay.json", func(r result) string { return r.out }, func() result { return c.run(createArgs("--output", "json")...) }},
		{"create_observed.json", func(r result) string { return r.out }, func() result {
			return c.run(withFlag(createArgs("--output", "json"), "idempotency-key", "deploy-other")...)
		}},
		{"error_unavailable_after_commit.json", func(r result) string { return r.err }, func() result {
			f2 := newFactory()
			f2.unavailable()
			return newCLI(t, f2).run("cleanup", depA, "--if-version", "3", "--output", "json")
		}},
		{"cleanup.json", func(r result) string { return r.out }, func() result { return c.run("cleanup", depA, "--if-version", "3", "--output", "json") }},
		{"error_not_found.json", func(r result) string { return r.err }, func() result { return c.run("get", depC, "--output", "json") }},
		{"error_stale_version.json", func(r result) string { return r.err }, func() result {
			return c.run("cleanup", depA, "--if-version", "3", "--output", "json")
		}},
		{"error_idempotency_conflict.json", func(r result) string { return r.err }, func() result {
			return c.run(withFlag(createArgs("--output", "json"), "image-digest", digest2)...)
		}},
	}
	for _, tc := range cases {
		got := tc.stream(tc.run())
		want, err := os.ReadFile(filepath.Join("testdata", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs:\n got %s\nwant %s", tc.file, got, want)
		}
	}
}
