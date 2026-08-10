package dindregistry

import (
	"strconv"
	"strings"
	"testing"
)

// TestRestartPolicyIsCapped pins the decision behind finding 12 of the security audit: the
// registry exits when its upstream is unreachable, so an uncapped policy leaves a container
// restarting forever, outliving every sandbox that asked for it.
func TestRestartPolicyIsCapped(t *testing.T) {
	attempts, found := strings.CutPrefix(restartPolicy, "on-failure:")
	if !found {
		t.Fatalf("restartPolicy = %q, want an on-failure policy with a retry cap", restartPolicy)
	}
	count, err := strconv.Atoi(attempts)
	if err != nil || count <= 0 {
		t.Fatalf("restartPolicy = %q, want a positive retry cap", restartPolicy)
	}
}

func TestReadinessProbeWindowsAreOrdered(t *testing.T) {
	if readyStable >= readyTimeout {
		t.Errorf("readyStable %s must fit inside readyTimeout %s, or the probe can never succeed",
			readyStable, readyTimeout)
	}
	if readyPoll >= readyStable {
		t.Errorf("readyPoll %s must be shorter than readyStable %s, or the probe samples once",
			readyPoll, readyStable)
	}
}
