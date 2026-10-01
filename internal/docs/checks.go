package docs

import (
	"fmt"
	"sort"
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/layout"
	"github.com/screenjson/screenjson-db-importer/internal/model"
	"github.com/screenjson/screenjson-db-importer/internal/paths"
	"github.com/screenjson/screenjson-db-importer/internal/schema"
	"github.com/screenjson/screenjson-db-importer/internal/state"
)

// idSet collects the IDs held in a root array of objects.
func idSet(fields map[string]any, key string) map[string]bool {
	out := map[string]bool{}
	list, _ := fields[key].([]any)
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				out[id] = true
			}
		}
	}
	return out
}

// strings returns the strings in a JSON array.
func stringList(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// checkNodes applies the per-node rules of checkDocument to some scenes and
// elements only, reporting whether they all pass.
func checkNodes(t *model.Tree, nodes []*model.Node) bool {
	root := t.Root.Fields
	authors := idSet(root, "authors")
	contributors := idSet(root, "contributors")
	in := func(v any, set map[string]bool) bool {
		for _, id := range stringList(v) {
			if !set[id] {
				return false
			}
		}
		return true
	}
	for _, n := range nodes {
		if !in(n.Fields["authors"], authors) || !in(n.Fields["contributors"], contributors) {
			return false
		}
		if n.Kind != model.KindElement {
			continue
		}
		if n.Type == model.TypeCharacter || n.Type == model.TypeDialogue {
			if c, _ := n.Fields["character"].(string); c != "" {
				if ch := t.Node(c); ch == nil || ch.Kind != model.KindCharacter {
					return false
				}
			}
		}
		if sc, ok := n.Fields["scene"]; ok && (n.Parent == nil || sc != n.Parent.ID) {
			return false
		}
		revs, _ := n.Fields["revisions"].([]any)
		for _, item := range revs {
			if m, ok := item.(map[string]any); ok && !in(m["authors"], authors) {
				return false
			}
		}
		notes, _ := n.Fields["notes"].([]any)
		for _, item := range notes {
			if m, ok := item.(map[string]any); ok {
				if c, ok := m["contributor"].(string); ok && !contributors[c] {
					return false
				}
			}
		}
	}
	return true
}

// checkDocument runs the rules of SPEC.md 7.2 over a whole tree. Unique IDs are
// checked when the tree is indexed. Pointers are into the assembled document.
func checkDocument(t *model.Tree) []schema.Defect {
	var out []schema.Defect
	add := func(ptr, format string, args ...any) {
		if len(out) < 200 {
			out = append(out, schema.Defect{Pointer: ptr, Message: fmt.Sprintf(format, args...)})
		}
	}
	root := t.Root.Fields
	authors := idSet(root, "authors")
	contributors := idSet(root, "contributors")
	sources := idSet(root, "sources")
	characters := map[string]bool{}
	for _, c := range t.Root.Characters() {
		characters[c.ID] = true
	}

	refs := func(ptr string, v any, set map[string]bool, what string) {
		for i, id := range stringList(v) {
			if !set[id] {
				add(fmt.Sprintf("%s/%d", ptr, i), "%s %s is not in the document's %s", what, id, what+"s")
			}
		}
	}
	revisions := func(ptr string, v any) {
		list, _ := v.([]any)
		for i, item := range list {
			if m, ok := item.(map[string]any); ok {
				refs(fmt.Sprintf("%s/%d/authors", ptr, i), m["authors"], authors, "author")
			}
		}
	}

	wrapper, _ := root["document"].(map[string]any)
	cover, _ := wrapper["cover"].(map[string]any)
	refs("/document/cover/authors", cover["authors"], authors, "author")
	refs("/document/cover/sources", cover["sources"], sources, "source")
	revisions("/revisions", root["revisions"])

	scenes := t.Scenes()
	if len(scenes) == 0 {
		add("/document/scenes", "a document needs at least one scene")
	}
	sceneBody := map[string]map[string]bool{}
	allElements := map[string]bool{}
	for si, s := range scenes {
		sp := fmt.Sprintf("/document/scenes/%d", si)
		refs(sp+"/authors", s.Fields["authors"], authors, "author")
		refs(sp+"/contributors", s.Fields["contributors"], contributors, "contributor")
		body := map[string]bool{}
		for ei, el := range s.Body() {
			ep := fmt.Sprintf("%s/body/%d", sp, ei)
			body[el.ID] = true
			allElements[el.ID] = true
			refs(ep+"/authors", el.Fields["authors"], authors, "author")
			refs(ep+"/contributors", el.Fields["contributors"], contributors, "contributor")
			if el.Type == model.TypeCharacter || el.Type == model.TypeDialogue {
				if c, _ := el.Fields["character"].(string); c != "" && !characters[c] {
					add(ep+"/character", "character %s is not in the document's characters", c)
				}
			}
			if sc, ok := el.Fields["scene"]; ok && sc != s.ID {
				add(ep+"/scene", "element's scene is %v but it is in scene %s", sc, s.ID)
			}
			revisions(ep+"/revisions", el.Fields["revisions"])
			notes, _ := el.Fields["notes"].([]any)
			for ni, item := range notes {
				if m, ok := item.(map[string]any); ok {
					if c, ok := m["contributor"].(string); ok && !contributors[c] {
						add(fmt.Sprintf("%s/notes/%d/contributor", ep, ni), "contributor %s is not in the document's contributors", c)
					}
				}
			}
		}
		sceneBody[s.ID] = body
	}

	bookmarks, _ := wrapper["bookmarks"].([]any)
	for i, item := range bookmarks {
		m, _ := item.(map[string]any)
		bp := fmt.Sprintf("/document/bookmarks/%d", i)
		sc, _ := m["scene"].(string)
		body, ok := sceneBody[sc]
		if !ok {
			add(bp+"/scene", "bookmark's scene %s does not exist", sc)
			continue
		}
		if el, _ := m["element"].(string); !body[el] {
			add(bp+"/element", "bookmark's element %s is not in scene %s", el, sc)
		}
	}

	if a := t.Root.Analysis(); a != nil {
		passages, _ := a.Fields["passages"].([]any)
		for i, item := range passages {
			m, _ := item.(map[string]any)
			pp := fmt.Sprintf("/analysis/passages/%d", i)
			if sc, _ := m["scene"].(string); sceneBody[sc] == nil {
				add(pp+"/scene", "passage's scene %s does not exist", sc)
			}
			for j, id := range stringList(m["elements"]) {
				if !allElements[id] {
					add(fmt.Sprintf("%s/elements/%d", pp, j), "passage's element %s does not exist", id)
				}
			}
		}
		summaries, _ := a.Fields["summaries"].([]any)
		for i, item := range summaries {
			m, _ := item.(map[string]any)
			if m["scope"] != "scene" {
				continue
			}
			if target, _ := m["target"].(string); sceneBody[target] == nil {
				add(fmt.Sprintf("/analysis/summaries/%d/target", i), "summary's target scene %s does not exist", target)
			}
		}
	}
	return out
}

// referencing lists up to 50 IDs of nodes that refer to id, for the 409
// reference answer when deleting a character, author, contributor or source
// that is still in use (R-VAL-06).
func referencing(t *model.Tree, id string, kind model.Kind) []string {
	var out []string
	add := func(ref string) {
		if len(out) < 50 {
			out = append(out, ref)
		}
	}
	has := func(v any) bool {
		for _, s := range stringList(v) {
			if s == id {
				return true
			}
		}
		return false
	}
	root := t.Root.Fields
	wrapper, _ := root["document"].(map[string]any)
	cover, _ := wrapper["cover"].(map[string]any)
	switch kind {
	case model.KindAuthor:
		if has(cover["authors"]) {
			add(t.Doc())
		}
	case model.KindSource:
		if has(cover["sources"]) {
			add(t.Doc())
		}
	}
	checkRevisions := func(owner string, v any) {
		list, _ := v.([]any)
		for _, item := range list {
			if m, ok := item.(map[string]any); ok && has(m["authors"]) {
				rid, _ := m["id"].(string)
				if rid == "" {
					rid = owner
				}
				add(rid)
			}
		}
	}
	if kind == model.KindAuthor {
		checkRevisions(t.Doc(), root["revisions"])
	}
	for _, s := range t.Scenes() {
		switch kind {
		case model.KindAuthor:
			if has(s.Fields["authors"]) {
				add(s.ID)
			}
		case model.KindContributor:
			if has(s.Fields["contributors"]) {
				add(s.ID)
			}
		}
		for _, el := range s.Body() {
			switch kind {
			case model.KindAuthor:
				if has(el.Fields["authors"]) {
					add(el.ID)
				}
				checkRevisions(el.ID, el.Fields["revisions"])
			case model.KindContributor:
				if has(el.Fields["contributors"]) {
					add(el.ID)
				}
				notes, _ := el.Fields["notes"].([]any)
				for _, item := range notes {
					if m, ok := item.(map[string]any); ok && m["contributor"] == id {
						nid, _ := m["id"].(string)
						add(nid)
					}
				}
			case model.KindCharacter:
				if el.Fields["character"] == id {
					add(el.ID)
				}
			}
		}
	}
	return out
}

// IndexEntry builds a document's entry in the state-store index (SPEC.md 4.7).
func IndexEntry(t *model.Tree) state.DocEntry {
	root := t.Root.Fields
	lang, _ := root["lang"].(string)
	e := state.DocEntry{
		ID:      t.Doc(),
		Title:   layout.SearchText(t.Root, lang),
		Logline: pickText(root["logline"], lang),
		Genre:   stringList(root["genre"]),
		Themes:  stringList(root["themes"]),
		Lang:    lang,
		Seq:     t.Seq(),
		Updated: t.Root.Env.Updated,
	}
	authors, _ := root["authors"].([]any)
	for _, a := range authors {
		if m, ok := a.(map[string]any); ok {
			given, _ := m["given"].(string)
			family, _ := m["family"].(string)
			e.Authors = append(e.Authors, strings.TrimSpace(given+" "+family))
		}
	}
	langs := map[string]bool{}
	collect := func(v any) {
		if m, ok := v.(map[string]any); ok {
			for k := range m {
				langs[k] = true
			}
		}
	}
	collect(root["title"])
	collect(root["logline"])
	e.Scenes, e.Elements = t.Counts()
	var size int
	for _, s := range t.Scenes() {
		for _, el := range s.Body() {
			collect(el.Fields["text"])
			if txt, ok := el.Fields["text"].(map[string]any); ok {
				for _, v := range txt {
					if s, ok := v.(string); ok {
						size += len(s)
					}
				}
			}
		}
	}
	for l := range langs {
		e.Langs = append(e.Langs, l)
	}
	sort.Strings(e.Langs)
	e.Bytes = int64(size)
	if e.Updated.IsZero() {
		e.Updated = t.Root.Env.Updated
	}
	return e
}

// pickText picks one language of a text map, as layout does for search text.
func pickText(v any, lang string) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	if s, ok := m[lang].(string); ok {
		return s
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return ""
	}
	s, _ := m[keys[0]].(string)
	return s
}

// queueTextHook prepares a text.changed webhook for a node (SPEC.md 9). An
// encrypted node's payload carries no text (R-ENC-03).
func (w *write) queueTextHook(n *model.Node) {
	p, _ := paths.For(n)
	ev := HookEvent{
		Event: OpTextChangedHook, Doc: w.t.Doc(), ID: n.ID, Kind: string(n.Kind), Type: string(n.Type),
		Path: p, EmbedURL: p + "/embeddings/{model}",
	}
	if m, ok := textSig(n).(map[string]any); ok && n.Kind == model.KindElement {
		for k := range m {
			ev.Langs = append(ev.Langs, k)
		}
		sort.Strings(ev.Langs)
		ev.Text = m
	}
	if w.encrypted() || ownEncrypted(n) {
		ev.Text, ev.Encrypted = nil, true
	}
	w.hooks = append(w.hooks, ev)
}

// sendHooks queues webhooks for the committed events. text.changed payloads
// get their final text_rev here.
func (w *write) sendHooks() {
	for _, h := range w.hooks {
		if n := w.t.Node(h.ID); n != nil {
			h.TextRev = n.Env.TextRev
		}
		w.m.o.Hooks.Enqueue(h)
	}
	for _, ev := range w.events {
		switch ev.Op {
		case OpInserted, OpRemoved, OpMoved, OpUpdated, OpCreated, OpDeleted:
			w.m.o.Hooks.Enqueue(HookEvent{Event: ev.Op, Doc: ev.Doc, ID: ev.ID, Kind: ev.Kind, Type: ev.Type, Path: ev.Path})
		}
	}
}
