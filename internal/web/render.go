// internal/web/render.go
package web

import (
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/browsemodel"
)

// base is embedded by every page's view-model so the layout can render identity.
type base struct {
	Session *auth.Session
	CSRF    string
	Flash   string
}

type landingData struct {
	base
	Repos map[string][]Repo
}

type loginData struct {
	base
	Error     string
	Next      string
	OIDC      bool
	OIDCLabel string
}

type errorData struct {
	base
	Code    int
	Message string
}

type browseHeader struct {
	base
	Tenant   string
	Repo     string
	Ref      string // current ref display name
	OID      string // resolved OID; used by RefOrOID when Ref is empty
	Refs     browsemodel.Refs
	Path     string                            // current directory path ("" at repo root)
	Activity map[string]browsemodel.CommitMeta // entry path -> last commit (best-effort; nil => "—")
	CanAdmin bool                              // session may manage this repo (shows [settings] link)
}

// RefOrOID returns the ref name for links, falling back to the resolved OID
// when the page was reached via a raw OID (Ref empty).
func (h browseHeader) RefOrOID() string {
	if h.Ref != "" {
		return h.Ref
	}
	return h.OID
}

type repoHomeData struct {
	browseHeader
	Entries    []browsemodel.TreeEntry
	ReadmeHTML template.HTML // sanitized; "" when no README (real render in Task 16)
}

type treeData struct {
	browseHeader
	Entries    []browsemodel.TreeEntry
	ReadmeHTML template.HTML
}

type blobData struct {
	browseHeader
	Path     string
	Blob     browsemodel.Blob
	Code     template.HTML // highlighted HTML; empty for binary/too-large
	Markdown bool          // path renders as Markdown (offer the toggle)
	Rendered template.HTML // sanitized rendered Markdown when ?view=rendered
}

type commitsData struct {
	browseHeader
	Commits []browsemodel.CommitMeta
	Page    int
	HasMore bool
}

type commitData struct {
	browseHeader
	Detail browsemodel.CommitDetail
}

// renderer parses the page templates. With dir=="" it parses the embedded
// assets once; with a non-empty dir it re-parses from disk on every render so
// designers can hot-iterate (templates/ under the given dir).
type renderer struct {
	dir      string
	cache    map[string]*template.Template
	partials *template.Template
}

func newRenderer(dir string) (*renderer, error) {
	r := &renderer{dir: dir}
	if dir == "" {
		r.cache = map[string]*template.Template{}
		for _, page := range []string{"landing.html", "login.html", "error.html", "repo.html", "tree.html", "blob.html", "commits.html", "commit.html", "settings.html", "settings_tokens.html", "settings_keys.html", "secret.html", "reposettings.html", "reposettings_access.html", "reposettings_webhooks.html", "reposettings_deliveries.html", "reposettings_policy.html", "reposettings_hooks.html", "reposettings_triggers.html", "reposettings_triggers_form.html", "admin.html", "admin_users.html", "admin_repos.html", "admin_quotas.html"} {
			t, err := parsePage(assetsFS, "templates", page)
			if err != nil {
				return nil, err
			}
			r.cache[page] = t
		}
		p, err := partialsSet(assetsFS, "templates")
		if err != nil {
			return nil, err
		}
		r.partials = p
	}
	return r, nil
}

// templateFuncs returns the FuncMap used by all page and partial templates.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"plus1": func(n int) int { return n + 1 },
		"minus1": func(n int) int {
			if n <= 0 {
				return 0
			}
			return n - 1
		},
		"urlpath": func(p string) string {
			seg := strings.Split(p, "/")
			for i := range seg {
				seg[i] = url.PathEscape(seg[i])
			}
			return strings.Join(seg, "/")
		},
		"reltime":   func(unix int64) string { return relTimeAt(time.Now(), unix) },
		"abstime":   absTime,
		"humansize": humanSize,
		"deref":     func(b *bool) bool { return b != nil && *b },
		"diffclass": func(kind byte) string { return diffClass(kind) },
		"scopestr": func(sc auth.TokenScope) string {
			if sc == auth.ScopeLegacy {
				return "legacy (full access)"
			}
			var parts []string
			for _, p := range []struct {
				bit  auth.TokenScope
				name string
			}{
				{auth.ScopeRepoAdmin, "repo:admin"}, {auth.ScopeRepoWrite, "repo:write"},
				{auth.ScopeRepoRead, "repo:read"}, {auth.ScopeLFSWrite, "lfs:write"},
				{auth.ScopeLFSRead, "lfs:read"}, {auth.ScopeWebhookAdmin, "webhook:admin"},
				{auth.ScopeStorageAdmin, "storage:admin"},
			} {
				if sc&p.bit != 0 {
					parts = append(parts, p.name)
				}
			}
			return strings.Join(parts, ",")
		},
	}
}

// parsePage parses base.html, _partials.html, and the named page file from fsys.
// The dir argument is the path prefix within fsys (e.g. "templates" or ".").
// Each page file ends with {{template "base" .}}, which means executing
// the template named after the page file produces the full rendered page.
func parsePage(fsys fs.FS, dir, page string) (*template.Template, error) {
	base, pg := "base.html", page
	partials := "_partials.html"
	if dir != "" && dir != "." {
		base = dir + "/base.html"
		pg = dir + "/" + page
		partials = dir + "/_partials.html"
	}
	return template.New("").Funcs(templateFuncs()).ParseFS(fsys, base, partials, pg)
}

// partialsSet parses just _partials.html (no base/page) for fragment rendering.
func partialsSet(fsys fs.FS, dir string) (*template.Template, error) {
	p := "_partials.html"
	if dir != "" && dir != "." {
		p = dir + "/_partials.html"
	}
	return template.New("").Funcs(templateFuncs()).ParseFS(fsys, p)
}

// renderPartial renders a named fragment from _partials.html (htmx swaps).
func (r *renderer) renderPartial(w io.Writer, name string, data any) error {
	if r.dir != "" {
		t, err := partialsSet(os.DirFS(filepath.Join(r.dir, "templates")), ".")
		if err != nil {
			return err
		}
		return t.ExecuteTemplate(w, name, data)
	}
	return r.partials.ExecuteTemplate(w, name, data)
}

func (r *renderer) lookup(page string) (*template.Template, error) {
	if r.dir == "" {
		t, ok := r.cache[page]
		if !ok {
			return nil, fmt.Errorf("unknown page %q", page)
		}
		return t, nil
	}
	return parsePage(os.DirFS(filepath.Join(r.dir, "templates")), ".", page)
}

func (r *renderer) render(w io.Writer, page string, data any) error {
	t, err := r.lookup(page)
	if err != nil {
		return err
	}
	// Each page file ends with {{template "base" .}}, causing the template
	// named after the page file to invoke "base" which renders the full page.
	// ExecuteTemplate(w, page, data) produces complete HTML output.
	return t.ExecuteTemplate(w, page, data)
}

// staticHandler serves embedded assets at /_ui/static/. With dir!="" it serves
// from <dir>/static on disk instead.
func staticHandler(dir string) http.Handler {
	var fsys fs.FS
	if dir == "" {
		sub, err := fs.Sub(assetsFS, "static")
		if err != nil {
			panic("web: embed static sub: " + err.Error()) // impossible: compile-time embed
		}
		fsys = sub
	} else {
		fsys = os.DirFS(filepath.Join(dir, "static"))
	}
	return http.StripPrefix("/_ui/static/", http.FileServer(http.FS(fsys)))
}

// chromaCSSHandler serves the generated highlight stylesheet. With dir != ""
// a static/chroma.css file on disk overrides the generated one (theming hook).
func chromaCSSHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if dir != "" {
			if b, err := os.ReadFile(filepath.Join(dir, "static", "chroma.css")); err == nil {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
				_, _ = w.Write(b)
				return
			}
		}
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write(chromaCSS())
	}
}
