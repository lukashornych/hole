package sandbox

import "testing"

func TestSelectAdoptable(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		target     string
		want       string
	}{
		{
			name:       "no candidates",
			candidates: nil,
			target:     "hole-sandbox/agent-proj-11111111:abc",
			want:       "",
		},
		{
			name:       "only the target itself",
			candidates: []string{"hole-sandbox/agent-proj-11111111:abc"},
			target:     "hole-sandbox/agent-proj-11111111:abc",
			want:       "",
		},
		{
			name:       "only the target itself, podman registry prefix",
			candidates: []string{"localhost/hole-sandbox/agent-proj-11111111:abc"},
			target:     "hole-sandbox/agent-proj-11111111:abc",
			want:       "",
		},
		{
			name:       "one other repository",
			candidates: []string{"hole-sandbox/agent-proj-22222222:abc"},
			target:     "hole-sandbox/agent-proj-11111111:abc",
			want:       "hole-sandbox/agent-proj-22222222:abc",
		},
		{
			name: "several others pick the first sorted",
			candidates: []string{
				"hole-sandbox/agent-proj-33333333:abc",
				"hole-sandbox/agent-proj-11111111:abc",
				"hole-sandbox/agent-proj-22222222:abc",
			},
			target: "hole-sandbox/agent-proj-11111111:abc",
			want:   "hole-sandbox/agent-proj-22222222:abc",
		},
		{
			name:       "global candidate adopted into a project repository",
			candidates: []string{"hole-sandbox/agent-global:abc"},
			target:     "hole-sandbox/agent-proj-11111111:abc",
			want:       "hole-sandbox/agent-global:abc",
		},
		{
			name:       "project candidate adopted into the global repository",
			candidates: []string{"hole-sandbox/agent-proj-11111111:abc"},
			target:     "hole-sandbox/agent-global:abc",
			want:       "hole-sandbox/agent-proj-11111111:abc",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := selectAdoptable(test.candidates, test.target); got != test.want {
				t.Errorf("selectAdoptable() = %q, want %q", got, test.want)
			}
		})
	}
}

// The daemon's listing order is not guaranteed, so the pick must not depend on it.
func TestSelectAdoptableDoesNotMutateCandidates(t *testing.T) {
	candidates := []string{
		"hole-sandbox/agent-proj-33333333:abc",
		"hole-sandbox/agent-proj-22222222:abc",
	}
	selectAdoptable(candidates, "hole-sandbox/agent-proj-11111111:abc")

	if candidates[0] != "hole-sandbox/agent-proj-33333333:abc" {
		t.Errorf("candidates were reordered in place: %v", candidates)
	}
}
