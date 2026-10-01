// Package paths is the URL grammar of the API (SPEC.md 6.4): one table of
// routes that the router, the OpenAPI document and the metrics labels are all
// generated from (R-PATH-01, R-SYS-01), a parser from a request path to a place
// in the document tree, and the reverse, from a node to its canonical path.
package paths

import (
	"strings"

	"github.com/screenjson/screenjson-db-importer/internal/model"
)

// Method is a set of HTTP methods.
type Method uint8

// The methods the API answers. HEAD is served as GET.
const (
	GET Method = 1 << iota
	POST
	PUT
	PATCH
	DELETE
)

// Common method sets.
const (
	// methodsNode is a node that may be replaced, patched and removed.
	methodsNode = GET | PUT | PATCH | DELETE
	// methodsRequired is a node the schema requires, so it can't be removed.
	methodsRequired = GET | PUT | PATCH
	// methodsList is a collection that can be read and inserted into.
	methodsList = GET | POST
	// methodsAny is every method, for routes that only ever redirect.
	methodsAny = GET | POST | PUT | PATCH | DELETE
)

var methodNames = []struct {
	m    Method
	name string
}{{GET, "GET"}, {POST, "POST"}, {PUT, "PUT"}, {PATCH, "PATCH"}, {DELETE, "DELETE"}}

// ParseMethod converts an HTTP method name. It returns 0 for one the API never
// answers.
func ParseMethod(name string) Method {
	if name == "HEAD" {
		return GET
	}
	for _, mn := range methodNames {
		if mn.name == name {
			return mn.m
		}
	}
	return 0
}

// Has reports whether the set holds m.
func (s Method) Has(m Method) bool { return m != 0 && s&m == m }

// Names lists the set's methods in a fixed order, as an Allow header wants them.
func (s Method) Names() []string {
	var out []string
	for _, mn := range methodNames {
		if s&mn.m != 0 {
			out = append(out, mn.name)
		}
	}
	return out
}

// Caps says which suffixes a route accepts after its own path (SPEC.md 6.4).
type Caps uint8

// The suffix capabilities.
const (
	// CanLang allows …/{textField}/{lang} for the route's LangFields.
	CanLang Caps = 1 << iota
	// CanSet allows …/{setField} for the route's SetFields.
	CanSet
	// CanEmbed allows …/embeddings[/{model}].
	CanEmbed
	// CanCheckout allows …/checkout. On a list route that is a bulk checkout.
	CanCheckout
	// CanMove allows …/move.
	CanMove
)

// Route is one entry in the table: a path pattern and what lives there.
type Route struct {
	// Name identifies the route in code and in logs, for example "element".
	Name string
	// Pattern is the path with {param} placeholders. It is also the template
	// used for metrics labels and in the OpenAPI document.
	Pattern string
	// Methods are the methods the route itself answers, before any suffix.
	Methods Method
	// Kind is the kind of node found here, or held by the list. Empty for
	// objects with no kind of their own (cover, layout, heading and so on) and
	// for action and system routes.
	Kind model.Kind
	// List marks a collection.
	List bool
	// Paged marks a list answered with {"items", "next"} rather than a plain
	// array (R-API-06).
	Paged bool
	// Derived marks data the server computes, which is read-only.
	Derived bool
	// Redirect marks a route that always answers 308 to a node's canonical path
	// (/nodes/{uuid} and an element path without its type, R-PATH-02).
	Redirect bool
	// Allows lists the suffixes accepted after Pattern.
	Allows Caps
	// LangFields are the language-map fields that have …/{field}/{lang} routes.
	LangFields []string
	// SetFields are the unique-string arrays that have …/{field} set routes.
	SetFields []string
	// Anchor names the path parameter identifying the node that owns what this
	// route addresses: the node itself for a node route, its parent for a list
	// or a field object. It is the node that Resolve looks up. Empty for routes
	// outside a document.
	Anchor string
	// AnchorKind is the kind the anchor must be. Empty means any kind, which
	// only /nodes/{uuid} uses.
	AnchorKind model.Kind
	// Pointer is where this route's data sits in the anchor node's ScreenJSON,
	// as a JSON Pointer. Empty when the route is the anchor node itself, and on
	// routes with no place in the ScreenJSON: actions and appearances.
	Pointer string

	// segs is Pattern split into segments.
	segs []string
	// owned is filled by compile: see Owned.
	owned []string
}

// Owned returns the fields of this route's object that have routes of their own
// and so may not be changed through its PATCH (R-PATH-05). The body of a scene,
// for example, belongs to …/elements, and its set fields to their set routes.
// Language maps are not included: a PATCH merges them per language (R-API-09).
func (r *Route) Owned() []string {
	out := make([]string, len(r.owned))
	copy(out, r.owned)
	return out
}

// IsNode reports whether the route addresses a single node rather than a list,
// an action or a system endpoint.
func (r *Route) IsNode() bool {
	return !r.List && r.Anchor != "" && !r.Redirect
}

// Parameter names used in patterns.
const (
	ParamDoc          = "doc"
	ParamScene        = "scene"
	ParamType         = "type"
	ParamElement      = "el"
	ParamKey          = "key"
	ParamSlug         = "slug"
	ParamRegistration = "registration"
	ParamNote         = "note"
	ParamRevision     = "rev"
	ParamID           = "id"
	ParamUUID         = "uuid"
	ParamName         = "name"
	ParamLabel        = "label"
	ParamLang         = "lang"
	ParamModel        = "model"
	ParamFile         = "file"
	ParamJob          = "job"
)

// Wildcard stands for "any document" or "any scene" (R-PATH-03).
const Wildcard = "-"

// rootCollection describes one of the arrays under /documents/{doc}.
type rootCollection struct {
	name    string
	kind    model.Kind
	pointer string
	// key is the item parameter: ParamKey (a UUID), ParamSlug or
	// ParamRegistration.
	key   string
	langs []string
	caps  Caps
}

// rootCollections are the root collections of SPEC.md 6.4, in its order.
var rootCollections = []rootCollection{
	{name: "authors", kind: model.KindAuthor, pointer: "/authors", key: ParamKey},
	{name: "contributors", kind: model.KindContributor, pointer: "/contributors", key: ParamKey, caps: CanSet},
	{name: "characters", kind: model.KindCharacter, pointer: "/characters", key: ParamKey,
		langs: []string{"desc"}, caps: CanLang | CanSet | CanEmbed | CanCheckout},
	{name: "sources", kind: model.KindSource, pointer: "/sources", key: ParamKey,
		langs: []string{"title"}, caps: CanLang},
	{name: "revisions", kind: model.KindRevision, pointer: "/revisions", key: ParamKey},
	{name: "bookmarks", kind: model.KindBookmark, pointer: "/document/bookmarks", key: ParamKey,
		langs: []string{"title", "desc"}, caps: CanLang},
	{name: "colors", kind: model.KindColor, pointer: "/colors", key: ParamSlug,
		langs: []string{"title"}, caps: CanLang},
	{name: "registrations", kind: model.KindRegistration, pointer: "/registrations", key: ParamRegistration},
}

// Prefixes shared by many patterns.
const (
	docPath     = "/documents/{doc}"
	scenePath   = docPath + "/scenes/{scene}"
	elementPath = scenePath + "/elements/{type}/{el}"
)

// table builds the route table. It is a function rather than a package
// variable so that the package holds no global state (SPEC.md 0.6); Table
// compiles it once per call site that needs it.
func table() []*Route {
	rs := []*Route{
		// Documents.
		{Name: "documents", Pattern: "/documents", Methods: methodsList,
			Kind: model.KindDocument, List: true, Paged: true},
		{Name: "document", Pattern: docPath, Methods: methodsNode,
			Kind: model.KindDocument, Anchor: ParamDoc, AnchorKind: model.KindDocument,
			Allows: CanLang | CanSet | CanCheckout, LangFields: []string{"title", "logline"},
			SetFields: model.SetFields(model.KindDocument)},
	}

	for _, c := range rootCollections {
		rs = append(rs, &Route{
			Name: c.name, Pattern: docPath + "/" + c.name, Methods: methodsList,
			Kind: c.kind, List: true, Anchor: ParamDoc, AnchorKind: model.KindDocument,
			Pointer: c.pointer,
		})
		item := &Route{
			Name: strings.TrimSuffix(c.name, "s"), Pattern: docPath + "/" + c.name + "/{" + c.key + "}",
			Methods: methodsNode, Kind: c.kind, Allows: c.caps, LangFields: c.langs,
			SetFields: model.SetFields(c.kind),
		}
		if c.kind.HasUUID() {
			item.Anchor, item.AnchorKind = c.key, c.kind
		} else {
			// Colors and registrations are not in the UUID index, so the
			// document is the anchor and the key picks the item from the array.
			item.Anchor, item.AnchorKind, item.Pointer = ParamDoc, model.KindDocument, c.pointer
		}
		rs = append(rs, item)
	}

	rs = append(rs,
		&Route{Name: "appearances", Pattern: docPath + "/characters/{key}/appearances", Methods: GET,
			Derived: true, Anchor: ParamKey, AnchorKind: model.KindCharacter},

		// Objects on the document.
		&Route{Name: "cover", Pattern: docPath + "/cover", Methods: methodsRequired,
			Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: "/document/cover",
			Allows: CanLang, LangFields: []string{"title", "extra"}},
		&Route{Name: "layout", Pattern: docPath + "/layout", Methods: methodsNode,
			Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: "/document/layout"},
	)
	for _, ribbon := range []string{"header", "footer"} {
		rs = append(rs, &Route{Name: "layout." + ribbon, Pattern: docPath + "/layout/" + ribbon,
			Methods: methodsNode, Anchor: ParamDoc, AnchorKind: model.KindDocument,
			Pointer: "/document/layout/" + ribbon, Allows: CanLang, LangFields: []string{"text"}})
	}
	rs = append(rs, &Route{Name: "layout.status", Pattern: docPath + "/layout/status",
		Methods: methodsNode, Anchor: ParamDoc, AnchorKind: model.KindDocument,
		Pointer: "/document/layout/status"})
	for _, l := range []struct {
		name string
		kind model.Kind
	}{{"styles", model.KindStyle}, {"templates", model.KindTemplate}, {"guides", model.KindGuide}} {
		ptr := "/document/layout/" + l.name
		rs = append(rs,
			&Route{Name: "layout." + l.name, Pattern: docPath + "/layout/" + l.name, Methods: methodsList,
				Kind: l.kind, List: true, Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: ptr},
			&Route{Name: "layout." + strings.TrimSuffix(l.name, "s"), Pattern: docPath + "/layout/" + l.name + "/{slug}",
				Methods: methodsNode, Kind: l.kind, Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: ptr},
		)
	}
	rs = append(rs, &Route{Name: "meta", Pattern: docPath + "/meta", Methods: methodsNode,
		Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: "/document/meta"})
	for _, f := range []string{"generator", "license", "encrypt"} {
		rs = append(rs, &Route{Name: f, Pattern: docPath + "/" + f, Methods: methodsNode,
			Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: "/" + f})
	}

	rs = append(rs,
		// Scenes and elements.
		&Route{Name: "scenes", Pattern: docPath + "/scenes", Methods: methodsList,
			Kind: model.KindScene, List: true, Anchor: ParamDoc, AnchorKind: model.KindDocument,
			Pointer: "/document/scenes", Allows: CanCheckout},
		&Route{Name: "scene", Pattern: scenePath, Methods: methodsNode,
			Kind: model.KindScene, Anchor: ParamScene, AnchorKind: model.KindScene,
			Allows: CanSet | CanEmbed | CanCheckout | CanMove, SetFields: model.SetFields(model.KindScene)},
		&Route{Name: "heading", Pattern: scenePath + "/heading", Methods: methodsRequired,
			Anchor: ParamScene, AnchorKind: model.KindScene, Pointer: "/heading",
			Allows: CanLang, LangFields: []string{"desc"}},
		&Route{Name: "cast", Pattern: scenePath + "/cast", Methods: GET, Derived: true,
			Anchor: ParamScene, AnchorKind: model.KindScene, Pointer: "/cast"},
		&Route{Name: "elements", Pattern: scenePath + "/elements", Methods: methodsList,
			Kind: model.KindElement, List: true, Anchor: ParamScene, AnchorKind: model.KindScene,
			Pointer: "/body", Allows: CanCheckout},
		&Route{Name: "elements.typed", Pattern: scenePath + "/elements/{type}", Methods: methodsList,
			Kind: model.KindElement, List: true, Anchor: ParamScene, AnchorKind: model.KindScene,
			Pointer: "/body", Allows: CanCheckout},
		&Route{Name: "element", Pattern: elementPath, Methods: methodsNode,
			Kind: model.KindElement, Anchor: ParamElement, AnchorKind: model.KindElement,
			Allows:     CanLang | CanSet | CanEmbed | CanCheckout | CanMove,
			LangFields: []string{"text"}, SetFields: model.SetFields(model.KindElement)},
		&Route{Name: "element.untyped", Pattern: scenePath + "/elements/{el}", Methods: methodsAny,
			Redirect: true, Anchor: ParamElement, AnchorKind: model.KindElement},
		&Route{Name: "notes", Pattern: elementPath + "/notes", Methods: methodsList,
			Kind: model.KindNote, List: true, Anchor: ParamElement, AnchorKind: model.KindElement,
			Pointer: "/notes"},
		&Route{Name: "note", Pattern: elementPath + "/notes/{note}", Methods: methodsNode,
			Kind: model.KindNote, Anchor: ParamNote, AnchorKind: model.KindNote,
			Allows: CanLang, LangFields: []string{"text"}},
		&Route{Name: "element.revisions", Pattern: elementPath + "/revisions", Methods: methodsList,
			Kind: model.KindRevision, List: true, Anchor: ParamElement, AnchorKind: model.KindElement,
			Pointer: "/revisions"},
		&Route{Name: "element.revision", Pattern: elementPath + "/revisions/{rev}", Methods: methodsNode,
			Kind: model.KindRevision, Anchor: ParamRevision, AnchorKind: model.KindRevision},

		// Analysis.
		&Route{Name: "analysis", Pattern: docPath + "/analysis", Methods: GET | DELETE,
			Kind: model.KindAnalysis, Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: "/analysis"},
		&Route{Name: "passages", Pattern: docPath + "/analysis/passages", Methods: methodsList,
			Kind: model.KindPassage, List: true, Anchor: ParamDoc, AnchorKind: model.KindDocument,
			Pointer: "/analysis/passages"},
		&Route{Name: "passage", Pattern: docPath + "/analysis/passages/{id}", Methods: methodsNode,
			Kind: model.KindPassage, Anchor: ParamID, AnchorKind: model.KindPassage,
			Allows: CanLang, LangFields: []string{"text"}},
		&Route{Name: "summaries", Pattern: docPath + "/analysis/summaries", Methods: methodsList,
			Kind: model.KindSummary, List: true, Anchor: ParamDoc, AnchorKind: model.KindDocument,
			Pointer: "/analysis/summaries"},
		&Route{Name: "summary", Pattern: docPath + "/analysis/summaries/{id}", Methods: methodsNode,
			Kind: model.KindSummary, Anchor: ParamID, AnchorKind: model.KindSummary,
			Allows: CanLang, LangFields: []string{"text"}},
		&Route{Name: "analysis.settings", Pattern: docPath + "/analysis/settings", Methods: methodsNode,
			Anchor: ParamDoc, AnchorKind: model.KindDocument, Pointer: "/analysis/settings"},

		// Any node by UUID.
		&Route{Name: "node", Pattern: docPath + "/nodes/{uuid}", Methods: methodsAny,
			Redirect: true, Anchor: ParamUUID},

		// Actions on a document (SPEC.md 6.8, 10.2, 11).
		&Route{Name: "checkouts", Pattern: docPath + "/checkouts", Methods: GET,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},
		&Route{Name: "commits", Pattern: docPath + "/commits", Methods: methodsList, Paged: true,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},
		&Route{Name: "export", Pattern: docPath + "/export", Methods: GET,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},
		&Route{Name: "exports", Pattern: docPath + "/exports", Methods: POST,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},
		// The script's own settings and what they have made happen.
		&Route{Name: "settings", Pattern: docPath + "/settings", Methods: GET | PUT,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},
		&Route{Name: "automation", Pattern: docPath + "/automation", Methods: GET | POST,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},
		// CLI-level work, handed to Greenlight (internal/jobs).
		&Route{Name: "jobs", Pattern: docPath + "/jobs", Methods: methodsList,
			Anchor: ParamDoc, AnchorKind: model.KindDocument},

		// Outside any document.
		&Route{Name: "tokens", Pattern: "/tokens", Methods: methodsList, Paged: true},
		&Route{Name: "tokens.me", Pattern: "/tokens/me", Methods: GET},
		&Route{Name: "token", Pattern: "/tokens/{name}", Methods: DELETE},
		&Route{Name: "passes", Pattern: "/passes", Methods: GET, Paged: true},
		&Route{Name: "pass", Pattern: "/passes/{label}", Methods: GET | DELETE},
		&Route{Name: "search", Pattern: "/search", Methods: POST},
		&Route{Name: "imports", Pattern: "/imports", Methods: GET | POST},
		&Route{Name: "import", Pattern: "/imports/{id}", Methods: GET},
		&Route{Name: "job", Pattern: "/jobs/{job}", Methods: GET},
		&Route{Name: "job.outputs", Pattern: "/jobs/{job}/outputs", Methods: GET},
		&Route{Name: "job.output", Pattern: "/jobs/{job}/outputs/{file}", Methods: GET},
		&Route{Name: "greenlight", Pattern: "/greenlight", Methods: GET},
		&Route{Name: "greenlight.tasks", Pattern: "/greenlight/tasks", Methods: GET},
		&Route{Name: "status", Pattern: "/status", Methods: GET},
		&Route{Name: "config", Pattern: "/config", Methods: GET | PATCH},
		&Route{Name: "openapi", Pattern: "/openapi.json", Methods: GET},
		&Route{Name: "healthz", Pattern: "/healthz", Methods: GET},
		&Route{Name: "readyz", Pattern: "/readyz", Methods: GET},
		&Route{Name: "metrics", Pattern: "/metrics", Methods: GET},
	)
	return rs
}
