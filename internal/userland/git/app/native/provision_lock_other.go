//go:build !unix

package native

import (
	"context"
	"fmt"
)

// acquireProvisionLock reports that tenant provisioning needs an advisory
// directory lock this platform does not provide. Provisioning refuses rather
// than publishing without cross-process serialization.
func acquireProvisionLock(ctx context.Context, tenantDirectory string) (func(), error) {
	return nil, fmt.Errorf("tenant provisioning needs a Unix advisory lock, which this platform does not provide")
}
