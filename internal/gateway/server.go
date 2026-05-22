// Package gateway implements the bucketvcs HTTP smart-Git server.
package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
	"unicode"

	"github.com/bucketvcs/bucketvcs/internal/auth"
	"github.com/bucketvcs/bucketvcs/internal/lfs"
	"github.com/bucketvcs/bucketvcs/internal/lfs/locks"
	"github.com/bucketvcs/bucketvcs/internal/mirror"
	"github.com/bucketvcs/bucketvcs/internal/policy"
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

	// BundleURIEnabled advertises the bundle-uri capability and serves
	// command=bundle-uri. Default false: clients fall through to standard fetch.
	BundleURIEnabled bool

	// BundleWarmCommits caps the commits-behind window for "warm" bundles.
	// Defaults to 5000 when BundleURIEnabled is true.
	BundleWarmCommits int

	// BundleWarmAge caps the age window for "warm" bundles. Defaults to 24h
	// when BundleURIEnabled is true.
	BundleWarmAge time.Duration

	// BundleURIMode controls how bundle URLs are minted: direct (signed),
	// proxied (HMAC gateway URL), auto (try direct, fall back to proxied),
	// or off. The zero value is URIModeAuto, so leaving this unset selects
	// auto. Ignored when BundleURIEnabled is false. Ignored when
	// BundleURIBuildURL is provided (legacy URLBuilder path only).
	//
	// Note on URIModeAuto / URIModeDirect: NewServer cannot detect at
	// startup whether the storage backend supports SignedGetURL — that
	// capability is reported per-call via storage.ErrNotSupported. When
	// the backend doesn't support signing AND no proxied fallback is
	// configured (URIModeAuto with no ProxiedURLSigningKey/ProxiedBaseURL,
	// or URIModeDirect at all), every bundle-uri request silently falls
	// back to an empty advertisement and the client falls through to
	// fetch. NewServer emits a startup warning via slog.Default() when
	// this configuration is detected; operators should either configure
	// proxied fallback or pair URIModeDirect/Auto only with signed-URL-
	// capable backends (S3, GCS, AzureBlob). Use URIModeProxied for
	// localfs / dev backends.
	BundleURIMode URIMode

	// BundleURIBuildURL, when non-nil, mints the URL advertised in
	// command=bundle-uri responses. Required when BundleURIEnabled is
	// true. Constructed by the operator (typically from a URLBuilder)
	// rather than internally so the same closure can be shared with the
	// SSH listener (sshd.Options.BundleURIBuildURL).
	BundleURIBuildURL func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)

	// BundleURITTL is the URL lifetime applied to bundle URLs only; see
	// PackURITTL for pack URLs. Defaults to 5 minutes when
	// BundleURIEnabled is true.
	//
	// Deprecated path: this field is retained for backward compatibility
	// with operators that still want gateway to construct its own
	// URLBuilder via the (BundleURIMode, ProxiedURLSigningKey,
	// ProxiedBaseURL) triple. New code should pass BundleURIBuildURL
	// directly.
	BundleURITTL time.Duration

	// ProxiedBaseURL is the absolute base URL the gateway is reachable at
	// (e.g. "https://gw.example.com"); required when bundle/pack URI
	// mode is "proxied" or "auto" and the storage backend doesn't support
	// direct signed URLs. Ignored when both BundleURIEnabled and
	// PackURIEnabled are false. Only consulted by the legacy URLBuilder
	// construction path; ignored when BundleURIBuildURL is provided.
	ProxiedBaseURL string

	// PackURIEnabled advertises the packfile-uris capability and emits
	// the in-fetch packfile-uris section. Mirrors BundleURIEnabled but
	// for packs (Git protocol-v2 packfile-uris).
	PackURIEnabled bool

	// PackURIBuildURL, when non-nil, mints pack URLs for the in-fetch
	// packfile-uris response. Required when PackURIEnabled is true.
	PackURIBuildURL func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)

	// PackURIMode controls how pack URLs are minted by the legacy internal
	// URLBuilder. Ignored when PackURIBuildURL is provided.
	PackURIMode URIMode

	// PackURITTL is the URL lifetime applied to pack URLs constructed by
	// the legacy internal URLBuilder. Ignored when PackURIBuildURL is
	// provided.
	PackURITTL time.Duration

	// LFSEnabled enables the M13 Git LFS Batch API. Default-on at the
	// CLI layer (cmd/bucketvcs/serve.go); zero-value here means
	// disabled (the CLI flips it explicitly when --lfs=true, which is
	// the default).
	LFSEnabled bool

	// LFSPresignTTL is the TTL passed into the LFS Store's
	// PresignPut/PresignGet calls. Zero -> 15 minutes.
	LFSPresignTTL time.Duration

	// LFSProxiedURLSigningKey, when non-empty (>= 16 bytes), enables the
	// gateway-proxied LFS transfer path for backends without native
	// presigned URLs (e.g. localfs). The same key signs and verifies
	// /_lfs/ tokens. Independent from ProxiedURLSigningKey (which gates
	// /_bundle/ + /_pack/); operators may set one without the other.
	LFSProxiedURLSigningKey []byte

	// LFSProxiedBaseURL is the external base URL of this gateway used
	// when minting /_lfs/ URLs. Required when LFSProxiedURLSigningKey is
	// set.
	LFSProxiedBaseURL string

	// LFSLocksStore enables the LFS Locks API endpoints (M13.3). When
	// nil, lock requests return 503. When non-nil, the four lock routes
	// (POST/GET /info/lfs/locks, POST /info/lfs/locks/verify, POST
	// /info/lfs/locks/<id>/unlock) are dispatched to the LFS handler
	// with this store attached. Ignored when LFSEnabled is false.
	LFSLocksStore *locks.Store

	// Policy enables M14 protected-refs enforcement in receive-pack
	// step 8b. When nil, ref updates are accepted as in pre-M14
	// deployments. When non-nil, CheckUpdate runs for every ref in the
	// command list and blocks updates that match a protected_refs rule.
	Policy *policy.Service

	// Logger is used for structured metric + audit emission. When nil, the
	// gateway falls back to slog.Default(). M11 Phase 12.5 adds this for
	// gateway-side observability; before that the gateway only used slog.Default()
	// ad-hoc for startup warnings.
	Logger *slog.Logger
}

// Server implements http.Handler.
type Server struct {
	store             storage.ObjectStore
	mgr               *mirror.Manager
	opts              Options
	logger            *slog.Logger
	mux               *http.ServeMux
	urlBuilder        *URLBuilder
	bundleURIBuildURL func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)
	packURLBuilder    *URLBuilder
	packURIBuildURL   func(ctx context.Context, hash, storageKey, expectedHash string) (string, error)
	lfsHandler        http.Handler
	lfsObjectHandler  http.Handler
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
	if len(opts.LFSProxiedURLSigningKey) > 0 {
		if len(opts.LFSProxiedURLSigningKey) < 16 {
			return nil, fmt.Errorf("gateway: LFSProxiedURLSigningKey too short (%d bytes); need >= 16", len(opts.LFSProxiedURLSigningKey))
		}
		if opts.LFSProxiedBaseURL == "" {
			return nil, fmt.Errorf("gateway: LFSProxiedBaseURL required when LFSProxiedURLSigningKey is set")
		}
	}
	mgr, err := mirror.NewManager(opts.MirrorDir, store)
	if err != nil {
		return nil, fmt.Errorf("gateway: mirror manager: %w", err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	s := &Server{store: store, mgr: mgr, opts: opts, logger: opts.Logger}

	// Apply BundleURI defaults and construct the URLBuilder once at startup.
	if opts.BundleURIEnabled {
		// Closure-supplied path (preferred). When the operator provides
		// BundleURIBuildURL directly, gateway does no URL minting itself;
		// the warm-window defaults still apply and the closure is wired
		// straight into the engine request.
		if opts.BundleURIBuildURL != nil {
			if opts.BundleWarmCommits == 0 {
				opts.BundleWarmCommits = 5000
			}
			if opts.BundleWarmAge == 0 {
				opts.BundleWarmAge = 24 * time.Hour
			}
			s.opts = opts
			s.bundleURIBuildURL = opts.BundleURIBuildURL
		} else {
			// Legacy path: gateway constructs URLBuilder from
			// (BundleURIMode, ProxiedURLSigningKey, ProxiedBaseURL).
			// Reject configurations where URL minting cannot succeed.
			// URIModeOff with BundleURIEnabled=true is contradictory; URIModeProxied
			// requires both pieces of proxied-URL configuration.
			switch opts.BundleURIMode {
			case URIModeOff:
				return nil, fmt.Errorf("gateway: BundleURIEnabled with BundleURIMode=off is contradictory")
			case URIModeProxied:
				if len(opts.ProxiedURLSigningKey) == 0 || opts.ProxiedBaseURL == "" {
					return nil, fmt.Errorf("gateway: BundleURIMode=proxied requires ProxiedURLSigningKey and ProxiedBaseURL")
				}
			case URIModeAuto, URIModeDirect:
				// Auto/Direct depend on storage signed-URL support; we can't
				// probe that without a sentinel SignedGetURL call against a
				// real key. Emit a startup warning when proxied fallback isn't
				// available either, so operators see the silent-degradation
				// risk in logs instead of debugging an "empty advertisement
				// every request" mystery in production.
				if len(opts.ProxiedURLSigningKey) == 0 || opts.ProxiedBaseURL == "" {
					slog.Default().Warn("gateway: BundleURIEnabled with no proxied fallback configured; bundle-uri will silently no-op on backends that lack SignedGetURL support",
						"mode", opts.BundleURIMode.String())
				}
			default:
				return nil, fmt.Errorf("gateway: unrecognized BundleURIMode %v", opts.BundleURIMode)
			}
			// URIModeAuto and URIModeDirect tolerate either signed-URL-capable
			// storage OR proxied fallback (Auto only); the URLBuilder reports
			// a runtime error per-request if neither path works for a given
			// backend, and HandleBundleURI degrades to an empty response.
			if opts.BundleWarmCommits == 0 {
				opts.BundleWarmCommits = 5000
			}
			if opts.BundleWarmAge == 0 {
				opts.BundleWarmAge = 24 * time.Hour
			}
			// BundleURIMode's zero value happens to be URIModeAuto today; no
			// explicit defaulting is required. Documented in the Options
			// field comment.
			if opts.BundleURITTL == 0 {
				opts.BundleURITTL = 5 * time.Minute
			}
			s.opts = opts
			s.urlBuilder = &URLBuilder{
				Store:          store,
				ProxiedKey:     opts.ProxiedURLSigningKey,
				ProxiedBaseURL: opts.ProxiedBaseURL,
				BundleTTL:      opts.BundleURITTL,
				Mode:           opts.BundleURIMode,
			}
			// Pre-build the adapter closure once at startup so the hot fetch
			// path doesn't allocate one per request. The closure drops the
			// "via" return from URLBuilder.BuildBundleURL — direct-vs-proxied
			// selection is still observable from the underlying SignedGetURL /
			// ProxiedHandler counters; we don't bubble it up through v2proto.
			ub := s.urlBuilder
			s.bundleURIBuildURL = func(ctx context.Context, hash, storageKey, expectedHash string) (string, error) {
				url, _, err := ub.BuildBundleURL(ctx, hash, storageKey, expectedHash)
				return url, err
			}
		}
	}

	// Apply PackURI defaults. Closure-supplied path is preferred, mirroring
	// BundleURI above.
	if opts.PackURIEnabled {
		if opts.PackURIBuildURL != nil {
			s.packURIBuildURL = opts.PackURIBuildURL
		} else {
			switch opts.PackURIMode {
			case URIModeOff:
				return nil, fmt.Errorf("gateway: PackURIEnabled with PackURIMode=off is contradictory")
			case URIModeProxied:
				if len(opts.ProxiedURLSigningKey) == 0 || opts.ProxiedBaseURL == "" {
					return nil, fmt.Errorf("gateway: PackURIMode=proxied requires ProxiedURLSigningKey and ProxiedBaseURL")
				}
			case URIModeAuto, URIModeDirect:
				if len(opts.ProxiedURLSigningKey) == 0 || opts.ProxiedBaseURL == "" {
					slog.Default().Warn("gateway: PackURIEnabled with no proxied fallback configured; packfile-uris will silently no-op on backends that lack SignedGetURL support",
						"mode", opts.PackURIMode.String())
				}
			default:
				return nil, fmt.Errorf("gateway: unrecognized PackURIMode %v", opts.PackURIMode)
			}
			if opts.PackURITTL == 0 {
				opts.PackURITTL = time.Hour
			}
			s.opts = opts
			s.packURLBuilder = &URLBuilder{
				Store:          store,
				ProxiedKey:     opts.ProxiedURLSigningKey,
				ProxiedBaseURL: opts.ProxiedBaseURL,
				PackTTL:        opts.PackURITTL,
				Mode:           opts.PackURIMode,
			}
			pub := s.packURLBuilder
			s.packURIBuildURL = func(ctx context.Context, hash, storageKey, expectedHash string) (string, error) {
				url, _, err := pub.BuildPackURL(ctx, hash, storageKey, expectedHash)
				return url, err
			}
		}
	}

	s.mux = http.NewServeMux()
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	if len(opts.ProxiedURLSigningKey) > 0 {
		proxied := NewProxiedHandler(store, opts.ProxiedURLSigningKey, "/_bundle/", "/_pack/", opts.ProxiedKeyResolver, s.logger)
		s.mux.Handle("/_bundle/", proxied)
		s.mux.Handle("/_pack/", proxied)
	}
	// LFS handler wiring (M13 P1). Constructed once at startup; the
	// route dispatcher in routeRepo calls it for OpLFSBatch.
	if opts.LFSEnabled {
		ttl := opts.LFSPresignTTL
		if ttl <= 0 {
			ttl = 15 * time.Minute
		}
		// Wire WithProxied so localfs (or any backend lacking native
		// SignedURLs) gets a real proxied URL minted by lfs.Store.
		// Cloud backends with SignedURLs return real signed URLs via
		// Store.PresignPut/PresignGet and never reach the proxied path.
		// LFSProxied* are independent from the gateway-wide
		// ProxiedURLSigningKey/ProxiedBaseURL (which gate /_bundle/ +
		// /_pack/). Operators must set the LFS pair explicitly when
		// they want proxied LFS transfer; the CLI does this.
		proxiedKey := opts.LFSProxiedURLSigningKey
		proxiedBase := opts.LFSProxiedBaseURL
		s.lfsHandler = lfs.NewHTTPHandler(lfs.Deps{
			AuthStore:        opts.AuthStore,
			ActorFromContext: ActorFromContext,
			NewStore: func(tenant, repo string) *lfs.Store {
				ls := lfs.NewStore(store, lfs.RepoLFSPrefix(tenant, repo))
				if len(proxiedKey) >= 16 && proxiedBase != "" {
					ls = ls.WithProxied(proxiedKey, proxiedBase, tenant, repo)
				}
				return ls
			},
			PresignTTL: ttl,
			Logger:     opts.Logger,
			LocksStore: opts.LFSLocksStore,
		})

		// Mount the proxied object handler at /_lfs/ when proxied URL
		// signing is configured. Without a signing key we cannot verify
		// tokens, so the handler is omitted and ProxiedPutURL/GetURL above
		// returns empty URLs (which Build then surfaces as per-object 503).
		if len(proxiedKey) >= 16 {
			s.lfsObjectHandler = lfs.NewProxiedObjectHandler(lfs.ProxiedDeps{
				Store:  store,
				Key:    proxiedKey,
				Logger: opts.Logger,
			})
		}
	}
	if s.lfsObjectHandler != nil {
		s.mux.Handle("/_lfs/", s.lfsObjectHandler)
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
