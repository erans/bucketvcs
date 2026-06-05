package main

import (
	"flag"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/authreplica"
)

// serveFlags carries every `bucketvcs serve` flag as the pointer returned by
// the flag package. Registered via registerServeFlags so `bucketvcs doctor`
// can accept the exact same command line ("swap serve for doctor") and
// validate the configuration without serving.
type serveFlags struct {
	addr            *string
	storeURL        *string
	mirrorDir       *string
	authDB          *string
	authDBMaxConns  *int
	maxBody         *int64
	shutdownTimeout *time.Duration

	// SSH flags.
	sshAddr    *string
	sshHostKey *string
	sshGrace   *time.Duration

	// Bundle/Pack URI delivery (M11).
	bundleURIMode    *string
	packURIMode      *string
	proxiedKeyFile   *string
	proxiedBundleTTL *time.Duration
	proxiedPackTTL   *time.Duration
	warmCommits      *int
	warmAge          *time.Duration
	proxiedBaseURL   *string

	// LFS (M13).
	lfsEnabled     *bool
	lfsPresignTTL  *time.Duration
	lfsSSHTokenTTL *time.Duration

	// M18 auth rate-limiting.
	authRateLimitBurst        *int
	authRateLimitRefillPerMin *float64
	trustProxyHeaders         *bool
	authRateLimitDisabled     *bool

	// M20 Tier 3 hooks.
	hooksEnabled                *bool
	hooksRoot                   *string
	hooksUnsafeNoSandbox        *bool
	hooksOnInternalError        *string
	hooksTimeoutSec             *int
	hooksCPUSec                 *int
	hooksMemoryMB               *int
	hooksOutputMaxKB            *int
	hooksAllowNetwork           *string
	hooksEnv                    *string
	hooksPostReceiveConcurrency *int
	hooksPostReceiveQueue       *int

	// M22 OIDC token-exchange.
	oidcEnabled       *bool
	oidcSweepInterval *time.Duration

	// Web UI (M24).
	uiEnabled       *bool
	uiAddr          *string
	uiDir           *string
	uiSessionTTL    *time.Duration
	uiBrowseTimeout *time.Duration

	// M24 Phase 1.5 — OIDC browser login (relying-party).
	oidcLogin      *bool
	oidcIssuer     *string
	oidcClientID   *string
	oidcSecretFile *string
	oidcRedirect   *string
	oidcScopes     *string
	oidcLabel      *string

	// M25 webhook egress policy (populated by repeatable fs.Func flags).
	webhookAllowCIDRs []netip.Prefix
	webhookDenyHosts  []string

	// M26 multi-region read replicas.
	replicaOf            *string
	replicaMode          *string
	replicaLagBudget     *time.Duration
	replicaCheckInterval *time.Duration
	writeRegionURL       *string

	// M27 BYOB (Bring Your Own Bucket).
	byobKeyFile  *string
	byobCredsTTL *time.Duration

	// M28 embedded authdb replication (Litestream).
	authDBReplica            *string
	authDBReplicaLeaseTTL    *time.Duration
	authDBReplicaSkipRestore *bool
}

// registerServeFlags registers the full serve flag surface on fs. The flag
// definitions here are MOVED VERBATIM from runServe — flag names, defaults,
// and help strings must not change (operator-visible surface).
func registerServeFlags(fs *flag.FlagSet) *serveFlags {
	sf := &serveFlags{}
	sf.addr = fs.String("addr", "", "HTTP listen address (host:port); leave empty to disable HTTP (default 127.0.0.1:8080 when --ssh-addr is also absent)")
	sf.storeURL = fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	sf.mirrorDir = fs.String("mirror-dir", "", "Mirror cache directory (default $XDG_CACHE_HOME/bucketvcs/mirrors)")
	sf.authDB = fs.String("auth-db", "", "Path to auth.db (default: $XDG_STATE_HOME/bucketvcs/bucketvcs.db)")
	sf.authDBMaxConns = fs.Int("auth-db-max-conns", 10, "Max DB connections for the auth/metadata DB (Postgres only; sqlite/libsql always use 1)")
	sf.maxBody = fs.Int64("max-body-bytes", 1<<30, "Per-request body limit in bytes")
	sf.shutdownTimeout = fs.Duration("shutdown-timeout", 30*time.Second, "Graceful shutdown deadline")

	// SSH flags.
	sf.sshAddr = fs.String("ssh-addr", "", "SSH listen address, e.g. 127.0.0.1:2222 (empty disables SSH)")
	sf.sshHostKey = fs.String("ssh-host-key", "", "Path to SSH host key (default $XDG_STATE_HOME/bucketvcs/ssh_host_ed25519_key)")
	sf.sshGrace = fs.Duration("ssh-grace", 10*time.Second, "Graceful shutdown deadline for in-flight SSH sessions")

	// Bundle/Pack URI delivery (M11). Defaults are off so existing
	// invocations continue to work; enabling either mode requires a
	// signing key + base URL when the mode is auto/proxied.
	sf.bundleURIMode = fs.String("bundle-uri-mode", "off", "Bundle URI delivery mode: auto|direct|proxied|off")
	sf.packURIMode = fs.String("pack-uri-mode", "off", "Pack URI delivery mode: auto|direct|proxied|off")
	sf.proxiedKeyFile = fs.String("proxied-url-signing-key", "", "Path to file containing >=16 byte HMAC key for gateway-proxied URLs (required when modes are auto or proxied)")
	sf.proxiedBundleTTL = fs.Duration("proxied-url-bundle-ttl", 4*time.Hour, "TTL for proxied/signed bundle URLs (kept long to cover initial-clone download windows)")
	sf.proxiedPackTTL = fs.Duration("proxied-url-pack-ttl", time.Hour, "TTL for proxied/signed pack URLs")
	sf.warmCommits = fs.Int("bundle-warm-commits", 5000, "Bundle freshness threshold: warm if behind by <= N commits")
	sf.warmAge = fs.Duration("bundle-warm-age", 24*time.Hour, "Bundle freshness threshold: warm if generated within D")
	sf.proxiedBaseURL = fs.String("proxied-url-base", "", "External base URL of this gateway, e.g. https://gw.example (required when modes are auto or proxied)")

	// LFS (M13). Default enabled; flip with --lfs=false.
	sf.lfsEnabled = fs.Bool("lfs", true, "Enable the LFS Batch API (M13)")
	sf.lfsPresignTTL = fs.Duration("lfs-presign-ttl", 15*time.Minute, "TTL for LFS upload/download presigned URLs")
	sf.lfsSSHTokenTTL = fs.Duration("lfs-ssh-token-ttl", 15*time.Minute, "TTL for bearers issued via SSH git-lfs-authenticate")

	// M18 auth rate-limiting. Defaults match ratelimit.DefaultConfig()
	// (Burst=10, RefillPerMinute=1, SweepInterval=5m). The limiter is
	// shared between the HTTPS gateway and the SSH server; --auth-rate-
	// limit-disabled skips construction entirely (nil Limiter is a no-op
	// on both transports).
	sf.authRateLimitBurst = fs.Int("auth-rate-limit-burst", 10,
		"Max credential failures before throttling per (IP, user)")
	sf.authRateLimitRefillPerMin = fs.Float64("auth-rate-limit-refill-per-minute", 1,
		"Failures cleared per minute when idle")
	sf.trustProxyHeaders = fs.Bool("trust-proxy-headers", false,
		"Honor the rightmost X-Forwarded-For hop as client IP. REQUIRED when "+
			"deployed behind a reverse proxy / load balancer — without this "+
			"flag every request appears to originate from the proxy IP and "+
			"a single attacker can fill the shared bucket and 429 every client.")
	sf.authRateLimitDisabled = fs.Bool("auth-rate-limit-disabled", false,
		"Disable auth rate-limiting entirely")

	// M25 webhook egress policy. The delivery worker denies loopback,
	// link-local (incl. cloud metadata), and private/ULA ranges by default;
	// these flags punch holes / add hostname deny patterns.
	fs.Func("webhook-allow-cidr",
		"CIDR webhook deliveries may reach despite the private-range deny default, e.g. 192.168.1.0/24 (repeatable; 0.0.0.0/0 restores the pre-M25 open behavior)",
		func(v string) error {
			p, err := netip.ParsePrefix(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("--webhook-allow-cidr: %w", err)
			}
			sf.webhookAllowCIDRs = append(sf.webhookAllowCIDRs, p)
			return nil
		})
	fs.Func("webhook-deny-host",
		"hostname glob webhook deliveries may never target: exact name or *.suffix, e.g. *.internal.example.com (repeatable; policy aid, not a security boundary — raw IPs bypass it)",
		func(v string) error {
			v = strings.TrimSpace(v)
			if v == "" || v == "*." || v == "*" {
				return fmt.Errorf("--webhook-deny-host: pattern must be an exact hostname or *.suffix, got %q", v)
			}
			sf.webhookDenyHosts = append(sf.webhookDenyHosts, v)
			return nil
		})

	// M20 Tier 3 hooks. Default disabled; flip with --hooks-enabled=true and
	// pass --hooks-root=<abs-dir>. On Linux the binary requires `bwrap` on
	// PATH unless --hooks-unsafe-no-sandbox=true is also set. On non-Linux
	// platforms --hooks-unsafe-no-sandbox=true is required (bwrap is Linux-
	// only); enabling unsafe mode logs an ERROR-level slog line at startup.
	sf.hooksEnabled = fs.Bool("hooks-enabled", false,
		"enable Tier 3 custom subprocess hooks (pre-receive + post-receive)")
	sf.hooksRoot = fs.String("hooks-root", "",
		"absolute directory containing hook script files (required when --hooks-enabled=true)")
	sf.hooksUnsafeNoSandbox = fs.Bool("hooks-unsafe-no-sandbox", false,
		"run hooks without bwrap namespace isolation. REQUIRED on macOS/non-Linux. NOT multi-tenant safe.")
	sf.hooksOnInternalError = fs.String("hooks-on-internal-error", "reject",
		"behavior when a hook subprocess fails for non-rejection reasons: reject | allow")
	sf.hooksTimeoutSec = fs.Int("hooks-timeout-sec", 30,
		"wall-clock timeout per hook subprocess")
	sf.hooksCPUSec = fs.Int("hooks-cpu-sec", 10,
		"RLIMIT_CPU per hook subprocess (sandbox mode only)")
	sf.hooksMemoryMB = fs.Int("hooks-memory-mb", 256,
		"RLIMIT_AS per hook subprocess in MiB (sandbox mode only)")
	sf.hooksOutputMaxKB = fs.Int("hooks-output-max-kb", 64,
		"stdout+stderr cap per hook (bytes beyond are dropped)")
	sf.hooksAllowNetwork = fs.String("hooks-allow-network", "",
		"comma-separated script_name list that gets --share-net; default empty (no network)")
	sf.hooksEnv = fs.String("hooks-env", "",
		"comma-separated KEY=VALUE list passed to every hook (each entry must contain '='; values may not contain ',')")
	sf.hooksPostReceiveConcurrency = fs.Int("hooks-postreceive-concurrency", 8,
		"worker pool size for post-receive hook execution")
	sf.hooksPostReceiveQueue = fs.Int("hooks-postreceive-queue", 256,
		"queue capacity for post-receive jobs; full queue drops with a metric")

	// M22 OIDC token-exchange. Default disabled; flip with --oidc=true.
	sf.oidcEnabled = fs.Bool("oidc", false,
		"Enable the OIDC token-exchange endpoint POST /_oidc/token (M22)")
	sf.oidcSweepInterval = fs.Duration("oidc-sweep-interval", 5*time.Minute,
		"Interval for sweeping expired OIDC-minted tokens")

	// Web UI (M24)
	sf.uiEnabled = fs.Bool("ui", true, "Enable the web UI (HTTP)")
	sf.uiAddr = fs.String("ui-addr", "", "Optional separate listen address for the web UI; empty shares --addr")
	sf.uiDir = fs.String("ui-dir", "", "Serve UI templates/static from this dir instead of the embedded assets (dev)")
	sf.uiSessionTTL = fs.Duration("ui-session-ttl", 168*time.Hour, "Web session lifetime (sliding)")
	sf.uiBrowseTimeout = fs.Duration("ui-browse-timeout", 20*time.Second,
		"Max wait for cold mirror materialization on a browse request before returning a 503 warming page")

	// M24 Phase 1.5 — OIDC browser login (relying-party)
	sf.oidcLogin = fs.Bool("oidc-login", false, "Enable OIDC browser login (relying-party)")
	sf.oidcIssuer = fs.String("oidc-login-issuer", "", "OIDC issuer URL, e.g. https://accounts.google.com")
	sf.oidcClientID = fs.String("oidc-login-client-id", "", "OAuth2 client id")
	sf.oidcSecretFile = fs.String("oidc-login-client-secret-file", "", "File with the OAuth2 client secret (or env BUCKETVCS_OIDC_LOGIN_CLIENT_SECRET)")
	sf.oidcRedirect = fs.String("oidc-login-redirect-url", "", "OAuth2 redirect URL, e.g. https://host/login/oidc/callback")
	sf.oidcScopes = fs.String("oidc-login-scopes", "openid,email,profile", "Comma-separated OIDC scopes")
	sf.oidcLabel = fs.String("oidc-login-label", "Single sign-on", "Login-page SSO button label")

	// M26 multi-region read replicas. Setting --replica-of activates
	// replica mode: this gateway serves reads from --store (the regional
	// bucket) with canonical fallback, and refuses all writes.
	sf.replicaOf = fs.String("replica-of", "",
		"Canonical (write-region) store URL; presence makes this a read-only regional replica gateway")
	sf.replicaMode = fs.String("replica-mode", "strong-current",
		"Replica freshness mode: strong-current (ref advertisement from the canonical bucket) | bounded-stale (regional manifest within --replica-lag-budget)")
	sf.replicaLagBudget = fs.Duration("replica-lag-budget", 5*time.Minute,
		"bounded-stale: max replication lag before this replica stops advertising refs (min 30s)")
	sf.replicaCheckInterval = fs.Duration("replica-check-interval", 0,
		"How often to compare regional vs canonical manifest versions per active repo (default: lag-budget/4, floor 15s)")
	sf.writeRegionURL = fs.String("write-region-url", "",
		"Write-region gateway URL included in push-refusal messages on replicas")

	// M27 BYOB (Bring Your Own Bucket).
	sf.byobKeyFile = fs.String("byob-encryption-key", "",
		"Path to file containing the 32-byte AES-256-GCM key for tenant credential encryption; required when any storage_bindings row exists")
	sf.byobCredsTTL = fs.Duration("byob-creds-ttl", time.Hour,
		"How long to cache an open per-tenant ObjectStore before re-reading the binding and re-opening")

	// M28 embedded authdb replication (Litestream). Default off; only the
	// primary (non-replica-serve) gateway with an embedded sqlite --auth-db
	// may replicate.
	sf.authDBReplica = fs.String("auth-db-replica", os.Getenv("BUCKETVCS_AUTH_DB_REPLICA"),
		`Replicate the sqlite authdb via embedded Litestream: "auto" (sys/authdb/ in --store), a storage URL, or "off" (default). Env: BUCKETVCS_AUTH_DB_REPLICA (flag wins)`)
	sf.authDBReplicaLeaseTTL = fs.Duration("auth-db-replica-lease-ttl", authreplica.DefaultLeaseTTL,
		"Lease validity window for the single-writer authdb replication lease (renewal runs every TTL/3)")
	sf.authDBReplicaSkipRestore = fs.Bool("auth-db-replica-skip-restore", false,
		"Skip restore-on-boot from the replica even when the local authdb file is missing (escape hatch; fail-open)")

	return sf
}
