package network

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/netip"
	"sort"
)

const (
	// DefaultSubnetPool is the address space Hole allocates sandbox networks from. It is
	// Hole's own pool, so Docker's default pools stay untouched (the 1.x probe-network
	// trick burned a default-pool /16 per network and exhausted it after ~14 sandboxes).
	DefaultSubnetPool = "10.222.0.0/16"

	// allocationBits is the prefix length of each allocated network.
	allocationBits = 24

	// minPoolBits is the smallest pool that can serve a single sandbox: each instance
	// needs two /24s.
	minPoolBits = 23

	// maxCandidates bounds enumeration for very large pools.
	maxCandidates = 65536
)

// Pool is a validated allocation pool.
type Pool struct {
	prefix netip.Prefix
}

// ParsePool validates a pool CIDR. The `/23` floor is enforced here rather than in the
// schema so the error can explain why: a smaller pool passes syntactic validation but can
// never start a sandbox.
func ParsePool(raw string) (Pool, error) {
	value := raw
	if value == "" {
		value = DefaultSubnetPool
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return Pool{}, fmt.Errorf("network.subnetPool '%s' is not a valid IPv4 CIDR: %w", value, err)
	}
	if !prefix.Addr().Is4() {
		return Pool{}, fmt.Errorf("network.subnetPool '%s' must be IPv4", value)
	}
	if prefix.Bits() > minPoolBits {
		return Pool{}, fmt.Errorf(
			"network.subnetPool '%s' is too small: each sandbox needs two /%d networks, so the pool must be /%d or larger",
			value, allocationBits, minPoolBits)
	}
	return Pool{prefix: prefix.Masked()}, nil
}

// String renders the pool CIDR.
func (p Pool) String() string { return p.prefix.String() }

// Capacity is the number of /24 networks the pool holds.
func (p Pool) Capacity() int {
	shift := allocationBits - p.prefix.Bits()
	if shift >= 32 {
		return maxCandidates
	}
	count := 1 << shift
	if count > maxCandidates {
		return maxCandidates
	}
	return count
}

// Candidates enumerates every /24 in the pool, lowest first.
func (p Pool) Candidates() []netip.Prefix {
	count := p.Capacity()
	out := make([]netip.Prefix, 0, count)
	addr := p.prefix.Addr()
	for i := 0; i < count; i++ {
		candidate := netip.PrefixFrom(addr, allocationBits)
		out = append(out, candidate.Masked())
		next, ok := addOffset(addr, 1<<(32-allocationBits))
		if !ok {
			break
		}
		addr = next
	}
	return out
}

// FreeCandidates returns the pool's /24s that do not overlap any used prefix. Overlap —
// not equality — is the test, because an existing supernet (say a Docker network holding
// the whole pool) blocks every candidate inside it.
func (p Pool) FreeCandidates(used []netip.Prefix) []netip.Prefix {
	var free []netip.Prefix
	for _, candidate := range p.Candidates() {
		if overlapsAny(candidate, used) {
			continue
		}
		free = append(free, candidate)
	}
	return free
}

// Allocate picks count non-overlapping /24s from the pool.
//
// attempt 0 takes the lowest free candidates, which keeps single-sandbox subnets
// predictable; later attempts pick at random so concurrent starts that lose the
// create-time race do not all stampede the same next candidate.
func (p Pool) Allocate(used []netip.Prefix, count, attempt int) ([]netip.Prefix, error) {
	free := p.FreeCandidates(used)
	if len(free) < count {
		return nil, fmt.Errorf(
			"subnet pool %s exhausted: %d of %d /%d networks free, %d needed — free some sandboxes or widen network.subnetPool",
			p.String(), len(free), p.Capacity(), allocationBits, count)
	}
	if attempt <= 0 {
		return free[:count], nil
	}

	picked := make([]netip.Prefix, 0, count)
	remaining := append([]netip.Prefix(nil), free...)
	for len(picked) < count {
		idx, err := randomIndex(len(remaining))
		if err != nil {
			return nil, err
		}
		picked = append(picked, remaining[idx])
		remaining = append(remaining[:idx], remaining[idx+1:]...)
	}
	sort.Slice(picked, func(i, j int) bool { return picked[i].Addr().Less(picked[j].Addr()) })
	return picked, nil
}

// GatewayIP is the fixed address the gateway container takes inside a sandbox subnet: the
// subnet base with the last octet set to 53 (it is the sandbox's DNS server).
func GatewayIP(subnet netip.Prefix) (string, error) {
	addr := subnet.Masked().Addr()
	if !addr.Is4() {
		return "", fmt.Errorf("subnet %s is not IPv4", subnet)
	}
	octets := addr.As4()
	octets[3] = 53
	return netip.AddrFrom4(octets).String(), nil
}

func overlapsAny(candidate netip.Prefix, used []netip.Prefix) bool {
	for _, existing := range used {
		if candidate.Overlaps(existing) {
			return true
		}
	}
	return false
}

func addOffset(addr netip.Addr, offset uint32) (netip.Addr, bool) {
	if !addr.Is4() {
		return netip.Addr{}, false
	}
	octets := addr.As4()
	value := uint32(octets[0])<<24 | uint32(octets[1])<<16 | uint32(octets[2])<<8 | uint32(octets[3])
	sum := uint64(value) + uint64(offset)
	if sum > 0xFFFFFFFF {
		return netip.Addr{}, false
	}
	result := uint32(sum)
	return netip.AddrFrom4([4]byte{
		byte(result >> 24), byte(result >> 16), byte(result >> 8), byte(result),
	}), true
}

func randomIndex(n int) (int, error) {
	if n <= 0 {
		return 0, fmt.Errorf("no candidates left")
	}
	value, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, fmt.Errorf("pick random subnet: %w", err)
	}
	return int(value.Int64()), nil
}
