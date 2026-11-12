//go:build e2e

package harness

import "testing"

// TestHarnessAdmittedSnapshotAcceptsAdmittedRun proves the AC2 assertion holds
// for a run that kept the admitted actions and original command.
func TestHarnessAdmittedSnapshotAcceptsAdmittedRun(t *testing.T) {
	admitted := []string{"ac02-first", "ac02-second"}
	observed := []string{"ac02-first", "ac02-second"}
	stdout := []byte("ac02-second-original\n")
	if err := VerifyAdmittedSnapshot(admitted, observed, stdout, "ac02-second-original", "ac02-second-replaced"); err != nil {
		t.Fatalf("admitted run was refused: %v", err)
	}
}

// TestHarnessAdmittedSnapshotDetectsReread is the stubbed immutable-snapshot
// regression: a Shell that re-reads the stored definition at the later action's
// activation produces the post-edit name and command. The assertion must fail.
func TestHarnessAdmittedSnapshotDetectsReread(t *testing.T) {
	admitted := []string{"ac02-first", "ac02-second"}
	for _, regression := range []struct {
		name     string
		observed []string
		stdout   string
	}{
		{
			name:     "action name re-read",
			observed: []string{"ac02-first", "ac02-changed"},
			stdout:   "ac02-second-original\n",
		},
		{
			name:     "command re-read",
			observed: []string{"ac02-first", "ac02-second"},
			stdout:   "ac02-second-replaced\n",
		},
		{
			name:     "both commands",
			observed: []string{"ac02-first", "ac02-second"},
			stdout:   "ac02-second-original\nac02-second-replaced\n",
		},
		{
			name:     "extra action re-read",
			observed: []string{"ac02-first", "ac02-second", "ac02-changed"},
			stdout:   "ac02-second-original\n",
		},
	} {
		t.Run(regression.name, func(t *testing.T) {
			if err := VerifyAdmittedSnapshot(admitted, regression.observed, []byte(regression.stdout), "ac02-second-original", "ac02-second-replaced"); err == nil {
				t.Fatal("a re-read execution was accepted as the admitted snapshot")
			}
		})
	}
	if err := VerifyAdmittedSnapshot(nil, nil, nil, "a", "b"); err == nil {
		t.Fatal("an empty admitted action set was accepted")
	}
	if err := VerifyAdmittedSnapshot(admitted, admitted, []byte("original"), "same", "same"); err == nil {
		t.Fatal("non-distinct markers were accepted")
	}
}
