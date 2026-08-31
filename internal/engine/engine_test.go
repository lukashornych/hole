package engine

import "testing"

// The cases named "podman" are what the parity job caught: podman answers a `reference=` filter
// at image granularity and qualifies names with their registry, so an unrelated tag of a shared
// image ID reaches callers that go on to remove what they are given.
func TestMatchesReference(t *testing.T) {
	tests := []struct {
		name    string
		pattern string
		listed  string
		want    bool
	}{
		{"exact repository, any tag", "hole-sandbox/agent-foo", "hole-sandbox/agent-foo:abc123", true},
		{"wildcard repository", "hole-sandbox/*", "hole-sandbox/agent-foo:abc123", true},
		{"wildcard with a tag", "hole-sandbox/agent-*:latest", "hole-sandbox/agent-foo:latest", true},
		{"wildcard with a different tag", "hole-sandbox/agent-*:latest", "hole-sandbox/agent-foo:old", false},
		{"unrelated repository", "hole-sandbox/*", "myproject/api:latest", false},
		// Every repository Hole produces, against the pattern `hole uninstall` sweeps with:
		// path.Match's `*` does not cross a slash, so a third segment would silently stop
		// matching. hostenv.sanitizeName strips slashes, which is what keeps that from arising.
		{"uninstall sweeps the gateway", "hole-sandbox/*", "hole-sandbox/gateway:abc123", true},
		{"uninstall sweeps an agent image", "hole-sandbox/*", "hole-sandbox/agent-myproj-1a2b3c4d:abc123", true},
		{"uninstall sweeps the global image", "hole-sandbox/*", "hole-sandbox/agent-global:abc123", true},
		{"legacy proxy images", "hole-sandbox/proxy-*", "hole-sandbox/proxy-myproj:latest", true},
		{"legacy per-project latest tags", "hole-sandbox/agent-*:latest", "hole-sandbox/agent-myproj:latest", true},
		{"podman qualifies the name", "hole-sandbox/*", "localhost/hole-sandbox/agent-foo:abc123", true},
		{"podman qualifies a tagged pattern", "hole-sandbox/agent-*:latest", "localhost/hole-sandbox/agent-foo:latest", true},
		{"podman drags in a shared image ID", "hole-sandbox/agent-*:tagtest", "docker.io/library/alpine:3.19", false},
		{"podman qualifies an unrelated name", "hole-sandbox/*", "docker.io/library/alpine:3.19", false},
		{"dangling reference", "hole-sandbox/*", "<none>:<none>", false},
		{"a prefix is not a match", "hole-sandbox/agent", "hole-sandbox/agent-foo:abc123", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := matchesReference(test.pattern, test.listed); got != test.want {
				t.Errorf("matchesReference(%q, %q) = %v, want %v", test.pattern, test.listed, got, test.want)
			}
		})
	}
}

func TestSplitReference(t *testing.T) {
	tests := []struct {
		reference string
		name      string
		tag       string
	}{
		{"hole-sandbox/agent-foo:abc123", "hole-sandbox/agent-foo", "abc123"},
		{"hole-sandbox/agent-foo", "hole-sandbox/agent-foo", ""},
		// The colon belongs to the registry's port, not to a tag.
		{"localhost:5000/hole-sandbox/agent-foo", "localhost:5000/hole-sandbox/agent-foo", ""},
		{"localhost:5000/hole-sandbox/agent-foo:abc123", "localhost:5000/hole-sandbox/agent-foo", "abc123"},
	}
	for _, test := range tests {
		name, tag := splitReference(test.reference)
		if name != test.name || tag != test.tag {
			t.Errorf("splitReference(%q) = (%q, %q), want (%q, %q)", test.reference, name, tag, test.name, test.tag)
		}
	}
}
