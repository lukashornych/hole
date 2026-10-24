package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SettingsTemplate is the document `hole settings create` writes: only the schema reference,
// so editors offer completion against the settings schema.
func SettingsTemplate() []byte {
	return []byte("{\n  \"$schema\": \"" + schemaResourceURL + "\"\n}\n")
}

// TemplateMode selects how WriteSettingsTemplate treats an existing settings file.
type TemplateMode int

const (
	// TemplateCreate fails when the file exists.
	TemplateCreate TemplateMode = iota
	// TemplateOverwrite replaces the file with the template.
	TemplateOverwrite
	// TemplateAppend merges the template's top-level fields into the file.
	TemplateAppend
)

// ErrSettingsExist is returned in TemplateCreate mode when the file already exists.
var ErrSettingsExist = errors.New("settings file already exists")

// WriteSettingsTemplate writes the core template to path according to mode and reports
// whether the file existed before. A missing file is created in every mode, together with its
// directory. os.Stat follows symlinks, so a dangling link counts as missing and the file is
// created at the link target, like an editor would do.
func WriteSettingsTemplate(path string, mode TemplateMode) (bool, error) {
	info, err := os.Stat(path)
	switch {
	case os.IsNotExist(err):
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return false, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, SettingsTemplate(), 0o644); err != nil {
			return false, fmt.Errorf("write %s: %w", path, err)
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("stat %s: %w", path, err)
	case !info.Mode().IsRegular():
		return true, fmt.Errorf("'%s' is not a settings file", path)
	}

	switch mode {
	case TemplateCreate:
		return true, fmt.Errorf("%w: '%s'; use --overwrite to replace it or --append to add the template fields", ErrSettingsExist, path)
	case TemplateOverwrite:
		if err := os.WriteFile(path, SettingsTemplate(), 0o644); err != nil {
			return true, fmt.Errorf("write %s: %w", path, err)
		}
		return true, nil
	default:
		existing, err := os.ReadFile(path)
		if err != nil {
			return true, fmt.Errorf("read %s: %w", path, err)
		}
		merged, err := AppendTemplate(existing, SettingsTemplate())
		if err != nil {
			return true, fmt.Errorf("%s: %w", path, err)
		}
		if err := os.WriteFile(path, merged, 0o644); err != nil {
			return true, fmt.Errorf("write %s: %w", path, err)
		}
		return true, nil
	}
}

// settingsField is one top-level key of a settings document with its value kept verbatim.
type settingsField struct {
	key   string
	value json.RawMessage
}

// AppendTemplate merges the template's top-level fields into an existing document. A key
// present in both is replaced in place with the template's whole value; a missing key is
// inserted before all existing keys, so `$schema` ends up first. Nested values are copied
// verbatim, so their key order survives; only the indentation is normalised.
//
// It does not go through Document, whose map would sort the keys and lose the user's order.
func AppendTemplate(existing, template []byte) ([]byte, error) {
	fields, err := topLevelFields(existing)
	if err != nil {
		return nil, fmt.Errorf("cannot merge into existing settings: %w; fix it or use --overwrite", err)
	}
	additions, err := topLevelFields(template)
	if err != nil {
		return nil, fmt.Errorf("parse settings template: %w", err)
	}
	var inserted []settingsField
	for _, addition := range additions {
		replaced := false
		for i := range fields {
			if fields[i].key == addition.key {
				fields[i].value = addition.value
				replaced = true
				break
			}
		}
		if !replaced {
			inserted = append(inserted, addition)
		}
	}
	return encodeTopLevel(append(inserted, fields...))
}

// topLevelFields reads the top-level object of a JSON document in key order. Whitespace-only
// input is an empty object. A non-object, invalid JSON, trailing data or a duplicate key is an
// error, so nothing is silently dropped or reordered.
func topLevelFields(data []byte) ([]settingsField, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := expectDelim(decoder, '{'); err != nil {
		return nil, err
	}
	var fields []settingsField
	seen := map[string]bool{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected token %v", token)
		}
		if seen[key] {
			return nil, fmt.Errorf("duplicate top-level key %q", key)
		}
		seen[key] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields = append(fields, settingsField{key: key, value: value})
	}
	if err := expectDelim(decoder, '}'); err != nil {
		return nil, err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("unexpected %v after the top-level object", token)
	}
	return fields, nil
}

func expectDelim(decoder *json.Decoder, want json.Delim) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delim, ok := token.(json.Delim); !ok || delim != want {
		return fmt.Errorf("expected %q, got %v", want, token)
	}
	return nil
}

// encodeTopLevel writes the fields as one object and re-indents it with two spaces.
func encodeTopLevel(fields []settingsField) ([]byte, error) {
	var compact bytes.Buffer
	compact.WriteByte('{')
	for i, field := range fields {
		if i > 0 {
			compact.WriteByte(',')
		}
		key, err := json.Marshal(field.key)
		if err != nil {
			return nil, err
		}
		compact.Write(key)
		compact.WriteByte(':')
		compact.Write(field.value)
	}
	compact.WriteByte('}')
	var indented bytes.Buffer
	if err := json.Indent(&indented, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	indented.WriteByte('\n')
	return indented.Bytes(), nil
}
