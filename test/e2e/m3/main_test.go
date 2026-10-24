package m3_test

import (
	"os"
	"testing"

	m2 "github.com/optimiware/kodama/test/e2e/m2/kit"
)

func TestMain(m *testing.M) { os.Exit(m2.Main(m)) }
