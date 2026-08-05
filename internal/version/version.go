// Package version reports the running Hole build. Release discovery and self-update live in
// internal/update, which needs the release assets as well.
package version

import (
	"strconv"
	"strings"
)

// DevelopmentVersion is reported by builds without a stamped version, which also skip
// update checks entirely.
const DevelopmentVersion = "development"

// Version is stamped at build time via -ldflags.
var Version = DevelopmentVersion

// IsDevelopment reports whether this is an unstamped build. Such builds skip update checks
// and refuse to self-update.
func IsDevelopment() bool { return Version == DevelopmentVersion }

// GreaterThan compares dotted numeric versions; missing components count as zero.
func GreaterThan(left, right string) bool {
	leftParts := strings.Split(left, ".")
	rightParts := strings.Split(right, ".")
	length := len(leftParts)
	if len(rightParts) > length {
		length = len(rightParts)
	}
	for i := 0; i < length; i++ {
		if component(leftParts, i) > component(rightParts, i) {
			return true
		}
		if component(leftParts, i) < component(rightParts, i) {
			return false
		}
	}
	return false
}

func component(parts []string, index int) int {
	if index >= len(parts) {
		return 0
	}
	value, err := strconv.Atoi(strings.TrimSpace(parts[index]))
	if err != nil {
		return 0
	}
	return value
}
