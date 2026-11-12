//go:build e2e

package harness

import (
	"bytes"
	"fmt"
)

// VerifyAdmittedSnapshot encodes the AC2 immutable-admission assertion: an
// execution must keep the action names admitted at start and its later action
// must run the admitted command, never a post-edit replacement. observedActions
// is the execution.inspect action-name sequence; stdout is the later action's
// retained stdout. A Shell that re-read the stored definition at a later
// activation would produce the replaced name and marker, and this check fails.
func VerifyAdmittedSnapshot(admittedActions, observedActions []string, stdout []byte, originalMarker, replacementMarker string) error {
	if len(admittedActions) == 0 {
		return fmt.Errorf("admitted action set is empty")
	}
	if originalMarker == "" || replacementMarker == "" || originalMarker == replacementMarker {
		return fmt.Errorf("admitted-snapshot markers must be distinct and non-empty")
	}
	if len(observedActions) != len(admittedActions) {
		return fmt.Errorf("observed %d action(s); the admitted execution has %d", len(observedActions), len(admittedActions))
	}
	for index := range admittedActions {
		if observedActions[index] != admittedActions[index] {
			return fmt.Errorf("action %d is %q; the admitted snapshot names %q", index, observedActions[index], admittedActions[index])
		}
	}
	if !bytes.Contains(stdout, []byte(originalMarker)) {
		return fmt.Errorf("the admitted action output does not contain its original marker")
	}
	if bytes.Contains(stdout, []byte(replacementMarker)) {
		return fmt.Errorf("the admitted action ran the post-edit replacement marker")
	}
	return nil
}
