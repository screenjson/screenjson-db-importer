package model

import "fmt"

// Kind is the schema definition a node is validated as (SPEC.md section 2).
type Kind string

// The node kinds. Every addressable part of a document has one.
const (
	KindDocument     Kind = "document"
	KindScene        Kind = "scene"
	KindElement      Kind = "element"
	KindCharacter    Kind = "character"
	KindAuthor       Kind = "author"
	KindContributor  Kind = "contributor"
	KindSource       Kind = "source"
	KindRevision     Kind = "revision"
	KindBookmark     Kind = "bookmark"
	KindNote         Kind = "note"
	KindColor        Kind = "color"
	KindStyle        Kind = "style"
	KindTemplate     Kind = "template"
	KindGuide        Kind = "guide"
	KindRegistration Kind = "registration"
	KindPassage      Kind = "passage"
	KindSummary      Kind = "summary"
	KindEmbedding    Kind = "embedding"
	// KindAnalysis is not a schema definition but is addressable and, when the
	// analysis level is split out, stored as its own record.
	KindAnalysis Kind = "analysis"
)

// allKinds lists every kind, for validation and for iterating in a fixed order.
var allKinds = []Kind{
	KindDocument, KindScene, KindElement, KindCharacter, KindAuthor,
	KindContributor, KindSource, KindRevision, KindBookmark, KindNote,
	KindColor, KindStyle, KindTemplate, KindGuide, KindRegistration,
	KindPassage, KindSummary, KindEmbedding, KindAnalysis,
}

// Kinds returns every node kind.
func Kinds() []Kind {
	out := make([]Kind, len(allKinds))
	copy(out, allKinds)
	return out
}

// ValidKind reports whether k is a known kind.
func ValidKind(k Kind) bool {
	for _, known := range allKinds {
		if known == k {
			return true
		}
	}
	return false
}

// HasUUID reports whether nodes of this kind are addressed by a UUID.
//
// Three kinds are not: a color is keyed by its slug, a registration by
// "{authority}~{id}" because its id is an opaque authority string rather than a
// UUID, and analysis is a singleton per document.
func (k Kind) HasUUID() bool {
	switch k {
	case KindColor, KindRegistration, KindAnalysis:
		return false
	default:
		return true
	}
}

// ElementType is the "type" field of an element (SPEC.md 3.3).
type ElementType string

// The seven element types.
const (
	TypeAction        ElementType = "action"
	TypeCharacter     ElementType = "character"
	TypeDialogue      ElementType = "dialogue"
	TypeParenthetical ElementType = "parenthetical"
	TypeShot          ElementType = "shot"
	TypeTransition    ElementType = "transition"
	TypeGeneral       ElementType = "general"
)

// allElementTypes lists the seven types in the order the schema declares them.
var allElementTypes = []ElementType{
	TypeAction, TypeCharacter, TypeDialogue, TypeParenthetical,
	TypeShot, TypeTransition, TypeGeneral,
}

// ElementTypes returns the seven element types.
func ElementTypes() []ElementType {
	out := make([]ElementType, len(allElementTypes))
	copy(out, allElementTypes)
	return out
}

// ValidElementType reports whether t is one of the seven.
func ValidElementType(t ElementType) bool {
	for _, known := range allElementTypes {
		if known == t {
			return true
		}
	}
	return false
}

// HasText reports whether an element of this type carries a text map.
//
// A character cue does not: it names a character and may carry a "display"
// string instead. Anything reading "the element's text" must skip cues
// (SPEC.md 3.3).
func (t ElementType) HasText() bool {
	return t != TypeCharacter
}

// ParseElementType checks and converts a string.
func ParseElementType(s string) (ElementType, error) {
	t := ElementType(s)
	if !ValidElementType(t) {
		return "", fmt.Errorf("model: %q is not a ScreenJSON element type", s)
	}
	return t, nil
}

// TextFields are the fields holding a text map: a map from a BCP 47 tag to a
// string of at most 10,000 characters (SPEC.md 3.4). They have per-language
// routes (R-API-18) and changes to them bump text_rev.
//
// A cue's "display" is not one: the schema makes it a plain string.
var TextFields = []string{"text", "desc", "logline", "extra"}

// NameFields are the fields holding a name map, whose strings are limited to 255
// characters. They have per-language routes too.
var NameFields = []string{"title"}

// IsTextField reports whether a field name holds a text map.
func IsTextField(name string) bool {
	for _, f := range TextFields {
		if f == name {
			return true
		}
	}
	return false
}

// IsNameField reports whether a field name holds a name map.
func IsNameField(name string) bool {
	for _, f := range NameFields {
		if f == name {
			return true
		}
	}
	return false
}

// IsLangMapField reports whether a field name holds either kind of language map.
func IsLangMapField(name string) bool {
	return IsTextField(name) || IsNameField(name)
}

// setFields lists, per kind, the fields that are arrays of unique strings and so
// have set routes taking {"add": [...], "remove": [...]} (SPEC.md 6.10).
var setFields = map[Kind][]string{
	KindScene: {
		"animals", "extra", "locations", "moods", "props",
		"sfx", "sounds", "tags", "vfx", "wardrobe",
	},
	KindDocument:    {"genre", "themes", "taggable"},
	KindElement:     {"styles", "access"},
	KindCharacter:   {"aliases", "traits"},
	KindContributor: {"roles"},
}

// SetFields returns the set-valued fields of a kind, in a fixed order.
func SetFields(k Kind) []string {
	src := setFields[k]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// IsSetField reports whether a kind has a set route for this field.
//
// Note scene "extra" is a set of slugs, while cover "extra" is a text map. The
// kind decides which, so this must always be asked with a kind.
func IsSetField(k Kind, name string) bool {
	for _, f := range setFields[k] {
		if f == name {
			return true
		}
	}
	return false
}
