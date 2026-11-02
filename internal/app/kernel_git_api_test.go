package app

import (
	"path/filepath"
	"testing"
)

// The published document is the union of the core assembly's routes and the
// kernel assembly's Git routes (api/kernel-git.yaml, T-170): the repository
// resource and the Git credential exchange are served by `kodama server
// --assembly kernel`, which hosts Git; the core assembly composed here does not
// serve them and answers 404, never a bypass. The core compositions below verify
// the rest of the document.

var kernelGitOperationKeys map[string]bool

// isKernelGitOperation reports whether an aggregate operation key ("METHOD
// /path") belongs to the kernel assembly's Git fragment.
func isKernelGitOperation(key string) bool {
	if kernelGitOperationKeys == nil {
		kernelGitOperationKeys = map[string]bool{}
		for _, op := range loadOperations(nopTB{}, filepath.Join("..", "..", "api", "kernel-git.yaml")) {
			kernelGitOperationKeys[op.Method+" "+op.Path] = true
		}
	}
	return kernelGitOperationKeys[key]
}

// nopTB lets the package-level cache read the fragment without a test handle;
// a read failure leaves the set empty and the callers' own assertions fail.
type nopTB struct{ testing.TB }

func (nopTB) Helper()                           {}
func (nopTB) Fatal(...any)                      {}
func (nopTB) Fatalf(format string, args ...any) {}
