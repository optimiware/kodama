package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/optimiware/kodama/internal/logs"
	rtjwt "github.com/optimiware/kodama/internal/runtimeauth/jwt"
)

// controllerDiagnosticFiles bounds the diagnostic artifacts (logs and their
// sidecars) the controller may retain for one attempt: the wrapper's pair plus
// the pair of each of the controller's generations.
const controllerDiagnosticFiles = 16

// loadRuntimeKeys reads the runtime signing key and builds the key ring at the
// configured generation. The deployment persists both and never reuses a
// generation for another key (rtjwt.KeyRing).
func loadRuntimeKeys(c RuntimeConfig) (*rtjwt.KeyRing, error) {
	data, err := os.ReadFile(c.SigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("runtime signing key: %w", err)
	}
	key, err := rtjwt.ParsePrivateKeyPEM(data)
	if err != nil {
		return nil, fmt.Errorf("runtime signing key: %w", err)
	}
	ring, err := rtjwt.NewKeyRing(c.SigningGeneration, key)
	if err != nil {
		return nil, fmt.Errorf("runtime signing key: %w", err)
	}
	return ring, nil
}

// noPlacement places no workload: live log reads are refused where the runtime
// supplies no trusted namespace and container (anything but Kubernetes).
type noPlacement struct{}

func (noPlacement) Placement(_ context.Context, _ logs.AttemptInfo) (string, string, error) {
	return "", "", fmt.Errorf("%w: this runtime has no live log placement", logs.ErrNotConfigured)
}

// noLive is the live reader of runtimes that cannot serve live logs. It is
// unreachable behind noPlacement and fails closed if it is ever reached.
type noLive struct{}

func (noLive) Open(context.Context, logs.Target, logs.LiveQuery) (io.ReadCloser, error) {
	return nil, logs.Unavailablef(errors.New("no live reader"), "live log reads are not available on this runtime")
}
