package config

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// HasGlobChars reports whether an entry should be treated as a pattern rather than a
// literal path. It matches the bash test for `*`, `?` and `[`.
func HasGlobChars(value string) bool {
	return strings.ContainsAny(value, "*?[")
}

// ExpandGlob resolves a glob pattern against root and returns matching paths relative to
// root, sorted. Only existing paths are returned (bash `nullglob`), and `**` matches zero
// or more path segments (bash `globstar`) — semantics stdlib filepath.Glob does not have.
func ExpandGlob(root, pattern string) []string {
	pattern = path.Clean(strings.TrimPrefix(filepath.ToSlash(pattern), "./"))
	if pattern == "." || pattern == "" {
		return nil
	}
	segments := strings.Split(pattern, "/")

	var matches []string
	expandSegments(root, "", segments, &matches)
	if len(matches) == 0 {
		return nil
	}

	sort.Strings(matches)
	return dedupStrings(matches)
}

func expandSegments(root, rel string, segments []string, out *[]string) {
	if len(segments) == 0 {
		if rel != "" {
			*out = append(*out, rel)
		}
		return
	}

	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		return
	}

	if segments[0] == "**" {
		// `**` may consume zero segments...
		expandSegments(root, rel, segments[1:], out)
		// ...or any number of them.
		for _, entry := range entries {
			child := path.Join(rel, entry.Name())
			if entry.IsDir() {
				expandSegments(root, child, segments, out)
				continue
			}
			expandSegments(root, child, segments[1:], out)
		}
		return
	}

	matcher := segmentMatcher(segments[0])
	for _, entry := range entries {
		if !matcher.MatchString(entry.Name()) {
			continue
		}
		child := path.Join(rel, entry.Name())
		if len(segments) == 1 {
			*out = append(*out, child)
			continue
		}
		if entry.IsDir() {
			expandSegments(root, child, segments[1:], out)
		}
	}
}

// segmentMatcher compiles one path segment of a glob into an anchored regexp.
func segmentMatcher(segment string) *regexp.Regexp {
	var sb strings.Builder
	sb.WriteString("^")
	for i := 0; i < len(segment); i++ {
		switch segment[i] {
		case '*':
			// A `**` inside a segment (rather than as a whole segment) degrades to `*`,
			// which is what bash does too.
			for i+1 < len(segment) && segment[i+1] == '*' {
				i++
			}
			sb.WriteString("[^/]*")
		case '?':
			sb.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(segment[i:], ']')
			if end < 0 {
				sb.WriteString(regexp.QuoteMeta("["))
				continue
			}
			class := segment[i : i+end+1]
			if strings.HasPrefix(class, "[!") {
				class = "[^" + class[2:]
			}
			sb.WriteString(class)
			i += end
		default:
			sb.WriteString(regexp.QuoteMeta(string(segment[i])))
		}
	}
	sb.WriteString("$")

	compiled, err := regexp.Compile(sb.String())
	if err != nil {
		// A pattern that cannot compile can only match its literal self.
		return regexp.MustCompile("^" + regexp.QuoteMeta(segment) + "$")
	}
	return compiled
}

func dedupStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}
