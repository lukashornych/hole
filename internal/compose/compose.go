// Package compose is a typed model of the small Compose subset Hole uses, plus its YAML
// serialisation. Hole generates exactly one compose file per run from this model — the 1.x
// five-file layering is gone.
package compose

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is a generated compose document.
type File struct {
	Services map[string]*Service `yaml:"services,omitempty"`
	Volumes  map[string]*Volume  `yaml:"volumes,omitempty"`
	Networks map[string]*Network `yaml:"networks,omitempty"`
}

// Service is one container definition.
type Service struct {
	Image        string                     `yaml:"image,omitempty"`
	Build        *Build                     `yaml:"build,omitempty"`
	PullPolicy   string                     `yaml:"pull_policy,omitempty"`
	Command      []string                   `yaml:"command,omitempty"`
	Entrypoint   []string                   `yaml:"entrypoint,omitempty"`
	Environment  []string                   `yaml:"environment,omitempty"`
	Volumes      []string                   `yaml:"volumes,omitempty"`
	Networks     map[string]*ServiceNetwork `yaml:"networks,omitempty"`
	DNS          []string                   `yaml:"dns,omitempty"`
	ExtraHosts   []string                   `yaml:"extra_hosts,omitempty"`
	DependsOn    map[string]Dependency      `yaml:"depends_on,omitempty"`
	Healthcheck  *Healthcheck               `yaml:"healthcheck,omitempty"`
	Labels       map[string]string          `yaml:"labels,omitempty"`
	CapAdd       []string                   `yaml:"cap_add,omitempty"`
	Sysctls      map[string]string          `yaml:"sysctls,omitempty"`
	Privileged   bool                       `yaml:"privileged,omitempty"`
	User         string                     `yaml:"user,omitempty"`
	StdinOpen    bool                       `yaml:"stdin_open,omitempty"`
	TTY          bool                       `yaml:"tty,omitempty"`
	WorkingDir   string                     `yaml:"working_dir,omitempty"`
	MemLimit     string                     `yaml:"mem_limit,omitempty"`
	MemswapLimit string                     `yaml:"memswap_limit,omitempty"`
	Restart      string                     `yaml:"restart,omitempty"`
}

// Build describes an image build.
type Build struct {
	Context    string            `yaml:"context"`
	Dockerfile string            `yaml:"dockerfile,omitempty"`
	Args       map[string]string `yaml:"args,omitempty"`
}

// ServiceNetwork attaches a service to a network, optionally with a fixed address.
type ServiceNetwork struct {
	IPv4Address string `yaml:"ipv4_address,omitempty"`
}

// Dependency is a depends_on entry.
type Dependency struct {
	Condition string `yaml:"condition"`
}

// Healthcheck is a service healthcheck.
type Healthcheck struct {
	Test     []string `yaml:"test"`
	Interval string   `yaml:"interval,omitempty"`
	Timeout  string   `yaml:"timeout,omitempty"`
	Retries  int      `yaml:"retries,omitempty"`
}

// Volume is a top-level volume declaration.
type Volume struct {
	External bool   `yaml:"external,omitempty"`
	Name     string `yaml:"name,omitempty"`
}

// Network is a top-level network declaration.
type Network struct {
	External bool   `yaml:"external,omitempty"`
	Name     string `yaml:"name,omitempty"`
	Internal bool   `yaml:"internal,omitempty"`
	Driver   string `yaml:"driver,omitempty"`
}

// ServiceHealthy is the depends_on condition that makes compose wait for a healthcheck.
var ServiceHealthy = Dependency{Condition: "service_healthy"}

// Marshal renders the compose file.
//
// Every string value is escaped so Compose performs no variable interpolation on it:
// generated values (project paths, environment values, agent commands) are already final,
// and a `$` surviving into the file would otherwise be substituted or emit a warning. A
// literal `$` therefore has to be written as `$$` — which is also how the DinD entrypoint
// wrapper gets its `"$@"`.
func Marshal(file *File) ([]byte, error) {
	escapeInterpolation(reflect.ValueOf(file))
	data, err := yaml.Marshal(file)
	if err != nil {
		return nil, fmt.Errorf("marshal compose file: %w", err)
	}
	return data, nil
}

func escapeInterpolation(value reflect.Value) {
	switch value.Kind() {
	case reflect.Ptr, reflect.Interface:
		if !value.IsNil() {
			escapeInterpolation(value.Elem())
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			escapeInterpolation(value.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			escapeInterpolation(value.Index(i))
		}
	case reflect.Map:
		for _, key := range value.MapKeys() {
			entry := value.MapIndex(key)
			if entry.Kind() == reflect.String {
				value.SetMapIndex(key, reflect.ValueOf(escapeString(entry.String())))
				continue
			}
			// Struct map values are not addressable, so copy, escape, store back.
			copied := reflect.New(entry.Type()).Elem()
			copied.Set(entry)
			escapeInterpolation(copied)
			value.SetMapIndex(key, copied)
		}
	case reflect.String:
		if value.CanSet() {
			value.SetString(escapeString(value.String()))
		}
	}
}

func escapeString(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}
