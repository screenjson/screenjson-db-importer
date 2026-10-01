// Package schema loads the ScreenJSON JSON Schema, applies the element allOf
// fix described in SPEC.md section 16, and compiles one validator per node kind
// and per element type.
package schema

import (
	"fmt"
	"regexp"
	"strings"
)

// elementTypeDefs are the $defs that compose #/$defs/element with an allOf and
// therefore suffer from the additionalProperties bug of SPEC.md section 16.1.
//
// Note "cue" is the $def name for the element whose "type" is "character".
var elementTypeDefs = []string{
	"action", "shot", "transition", "general", "cue", "parenthetical", "dialogue",
}

// isElementTypeDef reports whether a $defs name is one of the seven.
func isElementTypeDef(name string) bool {
	for _, n := range elementTypeDefs {
		if n == name {
			return true
		}
	}
	return false
}

var (
	// defsOpenRe matches the line opening the top-level $defs object.
	defsOpenRe = regexp.MustCompile(`^  "\$defs":\s*\{\s*$`)
	// topLevelRe matches any other top-level member or the line closing one,
	// which is how the end of $defs is recognised.
	topLevelRe = regexp.MustCompile(`^  ["}\]]`)
	// defOpenRe matches the line opening one entry of $defs. The four-space
	// indent is what makes it unambiguous: everything nested inside a def is
	// indented further, so this can only be a def entry.
	defOpenRe = regexp.MustCompile(`^    "([A-Za-z0-9_]+)":\s*\{\s*$`)
	// addPropsRe matches a line declaring additionalProperties: false.
	addPropsRe = regexp.MustCompile(`^\s*"additionalProperties":\s*false\s*,?\s*$`)
	// unevalRe matches a line declaring unevaluatedProperties, used to leave an
	// already-patched document alone.
	unevalRe = regexp.MustCompile(`^\s*"unevaluatedProperties":\s*false\s*,?\s*$`)
)

// Patch applies the section 16.2 fix to a raw ScreenJSON schema document and
// returns the patched document.
//
// The fix has three parts:
//
//  1. remove additionalProperties from $defs/element,
//  2. remove additionalProperties from each allOf branch of the seven element
//     type defs, and
//  3. add "unevaluatedProperties": false at the top level of each of those defs.
//
// Parts 1 and 2 stop the two allOf branches rejecting each other's properties:
// additionalProperties only sees the properties of its own schema object, so the
// $ref branch rejects "type" and "text" while the inline branch rejects "id" and
// "authors", and no element carrying any field can satisfy both. Part 3 restores
// the closed-object behaviour, evaluated once across the whole allOf rather than
// per branch.
//
// The document is edited line by line rather than decoded and re-encoded. A
// re-encode would reorder keys, drop the blank lines between sections, and turn
// a fifteen-line fix into a whole-file rewrite — unreviewable in the schema
// repository's history, and destructive of the property order that canonical
// ScreenJSON output follows (R-GIT-03). Line editing is safe here because JSON
// escapes newlines inside strings, so no string can span a line.
//
// Patching is idempotent: a document that already carries the fix comes back
// unchanged.
func Patch(raw []byte) ([]byte, error) {
	text := string(raw)
	// Keep the original line ending so the patched file matches its neighbours.
	newline := "\n"
	if strings.Contains(text, "\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")

	out := make([]string, 0, len(lines)+len(elementTypeDefs))
	inDefs := false
	current := ""
	seen := map[string]bool{}

	for i, line := range lines {
		if !inDefs {
			if defsOpenRe.MatchString(line) {
				inDefs = true
			}
			out = append(out, line)
			continue
		}

		// Another top-level member, or the brace closing $defs, ends the section.
		if topLevelRe.MatchString(line) {
			inDefs = false
			current = ""
			out = append(out, line)
			continue
		}

		if m := defOpenRe.FindStringSubmatch(line); m != nil {
			current = m[1]
			seen[current] = true
			out = append(out, line)
			// (3) Close the object across the whole allOf instead of per branch,
			// unless the document already says so.
			if isElementTypeDef(current) && !unevalRe.MatchString(next(lines, i)) {
				out = append(out, `      "unevaluatedProperties": false,`)
			}
			continue
		}

		// (1) and (2): drop the closing keyword wherever it appears inside
		// $defs/element or one of the seven element type defs.
		if (current == "element" || isElementTypeDef(current)) && addPropsRe.MatchString(line) {
			// A member with no trailing comma was the last one, so the comma on
			// the previous emitted line has to go too, or the object is left
			// with a dangling comma.
			if !strings.HasSuffix(strings.TrimSpace(line), ",") {
				dropTrailingComma(out)
			}
			continue
		}

		out = append(out, line)
	}

	// Guard against the schema having been restructured under us: every def the
	// fix targets must have been found.
	missing := []string{"element"}
	missing = append(missing, elementTypeDefs...)
	for _, name := range missing {
		if !seen[name] {
			return nil, fmt.Errorf("schema has no $defs/%s to patch", name)
		}
	}

	return []byte(strings.Join(out, newline)), nil
}

// next returns the line after i, or "".
func next(lines []string, i int) string {
	if i+1 < len(lines) {
		return lines[i+1]
	}
	return ""
}

// dropTrailingComma removes the comma from the last non-blank line emitted.
func dropTrailingComma(out []string) {
	for i := len(out) - 1; i >= 0; i-- {
		trimmed := strings.TrimRight(out[i], " \t")
		if trimmed == "" {
			continue
		}
		out[i] = strings.TrimSuffix(trimmed, ",")
		return
	}
}
