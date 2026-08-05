package version

import "testing"

func TestGreaterThan(t *testing.T) {
	tests := []struct {
		left, right string
		want        bool
	}{
		{"1.2.3", "1.2.2", true},
		{"1.3.0", "1.2.9", true},
		{"2.0.0", "1.9.9", true},
		{"1.2.3", "1.2.3", false},
		{"1.2.2", "1.2.3", false},
		{"1.2", "1.2.0", false},
		{"1.2.1", "1.2", true},
		{"1.10.0", "1.9.0", true},
	}
	for _, test := range tests {
		if got := GreaterThan(test.left, test.right); got != test.want {
			t.Errorf("GreaterThan(%q, %q) = %v, want %v", test.left, test.right, got, test.want)
		}
	}
}

func TestIsDevelopment(t *testing.T) {
	// An unstamped build must be recognised as a development one: it skips update checks and
	// refuses to self-update.
	if Version == DevelopmentVersion && !IsDevelopment() {
		t.Error("an unstamped build is not reported as a development build")
	}
	if Version != DevelopmentVersion && IsDevelopment() {
		t.Error("a stamped build is reported as a development build")
	}
}
