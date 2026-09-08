package policy

import (
	"fmt"
	"strconv"
	"strings"
)

// parseMiniYAML parses a small subset of YAML into a generic
// map[string]any/[]any/string/bool/nil tree, which Load then round-trips
// through encoding/json (marshal the map, unmarshal into Policy) so the
// policy.Policy struct's existing `json` tags do all the real field mapping —
// they are identical to its `yaml` tags field-for-field (see iface.go), so
// this only has to produce the right generic shape, never know about Policy
// itself.
//
// Supported, and only this: top-level "key: value" mappings; block sequences
// (a key with no inline value, followed by "  - item" lines indented under
// it); flow sequences ("key: [a, b, c]"); single- and double-quoted scalars;
// bare true/false/null; "#" comments (only when not inside a quoted scalar,
// and only when starting a token — "#" glued to the middle of a word is left
// alone); blank lines.
//
// Not supported, deliberately: nesting deeper than one level, anchors and
// aliases, multi-document streams, flow mappings ("{a: b}"), block scalars
// (| and >), multi-line flow sequences. policy.Policy is a flat structure —
// every field is a scalar or a list of scalars — so this is enough for every
// field it declares, and it is not meant to be a general YAML parser for
// anything else in this project.
func parseMiniYAML(data []byte) (map[string]any, error) {
	lines := strings.Split(normalizeNewlines(string(data)), "\n")
	result := map[string]any{}

	i := 0
	for i < len(lines) {
		indent, content := splitIndent(lines[i])
		content = strings.TrimRight(stripYAMLComment(content), " \t")
		if content == "" || content == "---" || content == "..." {
			i++
			continue
		}
		if indent != 0 {
			return nil, fmt.Errorf("policy: line %d: unexpected indentation (nesting beyond one level is not supported): %q", i+1, lines[i])
		}

		key, rest, ok := splitYAMLKey(content)
		if !ok {
			return nil, fmt.Errorf("policy: line %d: expected \"key: value\": %q", i+1, lines[i])
		}
		// YAML forbids a repeated key in one mapping, and a map cannot record
		// that it happened: the second assignment silently replaces the
		// first. For a policy file that is a rule quietly disarmed by a
		// later, probably forgotten, line — so it is refused here, where the
		// line number is still known, rather than downstream where only the
		// surviving value remains.
		if _, dup := result[key]; dup {
			return nil, fmt.Errorf("policy: line %d: duplicate key %q", i+1, key)
		}
		rest = strings.TrimSpace(rest)
		i++

		switch {
		case rest == "":
			items, next := collectBlockSequence(lines, i)
			if items != nil {
				result[key] = items
				i = next
			} else {
				result[key] = nil
			}
		case strings.HasPrefix(rest, "["):
			items, err := parseYAMLFlowSeq(rest)
			if err != nil {
				return nil, fmt.Errorf("policy: line %d: %w", i, err)
			}
			result[key] = items
		default:
			result[key] = parseYAMLScalar(rest)
		}
	}
	return result, nil
}

func normalizeNewlines(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\r", "\n")
}

// collectBlockSequence reads indented "- item" lines starting at lines[from].
// It returns nil (not a sequence) without consuming anything when the next
// non-blank line is not indented or is not a list item — in that case the
// key's value is YAML null.
func collectBlockSequence(lines []string, from int) ([]any, int) {
	var items []any
	i := from
	for i < len(lines) {
		indent, content := splitIndent(lines[i])
		content = strings.TrimRight(stripYAMLComment(content), " \t")
		if content == "" {
			i++
			continue
		}
		if indent == 0 {
			break
		}
		item, ok := splitYAMLListItem(content)
		if !ok {
			break
		}
		items = append(items, parseYAMLScalar(strings.TrimSpace(item)))
		i++
	}
	if items == nil {
		return nil, from
	}
	return items, i
}

// splitIndent returns the count of leading ASCII spaces and the remainder of
// the line. YAML forbids tabs for indentation; a leading tab is treated as
// one column of indentation here rather than rejected outright, since being
// lenient about it only affects whether collectBlockSequence recognises a
// sequence item, never silently misreads a value.
func splitIndent(line string) (int, string) {
	n := 0
	for n < len(line) && (line[n] == ' ' || line[n] == '\t') {
		n++
	}
	return n, line[n:]
}

// stripYAMLComment removes a "#" comment: a '#' starts a comment only when it
// is at the start of the content or preceded by whitespace, and only when it
// is not inside a single- or double-quoted scalar.
func stripYAMLComment(s string) string {
	var inSingle, inDouble bool
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == '#' && !inSingle && !inDouble && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

// splitYAMLKey splits "key: value" (or "key:" with nothing after it) at the
// first unquoted ": " or trailing ":". A bare colon inside a scalar (e.g. a
// URL) that is not followed by whitespace or end-of-line is not a key
// separator, matching plain YAML.
func splitYAMLKey(content string) (key, rest string, ok bool) {
	var inSingle, inDouble bool
	for i := 0; i < len(content); i++ {
		c := content[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == ':' && !inSingle && !inDouble:
			if i == len(content)-1 {
				return strings.TrimSpace(content[:i]), "", true
			}
			if content[i+1] == ' ' || content[i+1] == '\t' {
				return strings.TrimSpace(content[:i]), content[i+1:], true
			}
		}
	}
	return "", "", false
}

// splitYAMLListItem recognises a block sequence item: "- x", "-\tx", or a
// bare "-" (an item whose value is empty/null).
func splitYAMLListItem(content string) (item string, ok bool) {
	if content == "-" {
		return "", true
	}
	if len(content) >= 2 && content[0] == '-' && (content[1] == ' ' || content[1] == '\t') {
		return content[2:], true
	}
	return "", false
}

// parseYAMLFlowSeq parses "[a, b, c]". It does not support nested flow
// collections — policy.Policy never needs one.
func parseYAMLFlowSeq(s string) ([]any, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("malformed flow sequence: %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []any{}, nil
	}
	var out []any
	for _, part := range splitFlowItems(inner) {
		out = append(out, parseYAMLScalar(strings.TrimSpace(part)))
	}
	return out, nil
}

// splitFlowItems splits a flow sequence's inner text on commas that are not
// inside a quoted scalar.
func splitFlowItems(s string) []string {
	var out []string
	var inSingle, inDouble bool
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'' && !inDouble:
			inSingle = !inSingle
		case c == '"' && !inSingle:
			inDouble = !inDouble
		case c == ',' && !inSingle && !inDouble:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

// parseYAMLScalar interprets one scalar token: quoted string, bool, null, or
// a bare string (YAML's implicit-typing rules for numbers/dates are not
// applied — every policy.Policy scalar field is a string, bool, or
// []string, so a number-looking value such as a version glob is kept as
// text).
func parseYAMLScalar(s string) any {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
		return s[1 : len(s)-1]
	}
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	switch s {
	case "true", "True", "TRUE":
		return true
	case "false", "False", "FALSE":
		return false
	case "null", "Null", "NULL", "~", "":
		return nil
	}
	return s
}
