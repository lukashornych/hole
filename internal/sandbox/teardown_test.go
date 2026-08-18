package sandbox

import (
	"slices"
	"strings"
	"testing"

	"github.com/lukashornych/hole/v2/internal/hostenv"
	"github.com/lukashornych/hole/v2/internal/state"
)

// newTestStore gives each case its own registry directory.
func newTestStore(t *testing.T) (hostenv.Host, *state.Store) {
	t.Helper()
	host := hostenv.Host{Home: t.TempDir()}
	store, err := state.NewStore(host.InstancesDir())
	if err != nil {
		t.Fatal(err)
	}
	return host, store
}

// register writes an instance and, when live, takes the liveness lock that `Abandoned` reads.
// A WatchdogPID of -1 is never alive, so the lock alone decides.
func register(t *testing.T, store *state.Store, name string, live bool) *state.Instance {
	t.Helper()
	instance := &state.Instance{InstanceName: name, WatchdogPID: -1}
	if err := store.Write(instance); err != nil {
		t.Fatal(err)
	}
	if live {
		release, err := store.HoldLiveness(name)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	return instance
}

func TestIsLastInstance(t *testing.T) {
	const self = "hole-sandbox-demo-1a2b3c4d-self00"

	tests := map[string]struct {
		others map[string]bool // instance name -> still live
		want   bool
	}{
		// The exiting sandbox is still registered while its hooks run, so "only self" is the
		// normal last-instance case rather than an empty registry.
		"only self":                {others: nil, want: true},
		"another live sandbox":     {others: map[string]bool{"hole-sandbox-demo-1a2b3c4d-other0": true}, want: false},
		"only abandoned leftovers": {others: map[string]bool{"hole-sandbox-demo-1a2b3c4d-dead00": false}, want: true},
		"live and abandoned mixed": {others: map[string]bool{"hole-sandbox-demo-1a2b3c4d-dead00": false, "hole-sandbox-demo-1a2b3c4d-other0": true}, want: false},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, store := newTestStore(t)
			instance := register(t, store, self, true)
			for other, live := range test.others {
				register(t, store, other, live)
			}
			if got := isLastInstance(store, instance); got != test.want {
				t.Errorf("isLastInstance = %v, want %v", got, test.want)
			}
		})
	}
}

// An empty registry means the state file was already removed — a second teardown pass, or a
// hook run for an instance GC deregistered. Nothing is running, so the answer is still "last".
func TestIsLastInstanceWithEmptyRegistry(t *testing.T) {
	_, store := newTestStore(t)
	instance := &state.Instance{InstanceName: "hole-sandbox-demo-1a2b3c4d-gone00", WatchdogPID: -1}
	if !isLastInstance(store, instance) {
		t.Error("an empty registry should report the instance as the last one")
	}
}

func TestCleanupHookEnvironmentExportsIsLastInstance(t *testing.T) {
	host, store := newTestStore(t)
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-self00", true)

	if got := valueOf(t, cleanupHookEnvironment(host, instance, true), "HOLE_IS_LAST_INSTANCE"); got != "true" {
		t.Errorf("HOLE_IS_LAST_INSTANCE = %q, want \"true\"", got)
	}
	if got := valueOf(t, cleanupHookEnvironment(host, instance, false), "HOLE_IS_LAST_INSTANCE"); got != "false" {
		t.Errorf("HOLE_IS_LAST_INSTANCE = %q, want \"false\"", got)
	}
}

// setupHost shares hookEnvironment, where the variable would be meaningless: no sandbox has
// exited yet. Leaking it there would let a hook act on an answer that is always "false".
func TestSetupHostEnvironmentOmitsIsLastInstance(t *testing.T) {
	host, store := newTestStore(t)
	instance := register(t, store, "hole-sandbox-demo-1a2b3c4d-self00", true)

	for _, entry := range hookEnvironment(host, instance) {
		if strings.HasPrefix(entry, "HOLE_IS_LAST_INSTANCE=") {
			t.Fatal("hookEnvironment must not carry HOLE_IS_LAST_INSTANCE into setupHost")
		}
	}
	// The teardown environment is the base plus that one variable, so the base keeps its entries.
	base, cleanup := hookEnvironment(host, instance), cleanupHookEnvironment(host, instance, true)
	for _, entry := range base {
		if !slices.Contains(cleanup, entry) {
			t.Errorf("cleanupHookEnvironment dropped %q from the shared hook environment", entry)
		}
	}
}

func valueOf(t *testing.T, env []string, key string) string {
	t.Helper()
	for _, entry := range env {
		if name, value, found := strings.Cut(entry, "="); found && name == key {
			return value
		}
	}
	t.Fatalf("%s is not exported to the hook", key)
	return ""
}
