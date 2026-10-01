// Package layout decides which parts of a document are stored as records of
// their own (SPEC.md 5.2), takes a document apart into those records, and puts
// records back together into a document.
package layout

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/store"
)

// Level is a place in the document that can be stored separately.
type Level string

// The five levels, in the order SPEC.md 5.2 lists them.
const (
	Root       Level = "root"
	Scenes     Level = "scenes"
	Elements   Level = "elements"
	Characters Level = "characters"
	Analysis   Level = "analysis"
)

// levels lists every level in a fixed order.
var levels = []Level{Root, Scenes, Elements, Characters, Analysis}

// kindOf maps a level to the kind of node stored at it.
var kindOf = map[Level]model.Kind{
	Root:       model.KindDocument,
	Scenes:     model.KindScene,
	Elements:   model.KindElement,
	Characters: model.KindCharacter,
	Analysis:   model.KindAnalysis,
}

// Spec is one level's storage.
type Spec struct {
	// Collection is the collection, table, index or class the level is stored in.
	Collection string `json:"collection" yaml:"collection"`
	// Vector is the level's native vector, or nil (SPEC.md 5.5).
	Vector *store.VectorSpec `json:"vector,omitempty" yaml:"vector,omitempty"`
}

// Layout maps each split level to its storage. A level not in the map stays
// embedded in its parent's record.
type Layout map[Level]Spec

// Preset names (R-STORE-02).
const (
	PresetWhole    = "whole"
	PresetScenes   = "scenes"
	PresetElements = "elements"
	// PresetDefault is the preset used when none is configured.
	PresetDefault = PresetElements
)

// Preset returns a named preset with the default collection names.
func Preset(name string) (Layout, error) {
	root := Spec{Collection: "screenplays"}
	switch name {
	case PresetWhole:
		return Layout{Root: root}, nil
	case PresetScenes:
		return Layout{Root: root, Scenes: {Collection: "scenes"}, Analysis: {Collection: "analysis"}}, nil
	case PresetElements:
		return Layout{
			Root: root, Scenes: {Collection: "scenes"}, Elements: {Collection: "elements"},
			Characters: {Collection: "characters"}, Analysis: {Collection: "analysis"},
		}, nil
	}
	return nil, fmt.Errorf("layout: unknown preset %q (want whole, scenes or elements)", name)
}

// Parse reads a layout from SCREENJSON_STORAGE_LAYOUT: a preset name, or a
// JSON object with the full layout (R-CFG-04). The result is validated.
func Parse(s string) (Layout, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return Preset(s)
	}
	var l Layout
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&l); err != nil {
		return nil, fmt.Errorf("layout: parse JSON layout: %w", err)
	}
	if err := l.Validate(); err != nil {
		return nil, err
	}
	return l, nil
}

// collectionName is safe as a Mongo collection, Postgres table, Elastic index
// (once lowercased), Chroma collection and Weaviate class.
var collectionName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,62}$`)

// MaxDimensions is the largest vector ScreenJSON allows (SPEC.md 3.4).
const MaxDimensions = 4096

// Validate checks a layout, with messages clear enough to fail startup on
// (R-STORE-03).
func (l Layout) Validate() error {
	var problems []string
	for lv := range l {
		if _, known := kindOf[lv]; !known {
			problems = append(problems, fmt.Sprintf("unknown level %q (levels are root, scenes, elements, characters, analysis)", lv))
		}
	}
	if _, ok := l[Root]; !ok {
		problems = append(problems, "the root level is required")
	}
	_, scenes := l[Scenes]
	if _, elements := l[Elements]; elements && !scenes {
		problems = append(problems, "elements can only be split out if scenes are too")
	}
	used := map[string]Level{}
	for _, lv := range levels {
		spec, ok := l[lv]
		if !ok {
			continue
		}
		if !collectionName.MatchString(spec.Collection) {
			problems = append(problems, fmt.Sprintf("%s: collection name %q must be a letter then up to 62 letters, digits or underscores", lv, spec.Collection))
		}
		// Names are compared ignoring case: Elastic lowercases index names.
		key := strings.ToLower(spec.Collection)
		if other, dup := used[key]; dup {
			problems = append(problems, fmt.Sprintf("%s and %s both use collection %q", other, lv, spec.Collection))
		}
		used[key] = lv
		if v := spec.Vector; v != nil {
			if lv == Root || lv == Analysis {
				problems = append(problems, fmt.Sprintf("%s: only scenes, elements and characters can have a native vector, since only they have embeddings", lv))
			}
			if v.Model == "" {
				problems = append(problems, fmt.Sprintf("%s: vector needs a model", lv))
			}
			if v.Dimensions < 1 || v.Dimensions > MaxDimensions {
				problems = append(problems, fmt.Sprintf("%s: vector dimensions must be 1 to %d, not %d", lv, MaxDimensions, v.Dimensions))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("layout: invalid: %s", strings.Join(problems, "; "))
	}
	return nil
}

// Splits reports whether nodes of a kind are stored as records of their own.
func (l Layout) Splits(k model.Kind) bool {
	for lv, kind := range kindOf {
		if kind == k {
			_, ok := l[lv]
			return ok
		}
	}
	return false
}

// LevelOf returns the level a kind is stored at, if it is a splittable kind.
func LevelOf(k model.Kind) (Level, bool) {
	for lv, kind := range kindOf {
		if kind == k {
			return lv, true
		}
	}
	return "", false
}

// Collection returns the collection records of a kind live in: the kind's own
// level if it is split, otherwise the collection of the record that embeds it.
func (l Layout) Collection(k model.Kind) string {
	switch {
	case l.Splits(k):
		lv, _ := LevelOf(k)
		return l[lv].Collection
	case k == model.KindElement:
		return l.Collection(model.KindScene)
	}
	return l[Root].Collection
}

// VectorFor returns the native vector of a kind's level, or nil.
func (l Layout) VectorFor(k model.Kind) *store.VectorSpec {
	lv, ok := LevelOf(k)
	if !ok {
		return nil
	}
	if spec, split := l[lv]; split {
		return spec.Vector
	}
	return nil
}

// Collections lists what Ensure must create, in level order. Every level but
// analysis gets a text index, since analysis records carry no search text.
func (l Layout) Collections() []store.CollectionSpec {
	var out []store.CollectionSpec
	for _, lv := range levels {
		spec, ok := l[lv]
		if !ok {
			continue
		}
		out = append(out, store.CollectionSpec{
			Name: spec.Collection, Level: string(lv), Vector: spec.Vector, FullText: lv != Analysis,
		})
	}
	return out
}

// normalised is the layout as the fingerprint hashes it: levels in a fixed
// order, so map order can't change the hash.
type normalised struct {
	Envelope string       `json:"envelope"`
	Levels   []levelEntry `json:"levels"`
}

type levelEntry struct {
	Level      Level             `json:"level"`
	Collection string            `json:"collection"`
	Vector     *store.VectorSpec `json:"vector,omitempty"`
}

// Normalise returns the canonical JSON of the layout and envelope field name.
// It is what the fingerprint hashes and what is stored beside it, so relayout
// can read documents written with the old layout (R-STORE-07).
func (l Layout) Normalise(envelopeField string) []byte {
	n := normalised{Envelope: envelopeField}
	for _, lv := range levels {
		if spec, ok := l[lv]; ok {
			n.Levels = append(n.Levels, levelEntry{Level: lv, Collection: spec.Collection, Vector: spec.Vector})
		}
	}
	b, err := json.Marshal(n)
	if err != nil {
		// Only strings, ints and fixed structs are marshalled.
		panic(fmt.Sprintf("layout: marshal normalised layout: %v", err))
	}
	return b
}

// Fingerprint is the SHA-256 of the normalised layout (SPEC.md 5.3).
func (l Layout) Fingerprint(envelopeField string) string {
	sum := sha256.Sum256(l.Normalise(envelopeField))
	return hex.EncodeToString(sum[:])
}

// FromNormalised reads back what Normalise wrote.
func FromNormalised(b []byte) (Layout, string, error) {
	var n normalised
	if err := json.Unmarshal(b, &n); err != nil {
		return nil, "", fmt.Errorf("layout: read stored layout: %w", err)
	}
	l := Layout{}
	for _, e := range n.Levels {
		l[e.Level] = Spec{Collection: e.Collection, Vector: e.Vector}
	}
	return l, n.Envelope, l.Validate()
}
