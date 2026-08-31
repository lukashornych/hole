package config

import "encoding/json"

// Merge deep-merges settings documents in ascending precedence: later documents win.
//
// Semantics (unchanged from the bash implementation):
//   - objects merge recursively
//   - arrays concatenate, lower-precedence items first, then deduplicate preserving the
//     first occurrence
//   - scalars and type mismatches are overwritten by the higher-precedence value
func Merge(documents ...Document) Document {
	var merged any
	for _, doc := range documents {
		if doc == nil {
			continue
		}
		if merged == nil {
			merged = mergeValues(nil, map[string]any(doc))
			continue
		}
		merged = mergeValues(merged, map[string]any(doc))
	}
	if merged == nil {
		return Document{}
	}
	if asMap, ok := merged.(map[string]any); ok {
		return Document(asMap)
	}
	return Document{}
}

func mergeValues(lower, higher any) any {
	if lower == nil {
		return cloneValue(higher)
	}
	lowerMap, lowerIsMap := lower.(map[string]any)
	higherMap, higherIsMap := higher.(map[string]any)
	if lowerIsMap && higherIsMap {
		out := make(map[string]any, len(lowerMap)+len(higherMap))
		for key, value := range lowerMap {
			out[key] = cloneValue(value)
		}
		for key, value := range higherMap {
			if existing, ok := out[key]; ok {
				out[key] = mergeValues(existing, value)
				continue
			}
			out[key] = cloneValue(value)
		}
		return out
	}

	lowerSlice, lowerIsSlice := lower.([]any)
	higherSlice, higherIsSlice := higher.([]any)
	if lowerIsSlice && higherIsSlice {
		return dedupSlice(append(cloneSlice(lowerSlice), cloneSlice(higherSlice)...))
	}

	return cloneValue(higher)
}

// dedupSlice removes duplicates by deep value equality while preserving insertion order.
func dedupSlice(items []any) []any {
	out := make([]any, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		key := canonicalKey(item)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

// canonicalKey renders a value as JSON for equality comparison. json.Marshal sorts object
// keys, so two structurally equal objects always produce the same key.
func canonicalKey(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		// Values come from encoding/json, so this cannot happen; fall back to a key that
		// never matches rather than silently deduplicating unequal values.
		return "\x00unmarshalable"
	}
	return string(data)
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, inner := range typed {
			out[key] = cloneValue(inner)
		}
		return out
	case []any:
		return cloneSlice(typed)
	default:
		return value
	}
}

func cloneSlice(items []any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, cloneValue(item))
	}
	return out
}
