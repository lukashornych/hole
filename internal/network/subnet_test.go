package network

import (
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()
	out := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		prefix, err := netip.ParsePrefix(value)
		if err != nil {
			t.Fatalf("invalid test prefix %q: %v", value, err)
		}
		out = append(out, prefix)
	}
	return out
}

func TestParsePool(t *testing.T) {
	pool, err := ParsePool("")
	if err != nil {
		t.Fatalf("default pool rejected: %v", err)
	}
	if pool.String() != DefaultSubnetPool {
		t.Errorf("default pool = %s, want %s", pool, DefaultSubnetPool)
	}

	if pool, err := ParsePool("10.100.0.0/23"); err != nil {
		t.Errorf("/23 pool rejected: %v", err)
	} else if pool.Capacity() != 2 {
		t.Errorf("/23 capacity = %d, want 2", pool.Capacity())
	}

	// An unaligned base is masked to the containing prefix.
	if pool, err := ParsePool("10.222.5.7/16"); err != nil {
		t.Errorf("unaligned pool rejected: %v", err)
	} else if pool.String() != "10.222.0.0/16" {
		t.Errorf("unaligned pool = %s, want 10.222.0.0/16", pool)
	}
}

func TestParsePoolRejectsTooSmallAndInvalid(t *testing.T) {
	// A /24 pool passes syntactic validation but can never start a sandbox, which needs two.
	for _, value := range []string{"10.222.0.0/24", "10.222.0.0/30", "not-a-cidr", "2001:db8::/32", "10.222.0.0"} {
		if _, err := ParsePool(value); err == nil {
			t.Errorf("invalid pool %q accepted", value)
		}
	}
}

func TestParsePoolErrorExplainsCapacity(t *testing.T) {
	_, err := ParsePool("10.222.0.0/24")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !contains(err.Error(), "/23") {
		t.Errorf("error should name the minimum pool size: %v", err)
	}
}

func TestAllocateFirstFitIsPredictable(t *testing.T) {
	pool, err := ParsePool("10.222.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pool.Allocate(nil, 2, 0)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0].String() != "10.222.0.0/24" || got[1].String() != "10.222.1.0/24" {
		t.Errorf("first-fit allocation = %v, want the two lowest /24s", got)
	}
}

func TestAllocateSkipsOverlappingSubnets(t *testing.T) {
	pool, err := ParsePool("10.222.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	used := prefixes(t, "10.222.0.0/24", "10.222.1.128/25")
	got, err := pool.Allocate(used, 2, 0)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	// 10.222.1.0/24 must be skipped: a nested /25 still overlaps it.
	if got[0].String() != "10.222.2.0/24" || got[1].String() != "10.222.3.0/24" {
		t.Errorf("allocation = %v, want 10.222.2.0/24 and 10.222.3.0/24", got)
	}
}

func TestAllocateRespectsSupernets(t *testing.T) {
	pool, err := ParsePool("10.222.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	// An existing network covering the whole pool blocks every candidate.
	used := prefixes(t, "10.0.0.0/8")
	if _, err := pool.Allocate(used, 2, 0); err == nil {
		t.Error("allocation succeeded despite a supernet covering the pool")
	}
}

func TestAllocateExhaustionErrorNamesCapacity(t *testing.T) {
	pool, err := ParsePool("10.222.0.0/23")
	if err != nil {
		t.Fatal(err)
	}
	used := prefixes(t, "10.222.0.0/24")
	_, err = pool.Allocate(used, 2, 0)
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	// "too small" and "full" must be distinguishable from the message.
	if !contains(err.Error(), "1 of 2") {
		t.Errorf("exhaustion error should state free/total capacity: %v", err)
	}
}

func TestAllocateRetryUsesRandomCandidates(t *testing.T) {
	pool, err := ParsePool("10.222.0.0/16")
	if err != nil {
		t.Fatal(err)
	}
	// With jitter, repeated retry attempts must not always return the same candidates —
	// that is what stops concurrent starts from stampeding one subnet.
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		got, err := pool.Allocate(nil, 2, 1)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		seen[got[0].String()+","+got[1].String()] = true
	}
	if len(seen) < 5 {
		t.Errorf("retry allocation is not jittered: only %d distinct results", len(seen))
	}
}

func TestAllocateReturnsDistinctSubnets(t *testing.T) {
	pool, err := ParsePool("10.222.0.0/23")
	if err != nil {
		t.Fatal(err)
	}
	got, err := pool.Allocate(nil, 2, 1)
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if got[0] == got[1] {
		t.Errorf("allocation returned the same subnet twice: %v", got)
	}
}

func TestGatewayIP(t *testing.T) {
	tests := map[string]string{
		"10.222.0.0/24": "10.222.0.53",
		"10.222.7.0/24": "10.222.7.53",
		"172.19.0.0/16": "172.19.0.53",
	}
	for subnet, want := range tests {
		parsed := prefixes(t, subnet)[0]
		got, err := GatewayIP(parsed)
		if err != nil {
			t.Fatalf("GatewayIP(%s): %v", subnet, err)
		}
		if got != want {
			t.Errorf("GatewayIP(%s) = %s, want %s", subnet, got, want)
		}
	}
}

func TestPoolCapacityIsBounded(t *testing.T) {
	pool, err := ParsePool("10.0.0.0/8")
	if err != nil {
		t.Fatal(err)
	}
	if got := pool.Capacity(); got != maxCandidates {
		t.Errorf("capacity of a /8 pool = %d, want the %d bound", got, maxCandidates)
	}
}
