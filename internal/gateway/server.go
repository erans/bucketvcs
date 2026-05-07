// Package gateway implements the bucketvcs HTTP smart-Git server.
package gateway

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// Options configures a Server.
type Options struct {
	MirrorDir    string
	Version      string   // bucketvcs version string
	AuthMode     AuthMode // defined in auth.go (Task 13)
	AuthToken    string
	MaxBodyBytes int64
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
	if strings.ContainsAny(opts.Version, "\r\n\x00 ") {
		return nil, fmt.Errorf("gateway: Version must not contain whitespace or control characters")
	}
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 30 // 1 GiB
	}
	if opts.AuthMode != AuthAnonymous && opts.AuthToken == "" {
		return nil, fmt.Errorf("gateway: AuthToken must not be empty when AuthMode is not AuthAnonymous")
	}
	mgr, err := mirror.NewManager(opts.MirrorDir, store)
	if err != nil {
		return nil, fmt.Errorf("gateway: mirror manager: %w", err)
	}
	s := &Server{store: store, mgr: mgr, opts: opts}
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
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
