package paths

import (
	"fmt"
	"net/http"
	"net/url"

	"github.com/screenjson/screenjson-db-importer/internal/model"
)

// collectionOf names the root collection each kind held in one lives in.
var collectionOf = map[model.Kind]string{
	model.KindAuthor:       "authors",
	model.KindContributor:  "contributors",
	model.KindCharacter:    "characters",
	model.KindSource:       "sources",
	model.KindBookmark:     "bookmarks",
	model.KindColor:        "colors",
	model.KindRegistration: "registrations",
}

// layoutListOf names the layout list each layout kind lives in.
var layoutListOf = map[model.Kind]string{
	model.KindStyle:    "styles",
	model.KindTemplate: "templates",
	model.KindGuide:    "guides",
}

// For returns a node's canonical path: the one Location headers carry and
// stale paths redirect to (R-PATH-07). The node must be attached to its
// document, since its path depends on its ancestors: an element's holds its
// scene and its type, so it changes when either does.
func For(n *model.Node) (string, error) {
	if n == nil {
		return "", fmt.Errorf("paths: no node")
	}
	doc := "/documents/" + url.PathEscape(n.Doc())
	id := url.PathEscape(n.ID)

	switch n.Kind {
	case model.KindDocument:
		return doc, nil
	case model.KindScene:
		return doc + "/scenes/" + id, nil
	case model.KindElement:
		scene := n.Parent
		if scene == nil || scene.Kind != model.KindScene {
			return "", fmt.Errorf("paths: %s is not in a scene", n)
		}
		return doc + "/scenes/" + url.PathEscape(scene.ID) + "/elements/" + string(n.Type) + "/" + id, nil
	case model.KindNote:
		el, err := For(n.Parent)
		if err != nil {
			return "", fmt.Errorf("paths: note %s: %w", n.ID, err)
		}
		return el + "/notes/" + id, nil
	case model.KindRevision:
		// A revision lives on the root or on an element.
		if n.Parent != nil && n.Parent.Kind == model.KindElement {
			el, err := For(n.Parent)
			if err != nil {
				return "", fmt.Errorf("paths: revision %s: %w", n.ID, err)
			}
			return el + "/revisions/" + id, nil
		}
		return doc + "/revisions/" + id, nil
	case model.KindRegistration:
		authority, _ := n.Fields["authority"].(string)
		regID, _ := n.Fields["id"].(string)
		return doc + "/registrations/" + url.PathEscape(RegistrationKey(authority, regID)), nil
	case model.KindAnalysis:
		return doc + "/analysis", nil
	case model.KindPassage:
		return doc + "/analysis/passages/" + id, nil
	case model.KindSummary:
		return doc + "/analysis/summaries/" + id, nil
	}
	if coll, ok := collectionOf[n.Kind]; ok {
		return doc + "/" + coll + "/" + id, nil
	}
	if list, ok := layoutListOf[n.Kind]; ok {
		return doc + "/layout/" + list + "/" + id, nil
	}
	return "", fmt.Errorf("paths: %s has no path of its own", n)
}

// Resolved is a target found in its document.
type Resolved struct {
	// Node is the anchor node: the node itself for a node route, the parent for
	// a list or a field object such as a scene's heading.
	Node *model.Node
	// Location is set when the request must be redirected with 308 because the
	// path is stale — the node moved or changed type — or because the route
	// only ever redirects (R-PATH-02, R-PATH-07). It is the canonical path with
	// the rest of the request path carried over; the caller appends the query.
	Location string
}

// Resolve finds a target's anchor node in its document's tree.
//
// It answers 410 gone for an ID the tree remembers deleting and 404 for one it
// doesn't know (R-PATH-08), and 404 when the ID names the wrong kind of node,
// such as a scene ID in an author path. A live node requested at a path that is
// not its canonical one comes back with Location set (R-PATH-07).
func Resolve(t *model.Tree, tg *Target) (*Resolved, error) {
	if tg.anchor == "" {
		return nil, badRequest("%s does not address a node in one document", tg.Template)
	}
	if doc := tg.Doc(); doc != t.Doc() {
		return nil, notFound("document %s is not %s", t.Doc(), doc)
	}

	var n *model.Node
	if tg.anchorKind == model.KindDocument {
		n = t.Root
	} else {
		id := tg.Params[tg.anchor]
		n = t.Node(id)
		if n == nil {
			if t.WasDeleted(id) {
				return nil, &Error{Status: http.StatusGone, Code: CodeGone,
					Message: fmt.Sprintf("%s was deleted", id)}
			}
			return nil, notFound("no node %s in document %s", id, t.Doc())
		}
		if tg.anchorKind != "" && n.Kind != tg.anchorKind {
			return nil, notFound("%s is a %s, not a %s", id, n.Kind, tg.anchorKind)
		}
	}

	canon, err := For(n)
	if err != nil {
		return nil, err
	}
	res := &Resolved{Node: n}
	if canon != tg.anchorPath {
		res.Location = canon + tg.tail
	}
	return res, nil
}
