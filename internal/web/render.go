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
	Tenant string
	Repo   string
	Ref    string // current ref display name
	OID    string // resolved OID; used by RefOrOID when Ref is empty
	Refs   browsemodel.Refs
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
	Path    string
	Entries []browsemodel.TreeEntry
}

type blobData struct {
	browseHeader
	Path string
	Blob browsemodel.Blob
	Code template.HTML // highlighted HTML; empty for binary/too-large
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
	dir   string
	cache map[string]*template.Template
}

func newRenderer(dir string) (*renderer, error) {
	r := &renderer{dir: dir}
	if dir == "" {
		r.cache = map[string]*template.Template{}
		for _, page := range []string{"landing.html", "login.html", "error.html", "repo.html", "tree.html", "blob.html", "commits.html", "commit.html"} {
			t, err := parsePage(assetsFS, "templates", page)
			if err != nil {
				return nil, err
			}
			r.cache[page] = t
		}
	}
	return r, nil
}

// parsePage parses base.html and the named page file from fsys. The dir
// argument is the path prefix within fsys (e.g. "templates" or ".").
// Each page file ends with {{template "base" .}}, which means executing
// the template named after the page file produces the full rendered page.
func parsePage(fsys fs.FS, dir, page string) (*template.Template, error) {
	base, pg := "base.html", page
	if dir != "" && dir != "." {
		base = dir + "/base.html"
		pg = dir + "/" + page
	}
	funcs := template.FuncMap{
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
		"diffclass": func(kind byte) string { return diffClass(kind) },
	}
	return template.New("").Funcs(funcs).ParseFS(fsys, base, pg)
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
