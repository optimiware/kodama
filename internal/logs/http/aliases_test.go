package logshttp_test

import (
	"testing"
	"time"

	"github.com/optimiware/kodama/internal/logs/logstest/kubefake"
)

type kubefakeAPI = kubefake.API

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	waitClosedFor(t, ch, 10*time.Second, what)
}

func waitClosedFor(t *testing.T, ch <-chan struct{}, d time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatalf("%s was not closed within %s", what, d)
	}
}
