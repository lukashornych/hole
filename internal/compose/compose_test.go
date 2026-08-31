package compose

import (
	"strings"
	"testing"
)

func TestMarshalEscapesInterpolation(t *testing.T) {
	file := &File{
		Services: map[string]*Service{
			"agent": {
				Image:       "hole-sandbox/agent-demo:abc",
				Command:     []string{"/home/dev/.nvm/versions/node/v22/bin/node", "--flag=$VALUE"},
				Environment: []string{"TOKEN=${SECRET}"},
				Volumes:     []string{"/host/$dir:/container/$dir"},
				Build:       &Build{Context: "/ctx", Args: map[string]string{"EXTRA_PACKAGES": "a$b"}},
			},
		},
	}
	data, err := Marshal(file)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out := string(data)

	// Compose interpolates $VAR and ${VAR} in the file it reads. Everything Hole generates
	// is already final, so each `$` must reach the container as a literal.
	for _, want := range []string{"--flag=$$VALUE", "TOKEN=$${SECRET}", "/host/$$dir:/container/$$dir", "a$$b"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in generated compose file:\n%s", want, out)
		}
	}
	if strings.Contains(out, "--flag=$VALUE") {
		t.Error("an unescaped $ survived into the compose file")
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	build := func() *File {
		return &File{
			Services: map[string]*Service{
				"gateway": {Image: "gw", Labels: map[string]string{"b": "2", "a": "1", "c": "3"}},
				"agent":   {Image: "ag", Sysctls: map[string]string{"z": "1", "y": "2"}},
			},
			Networks: map[string]*Network{"internet": {External: true}, "sandbox": {External: true}},
		}
	}
	first, err := Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		again, err := Marshal(build())
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("Marshal is not deterministic:\n%s\n---\n%s", first, again)
		}
	}
}

func TestMarshalOmitsEmptyFields(t *testing.T) {
	data, err := Marshal(&File{Services: map[string]*Service{"agent": {Image: "ag"}}})
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	for _, unwanted := range []string{"privileged", "mem_limit", "depends_on", "healthcheck", "cap_add", "volumes:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("unset field %q was emitted:\n%s", unwanted, out)
		}
	}
}

func TestMarshalServiceNetworkForms(t *testing.T) {
	data, err := Marshal(&File{
		Services: map[string]*Service{
			"gateway": {Networks: map[string]*ServiceNetwork{
				"sandbox":  {IPv4Address: "10.222.0.53"},
				"internet": {},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, "ipv4_address: 10.222.0.53") {
		t.Errorf("fixed address missing:\n%s", out)
	}
	if !strings.Contains(out, "internet: {}") {
		t.Errorf("address-less network attachment should marshal as an empty mapping:\n%s", out)
	}
}
