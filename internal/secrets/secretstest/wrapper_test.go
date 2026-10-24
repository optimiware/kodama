package secretstest_test

import (
	"testing"

	"github.com/optimiware/kodama/internal/secrets/secretstest"
)

// TestUnitMemorySecretsWrapperContract runs the shared key-wrapper contract on
// the ephemeral in-memory wrapper.
func TestUnitMemorySecretsWrapperContract(t *testing.T) {
	secretstest.WrapperContract(t, secretstest.OpenEphemeral)
}

// TestKeyfileWrapperContract runs the same contract on the development keyfile
// provider. It needs only temporary files, so it runs in the unit layer too.
func TestKeyfileWrapperContract(t *testing.T) {
	secretstest.WrapperContract(t, secretstest.OpenKeyfile)
}
