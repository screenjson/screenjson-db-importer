// Package model holds the node kinds, the document tree, and ScreenJSON
// (de)serialisation, including the canonical output form.
package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/screenjson/screenjson-db-importer/internal/schema"
)

// Canonical writes a ScreenJSON value in the one form SPEC.md R-GIT-03 defines:
//
//   - object keys in the order the schema declares them,
//   - two-space indent,
//   - no HTML escaping,
//   - a trailing newline.
//
// The same value always produces the same bytes, which is what makes two Git
// exports byte-identical and keeps a one-word text edit to a one-line diff.
//
// Keys the schema does not name — the languages of a text map, meta keys,
// embedding model names — are sorted, since the schema gives no order for them
// and sorting is the only stable choice.
func Canonical(v any, ord *schema.Order) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("canonical: marshal: %w", err)
	}
	return CanonicalJSON(raw, ord)
}

// CanonicalJSON rewrites already-encoded JSON into canonical form.
func CanonicalJSON(raw json.RawMessage, ord *schema.Order) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeValue(&buf, raw, ord, 0); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

const indentUnit = "  "

// writeIndent writes the indent for the given depth.
func writeIndent(buf *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		buf.WriteString(indentUnit)
	}
}

// writeValue dispatches on the JSON type of raw.
func writeValue(buf *bytes.Buffer, raw json.RawMessage, ord *schema.Order, depth int) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return fmt.Errorf("canonical: empty value")
	}
	switch trimmed[0] {
	case '{':
		return writeObject(buf, trimmed, ord, depth)
	case '[':
		return writeArray(buf, trimmed, ord, depth)
	default:
		return writeScalar(buf, trimmed)
	}
}

// writeObject emits an object with its keys in schema order, then any remaining
// keys sorted.
func writeObject(buf *bytes.Buffer, raw json.RawMessage, ord *schema.Order, depth int) error {
	members, err := decodeObject(raw)
	if err != nil {
		return err
	}
	if len(members) == 0 {
		buf.WriteString("{}")
		return nil
	}

	present := make(map[string]json.RawMessage, len(members))
	for _, m := range members {
		present[m.key] = m.val
	}

	// Schema order first.
	keys := make([]string, 0, len(members))
	if ord != nil {
		for _, name := range ord.Props {
			if _, ok := present[name]; ok {
				keys = append(keys, name)
				delete(present, name)
			}
		}
	}
	// Then whatever the schema does not name, sorted.
	rest := make([]string, 0, len(present))
	for k := range present {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	keys = append(keys, rest...)

	buf.WriteString("{\n")
	for i, key := range keys {
		writeIndent(buf, depth+1)
		if err := writeString(buf, key); err != nil {
			return err
		}
		buf.WriteString(": ")
		val := valueOf(members, key)
		if err := writeValue(buf, val, ord.Child(key), depth+1); err != nil {
			return fmt.Errorf("canonical: at %q: %w", key, err)
		}
		if i < len(keys)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	writeIndent(buf, depth)
	buf.WriteByte('}')
	return nil
}

// writeArray emits an array, keeping its order: in ScreenJSON, array position
// is meaning (SPEC.md 3.4, "no implicit sorting").
func writeArray(buf *bytes.Buffer, raw json.RawMessage, ord *schema.Order, depth int) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("canonical: parse array: %w", err)
	}
	if len(items) == 0 {
		buf.WriteString("[]")
		return nil
	}
	buf.WriteString("[\n")
	for i, item := range items {
		writeIndent(buf, depth+1)
		if err := writeValue(buf, item, ord.Elem(), depth+1); err != nil {
			return fmt.Errorf("canonical: at index %d: %w", i, err)
		}
		if i < len(items)-1 {
			buf.WriteByte(',')
		}
		buf.WriteByte('\n')
	}
	writeIndent(buf, depth)
	buf.WriteByte(']')
	return nil
}

// writeScalar emits a string, number, boolean or null.
func writeScalar(buf *bytes.Buffer, raw json.RawMessage) error {
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("canonical: parse string: %w", err)
		}
		return writeString(buf, s)
	case 't', 'f', 'n':
		buf.Write(raw)
		return nil
	default:
		// Numbers are written back exactly as they arrived, so an integer stays
		// an integer and a decimal keeps its digits. Re-encoding through
		// float64 would turn 1 into 1e+00 and lose precision on long values.
		if _, err := strconv.ParseFloat(string(raw), 64); err != nil {
			return fmt.Errorf("canonical: %q is not a JSON value", raw)
		}
		buf.Write(raw)
		return nil
	}
}

// writeString emits a JSON string without HTML escaping (R-GIT-03). The
// standard encoder escapes <, > and & by default, which would make exported
// dialogue hard to read and differ from what the API returns.
func writeString(buf *bytes.Buffer, s string) error {
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return fmt.Errorf("canonical: encode string: %w", err)
	}
	// Encode appends a newline of its own.
	buf.Truncate(buf.Len() - 1)
	return nil
}

// member is one key/value pair of an object, in the order it was written.
type member struct {
	key string
	val json.RawMessage
}

// decodeObject reads an object keeping its key order, so that keys the schema
// does not name can still be handled deterministically.
func decodeObject(raw json.RawMessage) ([]member, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if _, err := dec.Token(); err != nil {
		return nil, fmt.Errorf("canonical: parse object: %w", err)
	}
	var out []member
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("canonical: parse object key: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return nil, fmt.Errorf("canonical: object key is not a string")
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, fmt.Errorf("canonical: parse value of %q: %w", key, err)
		}
		out = append(out, member{key: key, val: val})
	}
	return out, nil
}

// valueOf finds a member's value by key.
func valueOf(members []member, key string) json.RawMessage {
	for _, m := range members {
		if m.key == key {
			return m.val
		}
	}
	return json.RawMessage("null")
}
