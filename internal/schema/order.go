package schema

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Order is a tree describing the key order of one place in a ScreenJSON
// document, mirrored from the schema. It drives canonical output (SPEC.md
// R-GIT-03: "object keys in the order properties appear in the schema"), which
// is what makes two exports of the same document byte-identical and keeps a
// one-word edit to a one-line diff.
type Order struct {
	// Props is the order the schema declares this object's properties in.
	// It is empty for a map-like object — a text map, meta, or embeddings —
	// whose keys the schema does not name; those are sorted instead.
	Props []string
	// Children maps a property name to the order of its value.
	Children map[string]*Order
	// Items is the order of this array's elements, or nil.
	Items *Order
	// Values is the order of a map-like object's values, or nil.
	Values *Order
}

// Child returns the order for a property's value, falling back to the map-value
// order, or nil when the schema says nothing about it.
func (o *Order) Child(name string) *Order {
	if o == nil {
		return nil
	}
	if c, ok := o.Children[name]; ok {
		return c
	}
	return o.Values
}

// Elem returns the order for an array element, or nil.
func (o *Order) Elem() *Order {
	if o == nil {
		return nil
	}
	return o.Items
}

// BuildOrder walks a schema document and returns the key order for the root.
//
// It resolves local $refs, and merges allOf, oneOf and anyOf branches in the
// order they are written. Merging oneOf is what gives the seven element types
// one stable key order: every branch contributes the properties it adds, first
// occurrence wins, and because elements are closed objects any real element's
// keys are a subset of the merged list.
func BuildOrder(raw []byte) (*Order, error) {
	doc, err := ParseValue(raw)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	b := &orderBuilder{root: doc, done: map[string]*Order{}}
	return b.build(doc, map[string]bool{}), nil
}

// OrderAt returns the key order for one $defs entry, for exporting a single
// node rather than a whole document.
func OrderAt(raw []byte, def string) (*Order, error) {
	doc, err := ParseValue(raw)
	if err != nil {
		return nil, fmt.Errorf("parse schema: %w", err)
	}
	b := &orderBuilder{root: doc, done: map[string]*Order{}}
	target := b.resolve("/$defs/" + def)
	if target == nil {
		return nil, fmt.Errorf("schema has no $defs/%s", def)
	}
	return b.build(target, map[string]bool{}), nil
}

type orderBuilder struct {
	root *Value
	// done memoises $defs that have been built, both to save work and so a
	// recursive definition terminates.
	done map[string]*Order
}

// resolve walks a local JSON Pointer through the schema document.
func (b *orderBuilder) resolve(ptr string) *Value {
	cur := b.root
	for _, part := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		if part == "" {
			continue
		}
		next, ok := cur.Get(unescapePointer(part))
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// build produces the Order for one schema node. seen guards against a $ref
// cycle, which ScreenJSON has: a note contains notes.
func (b *orderBuilder) build(node *Value, seen map[string]bool) *Order {
	if node == nil || node.Kind != 'o' {
		return nil
	}

	if refVal, ok := node.Get("$ref"); ok && refVal.Kind == 's' {
		var ref string
		if err := json.Unmarshal(refVal.Raw, &ref); err != nil {
			return nil
		}
		target := strings.TrimPrefix(ref, "#")
		if o, built := b.done[target]; built {
			return o
		}
		if seen[target] {
			// Mid-cycle: the outer call fills this in.
			return nil
		}
		seen[target] = true
		o := b.build(b.resolve(target), seen)
		delete(seen, target)
		b.done[target] = o
		return o
	}

	out := &Order{Children: map[string]*Order{}}

	// allOf, oneOf and anyOf branches all contribute, in the order written.
	for _, key := range []string{"allOf", "oneOf", "anyOf"} {
		branches, ok := node.Get(key)
		if !ok || branches.Kind != 'a' {
			continue
		}
		for _, branch := range branches.Items {
			out.merge(b.build(branch, seen))
		}
	}

	if props, ok := node.Get("properties"); ok && props.Kind == 'o' {
		for _, name := range props.Keys {
			out.addProp(name)
			if existing, ok := out.Children[name]; !ok || existing == nil {
				out.Children[name] = b.build(props.Members[name], seen)
			}
		}
	}

	if items, ok := node.Get("items"); ok {
		if o := b.build(items, seen); o != nil {
			out.Items = o
		}
	}

	// A map-like object names its values through additionalProperties or
	// patternProperties. Its keys are data, not schema, so they are sorted at
	// output time; only the shape of the values matters here.
	if ap, ok := node.Get("additionalProperties"); ok {
		if o := b.build(ap, seen); o != nil {
			out.Values = o
		}
	}
	if pp, ok := node.Get("patternProperties"); ok && pp.Kind == 'o' {
		for _, pat := range pp.Keys {
			if o := b.build(pp.Members[pat], seen); o != nil {
				out.Values = o
				break
			}
		}
	}

	if len(out.Props) == 0 && out.Items == nil && out.Values == nil {
		return nil
	}
	return out
}

// merge folds another order into o, keeping the first occurrence of each
// property so that branch order decides.
func (o *Order) merge(other *Order) {
	if other == nil {
		return
	}
	for _, name := range other.Props {
		o.addProp(name)
		if existing, ok := o.Children[name]; !ok || existing == nil {
			o.Children[name] = other.Children[name]
		}
	}
	if o.Items == nil {
		o.Items = other.Items
	}
	if o.Values == nil {
		o.Values = other.Values
	}
}

// addProp appends a property name unless it is already present.
func (o *Order) addProp(name string) {
	for _, existing := range o.Props {
		if existing == name {
			return
		}
	}
	o.Props = append(o.Props, name)
}

func unescapePointer(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
}
