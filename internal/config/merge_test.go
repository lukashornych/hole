package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func doc(t *testing.T, raw string) Document {
	t.Helper()
	var out Document
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("invalid test document: %v", err)
	}
	return out
}

func TestMergeSemantics(t *testing.T) {
	tests := []struct {
		name    string
		global  string
		project string
		want    string
	}{
		{
			name:    "scalars: project wins",
			global:  `{"container":{"memoryLimit":"2g"}}`,
			project: `{"container":{"memoryLimit":"8g"}}`,
			want:    `{"container":{"memoryLimit":"8g"}}`,
		},
		{
			name:    "objects deep-merge",
			global:  `{"container":{"memoryLimit":"2g","docker":true}}`,
			project: `{"container":{"memoryLimit":"8g"}}`,
			want:    `{"container":{"memoryLimit":"8g","docker":true}}`,
		},
		{
			name:    "arrays concatenate global first",
			global:  `{"dependencies":["make"]}`,
			project: `{"dependencies":["gcc"]}`,
			want:    `{"dependencies":["make","gcc"]}`,
		},
		{
			name:    "arrays deduplicate preserving insertion order",
			global:  `{"dependencies":["make","gcc"]}`,
			project: `{"dependencies":["gcc","cmake"]}`,
			want:    `{"dependencies":["make","gcc","cmake"]}`,
		},
		{
			name:    "arrays of objects deduplicate by value",
			global:  `{"hooks":{"prestart":[{"script":"a.sh"}]}}`,
			project: `{"hooks":{"prestart":[{"script":"a.sh"},{"script":"b.sh"}]}}`,
			want:    `{"hooks":{"prestart":[{"script":"a.sh"},{"script":"b.sh"}]}}`,
		},
		{
			name:    "maps merge per key",
			global:  `{"files":{"include":{"~/.npmrc":"~/.npmrc"}}}`,
			project: `{"files":{"include":{"~/.gitconfig":"~/.gitconfig"}}}`,
			want:    `{"files":{"include":{"~/.npmrc":"~/.npmrc","~/.gitconfig":"~/.gitconfig"}}}`,
		},
		{
			name:    "type mismatch: project wins",
			global:  `{"libraries":{"/a":"/a"}}`,
			project: `{"libraries":{"/a":{"path":"/a","readwrite":true}}}`,
			want:    `{"libraries":{"/a":{"path":"/a","readwrite":true}}}`,
		},
		{
			name:    "empty project keeps global",
			global:  `{"dependencies":["make"]}`,
			project: `{}`,
			want:    `{"dependencies":["make"]}`,
		},
		{
			// An empty array contributes nothing, so the lower-precedence entries survive.
			name:    "explicit empty array overrides nothing away",
			global:  `{"network":{"allow":["example.com"]}}`,
			project: `{"network":{"allow":[]}}`,
			want:    `{"network":{"allow":["example.com"]}}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := Merge(doc(t, test.global), doc(t, test.project))
			want := doc(t, test.want)
			if !reflect.DeepEqual(map[string]any(got), map[string]any(want)) {
				gotJSON, _ := json.Marshal(got)
				t.Errorf("Merge() = %s, want %s", gotJSON, test.want)
			}
		})
	}
}

func TestMergeNilDocuments(t *testing.T) {
	if got := Merge(nil, nil); len(got) != 0 {
		t.Errorf("merging two missing files should yield an empty document, got %v", got)
	}
	got := Merge(nil, doc(t, `{"dependencies":["make"]}`))
	if !reflect.DeepEqual(got["dependencies"], []any{"make"}) {
		t.Errorf("project-only merge lost data: %v", got)
	}
}

func TestMergeDoesNotMutateInputs(t *testing.T) {
	global := doc(t, `{"dependencies":["make"],"container":{"docker":true}}`)
	project := doc(t, `{"dependencies":["gcc"]}`)
	_ = Merge(global, project)

	if deps := global["dependencies"].([]any); len(deps) != 1 {
		t.Errorf("global document was mutated: %v", deps)
	}
	if deps := project["dependencies"].([]any); len(deps) != 1 {
		t.Errorf("project document was mutated: %v", deps)
	}
}

func TestDecodeLibraryForms(t *testing.T) {
	settings, err := Decode(doc(t, `{
	  "libraries": {
	    "/host/a": "/container/a",
	    "/host/b": {"path": "/container/b", "readwrite": true}
	  }
	}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if lib := settings.Libraries["/host/a"]; lib.Path != "/container/a" || lib.ReadWrite {
		t.Errorf("string library form decoded as %+v, want read-only /container/a", lib)
	}
	if lib := settings.Libraries["/host/b"]; lib.Path != "/container/b" || !lib.ReadWrite {
		t.Errorf("object library form decoded as %+v", lib)
	}
}

func TestDecodeNetworkAllow(t *testing.T) {
	settings, err := Decode(doc(t, `{"network":{"allow":["api.github.com","db.example.com:5432"],"subnetPool":"10.222.0.0/16"}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !reflect.DeepEqual(settings.Network.Allow, []string{"api.github.com", "db.example.com:5432"}) {
		t.Errorf("allow = %v", settings.Network.Allow)
	}
	if settings.Network.SubnetPool != "10.222.0.0/16" {
		t.Errorf("subnetPool = %q", settings.Network.SubnetPool)
	}
}

func TestDecodeSetupHookIsAnArray(t *testing.T) {
	settings, err := Decode(doc(t, `{"hooks":{"setup":[{"script":"a.sh"},{"script":".hole/setup.d/*.sh"}]}}`))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(settings.Hooks.Setup) != 2 || settings.Hooks.Setup[1].Script != ".hole/setup.d/*.sh" {
		t.Errorf("setup hooks = %+v", settings.Hooks.Setup)
	}
}

func TestSortedKeysIsDeterministic(t *testing.T) {
	input := map[string]string{"b": "", "a": "", "c": ""}
	want := []string{"a", "b", "c"}
	for i := 0; i < 10; i++ {
		if got := SortedKeys(input); !reflect.DeepEqual(got, want) {
			t.Fatalf("SortedKeys = %v, want %v", got, want)
		}
	}
}
