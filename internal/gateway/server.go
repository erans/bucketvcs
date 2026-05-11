// Package gateway implements the bucketvcs HTTP smart-Git server.
package gateway

import (
	"fmt"
	"net/http"
	"unicode"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures a Server.
type Options struct {
	MirrorDir    string
	Version      string
	AuthStore    auth.Store
	MaxBodyBytes int64

	// ProxiedURLSigningKey, when non-empty, enables the gateway-proxied
	// bundle/pack URL endpoints (/_bundle/<hash>, /_pack/<hash>). M11.
	// Must be at least 16 bytes when set (matches proxiedurl.Mint).
	ProxiedURLSigningKey []byte
	// ProxiedKeyResolver maps URL-path hashes to storage keys. REQUIRED
	// when ProxiedURLSigningKey is set; ignored otherwise.
	ProxiedKeyResolver ProxiedKeyResolver
}

// Server implements http.Handler.
type Server struct {
	store storage.ObjectStore
	mgr   *mirror.Manager
	opts  Options
	mux   *http.ServeMux
}

// NewServer constructs a Server. The mirror manager acquires a process flock
// on opts.MirrorDir; the caller must Close() the server on shutdown.
func NewServer(store storage.ObjectStore, opts Options) (*Server, error) {
	if opts.Version == "" {
		opts.Version = "0.0-dev"
	}
	for _, r := range opts.Version {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return nil, fmt.Errorf("gateway: Version must not contain whitespace or control characters")
		}
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 30 // 1 GiB
	}
	if opts.AuthStore == nil {
		return nil, fmt.Errorf("gateway: AuthStore is required")
	}
	if len(opts.ProxiedURLSigningKey) > 0 {
		if len(opts.ProxiedURLSigningKey) < 16 {
			return nil, fmt.Errorf("gateway: ProxiedURLSigningKey too short (%d bytes); need >= 16", len(opts.ProxiedURLSigningKey))
		}
		if opts.ProxiedKeyResolver == nil {
			return nil, fmt.Errorf("gateway: ProxiedKeyResolver required when ProxiedURLSigningKey is set")
		}
	}
	mgr, err := mirror.NewManager(opts.MirrorDir, store)
	if err != nil {
		return nil, fmt.Errorf("gateway: mirror manager: %w", err)
	}
	s := &Server{store: store, mgr: mgr, opts: opts}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	if len(opts.ProxiedURLSigningKey) > 0 {
		proxied := NewProxiedHandler(store, opts.ProxiedURLSigningKey, "/_bundle/", "/_pack/", opts.ProxiedKeyResolver)
		s.mux.Handle("/_bundle/", proxied)
		s.mux.Handle("/_pack/", proxied)
	}
	s.mux.HandleFunc("/", s.routeRoot)
	return s, nil
}

// Close releases the mirror manager's process flock.
func (s *Server) Close() error { return s.mgr.Close() }

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) routeRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = fmt.Fprintf(w, "bucketvcs %s\n", s.opts.Version)
		return
	}
	s.routeRepo(w, r)
}
