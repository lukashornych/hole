package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/lukashornych/hole/v2/assets"
)

// schemaResourceURL is the identifier the compiled schema is registered under. It is also
// the URL users put in their `$schema` key, so the two must match.
const schemaResourceURL = "https://raw.githubusercontent.com/lukashornych/hole/main/assets/schema/settings.schema.json"

var (
	compiledSchema     *jsonschema.Schema
	compiledSchemaErr  error
	compiledSchemaOnce sync.Once
)

func settingsSchema() (*jsonschema.Schema, error) {
	compiledSchemaOnce.Do(func() {
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(assets.Schema()))
		if err != nil {
			compiledSchemaErr = fmt.Errorf("parse embedded settings schema: %w", err)
			return
		}
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource(schemaResourceURL, doc); err != nil {
			compiledSchemaErr = fmt.Errorf("register embedded settings schema: %w", err)
			return
		}
		compiledSchema, err = compiler.Compile(schemaResourceURL)
		if err != nil {
			compiledSchemaErr = fmt.Errorf("compile embedded settings schema: %w", err)
		}
	})
	return compiledSchema, compiledSchemaErr
}

// ValidationFailure carries every schema violation of one settings file so the caller can
// report them all at once, the way the jv-based bash implementation did.
type ValidationFailure struct {
	Label   string
	Details []string
}

func (e *ValidationFailure) Error() string {
	return fmt.Sprintf("%s is not valid:\n%s", e.Label, strings.Join(e.Details, "\n"))
}

// Validate checks one settings file against the embedded schema. A missing file passes —
// both settings files are optional.
func Validate(path, label string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	return ValidateBytes(data, label)
}

// ValidateBytes validates an in-memory settings document.
func ValidateBytes(data []byte, label string) error {
	schema, err := settingsSchema()
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return &ValidationFailure{Label: label, Details: []string{fmt.Sprintf("not valid JSON: %v", err)}}
	}
	if err := schema.Validate(instance); err != nil {
		var details []string
		var validationErr *jsonschema.ValidationError
		if ok := asValidationError(err, &validationErr); ok {
			for _, line := range strings.Split(validationErr.Error(), "\n") {
				line = strings.TrimSpace(line)
				if line != "" {
					details = append(details, line)
				}
			}
		} else {
			details = []string{err.Error()}
		}
		return &ValidationFailure{Label: label, Details: details}
	}
	return nil
}

// asValidationError is errors.As specialised to the validator's error type; it exists so
// the import list stays free of errors just for one call site.
func asValidationError(err error, target **jsonschema.ValidationError) bool {
	if typed, ok := err.(*jsonschema.ValidationError); ok {
		*target = typed
		return true
	}
	return false
}
