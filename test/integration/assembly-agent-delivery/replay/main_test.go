package replay_test

import (
	"os"
	"testing"

	kit "github.com/optimiware/kodama/test/e2e/m2/kit"
)

// The delivery replay scenarios are the longest of the checkpoint (kind pods, a
// 60 s preview TTL), so they are a package of their own: packages run in
// parallel with a timeout each.
func TestMain(m *testing.M) { os.Exit(kit.Main(m)) }
