package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Value is a JSON value that remembers the order its object keys were written
// in. encoding/json cannot do this: unmarshalling an object into a map loses
// the order, and the schema's property order is meaning, not decoration — it is
// what canonical ScreenJSON output follows (R-GIT-03).
//
// Value exists so BuildOrder can read the schema's declared property order,
// which a map-based decode would throw away.
type Value struct {
	// Kind is 'o' for object, 'a' for array, or 's' for any scalar.
	Kind byte
	// Keys is the object's key order.
	Keys []string
	// Members maps an object's keys to their values.
	Members map[string]*Value
	// Items holds an array's values.
	Items []*Value
	// Raw is a scalar's source bytes, kept verbatim so numbers keep their exact
	// spelling and strings keep their escaping.
	Raw json.RawMessage
}

// ParseValue decodes JSON while preserving object key order.
func ParseValue(raw json.RawMessage) (*Value, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// parseValue reads one value from the decoder.
func parseValue(dec *json.Decoder) (*Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return parseFrom(dec, tok)
}

// parseFrom builds a value from an already-read opening token.
func parseFrom(dec *json.Decoder, tok json.Token) (*Value, error) {
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			out := &Value{Kind: 'o', Members: map[string]*Value{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, fmt.Errorf("parse object key: %w", err)
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, fmt.Errorf("object key is not a string")
				}
				val, err := parseValue(dec)
				if err != nil {
					return nil, fmt.Errorf("at %q: %w", key, err)
				}
				if _, seen := out.Members[key]; !seen {
					out.Keys = append(out.Keys, key)
				}
				out.Members[key] = val
			}
			if _, err := dec.Token(); err != nil { // consumes '}'
				return nil, fmt.Errorf("parse end of object: %w", err)
			}
			return out, nil
		case '[':
			out := &Value{Kind: 'a'}
			for dec.More() {
				item, err := parseValue(dec)
				if err != nil {
					return nil, fmt.Errorf("in array: %w", err)
				}
				out.Items = append(out.Items, item)
			}
			if _, err := dec.Token(); err != nil { // consumes ']'
				return nil, fmt.Errorf("parse end of array: %w", err)
			}
			return out, nil
		default:
			return nil, fmt.Errorf("unexpected delimiter %v", delim)
		}
	}

	// A scalar. Re-encode it so Raw holds exactly one JSON value.
	raw, err := encodeScalar(tok)
	if err != nil {
		return nil, err
	}
	return &Value{Kind: 's', Raw: raw}, nil
}

// encodeScalar renders a scalar token as JSON bytes.
func encodeScalar(tok json.Token) (json.RawMessage, error) {
	switch t := tok.(type) {
	case nil:
		return json.RawMessage("null"), nil
	case bool:
		return json.RawMessage(strconv.FormatBool(t)), nil
	case json.Number:
		return json.RawMessage(t.String()), nil
	case string:
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(t); err != nil {
			return nil, fmt.Errorf("encode string: %w", err)
		}
		return json.RawMessage(bytes.TrimRight(buf.Bytes(), "\n")), nil
	default:
		return nil, fmt.Errorf("unexpected scalar %T", tok)
	}
}

// Get returns a member of an object.
func (v *Value) Get(key string) (*Value, bool) {
	if v == nil || v.Kind != 'o' {
		return nil, false
	}
	m, ok := v.Members[key]
	return m, ok
}
