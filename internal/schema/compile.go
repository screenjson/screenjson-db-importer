package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// printer renders the library's error kinds as English text. The library keeps
// its own equivalent unexported, so we hold one here rather than using the full
// error string, which carries the whole cause tree on one line.
var printer = message.NewPrinter(language.English)

// resourceURL is the base URL the schema is registered under inside the
// compiler. It is local to this process; nothing is fetched over the network.
const resourceURL = "https://screenjson.com/schema.json"

// Set holds the compiled validators for one schema document: the whole
// document, each addressable node kind, and each element type.
type Set struct {
	// Raw is the patched schema document the validators were built from.
	Raw []byte

	document *jsonschema.Schema
	kinds    map[string]*jsonschema.Schema
	elements map[string]*jsonschema.Schema
	// shallow validates a root or scene on its own, with the child lists a
	// layout splits out (scenes, characters, analysis, a scene's body) checked
	// only as arrays or objects. Each child is validated as its own kind when
	// written, so a change to a heading needn't revalidate every element.
	shallow map[string]*jsonschema.Schema
}

// kindDefs maps a node kind (SPEC.md section 2) to its $defs name. The kinds
// "document" and "element" are handled separately: "document" is the root
// schema itself, and an element is validated as one of the seven types.
var kindDefs = map[string]string{
	"scene":        "scene",
	"character":    "character",
	"author":       "author",
	"contributor":  "contributor",
	"source":       "source",
	"revision":     "revision",
	"bookmark":     "bookmark",
	"note":         "note",
	"color":        "color",
	"style":        "style",
	"template":     "template",
	"guide":        "format",
	"registration": "registration",
	"passage":      "passage",
	"summary":      "summary",
	"embedding":    "embedding",
}

// elementDefs maps an element's "type" field to its $defs name. The cue
// element carries type "character" but is defined under $defs/cue.
var elementDefs = map[string]string{
	"action":        "action",
	"character":     "cue",
	"dialogue":      "dialogue",
	"parenthetical": "parenthetical",
	"shot":          "shot",
	"transition":    "transition",
	"general":       "general",
}

// Compile builds the validator set from a raw schema document. The document is
// compiled as JSON Schema Draft 2020-12 whatever its $schema keyword says,
// because ScreenJSON declares a custom draft URL that no validator recognises
// (SPEC.md R-VAL-02, and the open question in section 16.3).
func Compile(raw []byte) (*Set, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	// Drop the custom $schema so the compiler applies its default draft rather
	// than refusing an unknown dialect.
	if m, ok := doc.(map[string]any); ok {
		delete(m, "$schema")
	}

	newCompiler := func() *jsonschema.Compiler {
		c := jsonschema.NewCompiler()
		c.DefaultDraft(jsonschema.Draft2020)
		return c
	}

	compileAt := func(ptr string) (*jsonschema.Schema, error) {
		c := newCompiler()
		if err := c.AddResource(resourceURL, doc); err != nil {
			return nil, fmt.Errorf("add schema resource: %w", err)
		}
		s, err := c.Compile(resourceURL + ptr)
		if err != nil {
			return nil, fmt.Errorf("compile %s: %w", ptr, err)
		}
		return s, nil
	}

	set := &Set{
		Raw:      raw,
		kinds:    make(map[string]*jsonschema.Schema, len(kindDefs)),
		elements: make(map[string]*jsonschema.Schema, len(elementDefs)),
	}

	var err error
	if set.document, err = compileAt(""); err != nil {
		return nil, err
	}
	if set.kinds["analysis"], err = compileAt("#/properties/analysis"); err != nil {
		return nil, err
	}
	if set.shallow, err = compileShallow(doc, newCompiler); err != nil {
		return nil, err
	}
	for kind, def := range kindDefs {
		if set.kinds[kind], err = compileAt("#/$defs/" + def); err != nil {
			return nil, err
		}
	}
	for typ, def := range elementDefs {
		if set.elements[typ], err = compileAt("#/$defs/" + def); err != nil {
			return nil, err
		}
	}
	return set, nil
}

// Document returns the validator for a whole ScreenJSON document.
func (s *Set) Document() *jsonschema.Schema { return s.document }

// Shallow returns the validator for a root ("document") or a scene that leaves
// out their splittable children: document.scenes, characters and analysis on
// the root, body on a scene. Each is checked only for being an array (or, for
// analysis, an object). The rule that a document has at least one scene is a
// document check (SPEC.md 7.2) rather than part of this.
func (s *Set) Shallow(kind string) (*jsonschema.Schema, bool) {
	v, ok := s.shallow[kind]
	return v, ok
}

// Kind returns the validator for a node kind, or false if the kind has no
// schema of its own. "analysis" is included: it is not a $defs entry, but is
// stored as a record of its own.
func (s *Set) Kind(kind string) (*jsonschema.Schema, bool) {
	v, ok := s.kinds[kind]
	return v, ok
}

// Element returns the validator for an element type, or false if the type is
// not one of the seven ScreenJSON element types.
func (s *Set) Element(typ string) (*jsonschema.Schema, bool) {
	v, ok := s.elements[typ]
	return v, ok
}

// Defect is one schema violation, located by a JSON Pointer into the value that
// was checked. It matches the shape of an API error detail (SPEC.md R-API-04).
type Defect struct {
	Pointer string `json:"pointer"`
	Message string `json:"message"`
}

// Validate checks a value, which must be the result of decoding JSON into any,
// against a compiled schema. It returns nil when the value is valid.
func Validate(s *jsonschema.Schema, v any) []Defect {
	err := s.Validate(v)
	if err == nil {
		return nil
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return []Defect{{Pointer: "", Message: err.Error()}}
	}
	return flatten(ve)
}

// ValidateJSON decodes raw JSON and checks it against a compiled schema. Numbers
// are decoded with json.Number so that large integers keep their exact value.
func ValidateJSON(s *jsonschema.Schema, raw []byte) ([]Defect, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	return Validate(s, v), nil
}

// flatten turns the tree of causes a validation error carries into a flat list
// of leaf defects, which is what an API client can act on.
func flatten(ve *jsonschema.ValidationError) []Defect {
	if len(ve.Causes) == 0 {
		return []Defect{{
			Pointer: "/" + strings.Join(ve.InstanceLocation, "/"),
			Message: ve.ErrorKind.LocalizedString(printer),
		}}
	}
	var out []Defect
	for _, c := range ve.Causes {
		out = append(out, flatten(c)...)
	}
	return out
}

// shallowURL is where the loosened copy of the schema is registered.
const shallowURL = "https://screenjson.com/shallow.json"

// compileShallow builds the Shallow validators from a copy of the schema with
// the splittable child lists loosened.
func compileShallow(doc any, newCompiler func() *jsonschema.Compiler) (map[string]*jsonschema.Schema, error) {
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	var loose map[string]any
	if err := json.Unmarshal(raw, &loose); err != nil {
		return nil, err
	}
	set := func(path []string, v any) error {
		cur := loose
		for _, p := range path[:len(path)-1] {
			next, ok := cur[p].(map[string]any)
			if !ok {
				return fmt.Errorf("shallow schema: no %s", strings.Join(path, "/"))
			}
			cur = next
		}
		cur[path[len(path)-1]] = v
		return nil
	}
	array := map[string]any{"type": "array"}
	object := map[string]any{"type": "object"}
	for _, e := range []struct {
		path []string
		v    any
	}{
		{[]string{"properties", "document", "properties", "scenes"}, array},
		{[]string{"properties", "characters"}, array},
		{[]string{"properties", "analysis"}, object},
		{[]string{"$defs", "scene", "properties", "body"}, array},
	} {
		if err := set(e.path, e.v); err != nil {
			return nil, err
		}
	}

	c := newCompiler()
	if err := c.AddResource(shallowURL, loose); err != nil {
		return nil, fmt.Errorf("add shallow schema: %w", err)
	}
	out := map[string]*jsonschema.Schema{}
	for kind, ptr := range map[string]string{"document": "", "scene": "#/$defs/scene"} {
		s, err := c.Compile(shallowURL + ptr)
		if err != nil {
			return nil, fmt.Errorf("compile shallow %s: %w", kind, err)
		}
		out[kind] = s
	}
	return out, nil
}

// DefName returns the $defs name of a node kind's schema ("guide" is
// "format"), or false if the kind has none of its own.
func DefName(kind string) (string, bool) {
	d, ok := kindDefs[kind]
	return d, ok
}

// ElementDefNames maps each element type to its $defs name.
func ElementDefNames() map[string]string {
	out := make(map[string]string, len(elementDefs))
	for k, v := range elementDefs {
		out[k] = v
	}
	return out
}
