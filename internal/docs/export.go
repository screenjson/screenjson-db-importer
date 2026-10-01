package docs

import (
	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
)

// Check runs every check a loaded document must pass (SPEC.md 7.1 and 7.2):
// the schema over the whole document, the cross-document rules, and a cast
// that matches each scene's body. fsck uses it.
func Check(set *schema.Set, t *model.Tree) []schema.Defect {
	var out []schema.Defect
	if v, err := t.JSON(); err != nil {
		out = append(out, schema.Defect{Message: err.Error()})
	} else if d := schema.Validate(set.Document(), v); d != nil {
		out = append(out, d...)
	}
	out = append(out, checkDocument(t)...)
	for _, s := range t.Scenes() {
		if _, stale := staleCast(s); stale {
			out = append(out, schema.Defect{Pointer: "/document/scenes", Message: "scene " + s.ID + " has a stale cast"})
		}
	}
	return out
}

// FixCast recomputes every scene's cast in place, reporting how many changed.
func FixCast(t *model.Tree) int {
	n := 0
	for _, s := range t.Scenes() {
		if cast, stale := staleCast(s); stale {
			s.Fields["cast"] = cast
			n++
		}
	}
	return n
}

// FillEnvelopes gives embedded objects without an envelope the starting
// values a load gives them.
func FillEnvelopes(t *model.Tree) { fillEmbeddedEnvelopes(t) }
