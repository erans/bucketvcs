package maintenance

import (
	"fmt"
	"log/slog"
	"time"
)

// Defaults for RunOptions / Thresholds. Exposed for tests and the CLI.
const (
	DefaultCASRetry     = 5
	DefaultRecentWindow = 24 * time.Hour
)

// Thresholds are the §15.3 force-repack triggers. A zero value disables
// that specific trigger; setting all to zero with !Force makes Run a
// no-op. Bitmap-coverage and lookup-latency triggers are intentionally
// omitted from M9 — they ship in their successor milestones.
type Thresholds struct {
	// RecentPackCount triggers when the count of canonical packs whose
	// object-store creation_time is within RecentWindow exceeds this.
	RecentPackCount int

	// TotalPackCount triggers when len(manifest.Packs) exceeds this.
	TotalPackCount int

	// ManifestPackBytes triggers when the JSON byte size of
	// manifest.Packs exceeds this.
	ManifestPackBytes int64

	// M10 §14.2 — reachability delta-chain compaction triggers.
	// 0 disables the specific check (matches M9 convention).
	// Defaults: 1000 commits / 100 pushes / 64 MiB.
	ReachabilityDeltaCommits int
	ReachabilityDeltaPushes  int
	ReachabilityDeltaBytes   int64

	// §16.3 — bundle regeneration triggers (M11). 0 disables the
	// specific check (matches M9/M10 convention). Defaults: 100 commits,
	// 24h.
	BundleCommits int
	BundleAge     time.Duration
}

// DefaultThresholds returns the spec §15.3 recommended values.
func DefaultThresholds() Thresholds {
	return Thresholds{
		RecentPackCount:   1000,
		TotalPackCount:    10000,
		ManifestPackBytes: 8 << 20, // 8 MiB

		ReachabilityDeltaCommits: 1000,
		ReachabilityDeltaPushes:  100,
		ReachabilityDeltaBytes:   64 * 1024 * 1024,

		BundleCommits: 100,
		BundleAge:     24 * time.Hour,
	}
}

// RunOptions configures one Run invocation against one repo.
type RunOptions struct {
	Thresholds   Thresholds
	RecentWindow time.Duration // window for "recent" pack classification
	CASRetry     int           // bound on Phase 6 CAS-merge retries
	Force        bool          // skip threshold evaluation; always proceed
	// DryRun: walk + plan + report; no manifest commit is written.
	//
	// Side-effect asymmetry under BundleOnly: the bundle-refresh phase
	// uploads the bundle artifact + sidecar to content-addressed
	// storage BEFORE it considers whether to CAS-merge. Under
	// DryRun+BundleOnly the uploads still happen (since the artifact
	// is needed to construct the BundleEntry that the dry-run is
	// previewing), but the manifest is not modified. Those bundle
	// blobs are unreferenced from the manifest and are GC'd by M8 on
	// the next sweep. Operators who require a true zero-side-effect
	// probe should not combine DryRun with BundleOnly.
	DryRun       bool
	Actor        string        // tx record actor; "u_op" if empty
	Logger       *slog.Logger  // defaults to slog.Default()
	Now          func() time.Time

	// BundleOnly skips repack + compact phases; only the bundle-refresh
	// phase runs. Mutually exclusive with NoBundle.
	BundleOnly bool

	// NoBundle skips the bundle-refresh phase. Repack and compact
	// proceed as configured. Mutually exclusive with BundleOnly.
	NoBundle bool

	// BundleDefaultBranch overrides the auto-detected default branch
	// for bundle generation. Empty means use HEAD's resolution from
	// the manifest. Format: "refs/heads/<name>".
	BundleDefaultBranch string

	// BetweenRepackAndCAS is a test hook invoked inside the first
	// buildBody callback of Phase 6 CAS-merge, before the merged body
	// is constructed. The hook fires exactly once per Run (gated by an
	// internal flag in pipeline.go) so it triggers a CAS retry on the
	// first attempt only. It fires for both the repack path and the
	// compact-only CAS callback. Production callers leave it nil.
	BetweenRepackAndCAS func() `json:"-"`
}

// Normalize fills in defaults for unset fields. Idempotent. Sub-hour
// RecentWindow values set by the caller are preserved here so Validate
// can reject them with a clear message.
func (o *RunOptions) Normalize() {
	if o.Thresholds == (Thresholds{}) {
		o.Thresholds = DefaultThresholds()
	}
	if o.CASRetry <= 0 {
		o.CASRetry = DefaultCASRetry
	}
	if o.RecentWindow <= 0 {
		o.RecentWindow = DefaultRecentWindow
	}
	if o.Actor == "" {
		o.Actor = "u_op"
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
}

// Validate returns an error wrapped in ErrInvalidFlags if RunOptions is
// inconsistent. Call after Normalize. RecentWindow is the only field
// where a caller-supplied non-zero value can survive Normalize and
// reach Validate; CASRetry is always bumped to DefaultCASRetry by
// Normalize, so no check is needed here.
func (o RunOptions) Validate() error {
	if o.RecentWindow < time.Hour {
		return fmt.Errorf("%w: RecentWindow=%s is below the 1h minimum",
			ErrInvalidFlags, o.RecentWindow)
	}
	if o.BundleOnly && o.NoBundle {
		return fmt.Errorf("%w: --bundle-only and --no-bundle are mutually exclusive",
			ErrInvalidFlags)
	}
	return nil
}

// ReachabilityCompactionReport summarises the M10 compact-only phase.
type ReachabilityCompactionReport struct {
	Triggered     bool   `json:"triggered,omitempty"`
	TriggerReason string `json:"trigger_reason,omitempty"`
	DeltasDropped int    `json:"deltas_dropped,omitempty"`
	BaseSwapped   bool   `json:"base_swapped,omitempty"`
}

// BundleResult records the outcome of the bundle-refresh phase. Nil
// when the phase did not run (NoBundle, no triggers fired in non-Force
// mode, or the maintenance run failed before reaching the phase).
type BundleResult struct {
	// Generated is true if a fresh bundle was uploaded and CAS-merged.
	// False indicates the phase ran but decided no regeneration was
	// needed (Generated=false, TriggerReason="no_trigger").
	Generated bool `json:"generated"`

	// BundleID is the new BundleEntry.ID. Empty when !Generated.
	BundleID string `json:"bundle_id,omitempty"`

	// BundleHash is the SHA-256 of the bundle file body. Empty when !Generated.
	BundleHash string `json:"bundle_hash,omitempty"`

	// CoversManifestVersion is the M_now version captured at generation start.
	CoversManifestVersion uint64 `json:"covers_manifest_version,omitempty"`

	// ByteSize is the bundle file size. Zero when !Generated.
	ByteSize int64 `json:"byte_size,omitempty"`

	// DurationMS is the wall time of the phase.
	DurationMS int64 `json:"duration_ms,omitempty"`

	// TriggerReason is one of: "missing", "age", "commits", "force",
	// "no_trigger", "skipped_no_default_branch",
	// "skipped_reachability_load_error", "skipped_trigger_eval_error".
	TriggerReason string `json:"trigger_reason,omitempty"`

	// ErrorMessage is non-empty if the phase failed and the rest of
	// the maintenance run continued. Failure does not set Generated.
	ErrorMessage string `json:"error_message,omitempty"`
}

// Report summarizes one Run for the caller (CLI, future scheduler).
type Report struct {
	RepoID            string        `json:"repo_id"`
	Outcome           string        `json:"outcome"` // success|noop|failed_*
	DryRun            bool          `json:"dry_run"`
	ManifestVersionAt uint64        `json:"manifest_version_at_start"`
	ManifestVersionTo uint64        `json:"manifest_version_after,omitempty"`
	TriggerEval       TriggerReport `json:"trigger_eval"`
	BeforePackCount   int           `json:"before_pack_count"`
	AfterPackCount    int           `json:"after_pack_count"`
	BeforeManifestPB  int64         `json:"before_manifest_pack_bytes"`
	AfterManifestPB   int64         `json:"after_manifest_pack_bytes"`
	NewPackKey        string        `json:"new_pack_key,omitempty"`
	NewPackObjects    int           `json:"new_pack_objects,omitempty"`
	NewPackBytes      int64         `json:"new_pack_bytes,omitempty"`
	NewObjectMapKey   string        `json:"new_object_map_key,omitempty"`
	NewCommitGraphKey string        `json:"new_commit_graph_key,omitempty"`
	RepackedPackKeys  []string      `json:"repacked_pack_keys"`
	CASAttempts       int           `json:"cas_attempts"`
	DurationMS        int64         `json:"duration_ms"`

	// M10 reachability compaction detail.
	ReachabilityCompaction ReachabilityCompactionReport `json:"reachability_compaction,omitempty"`

	// M11 bundle-refresh phase detail. Nil when the phase did not run.
	BundleResult *BundleResult `json:"bundle_result,omitempty"`
}

// TriggerReport records what Phase 0 saw, regardless of outcome.
type TriggerReport struct {
	Triggered         bool       `json:"triggered"`
	Reason            string     `json:"reason,omitempty"` // first trigger that fired
	RecentPackCount   int        `json:"recent_pack_count"`
	TotalPackCount    int        `json:"total_pack_count"`
	ManifestPackBytes int64      `json:"manifest_pack_bytes"`
	Thresholds        Thresholds `json:"thresholds"`

	// M10 reachability compaction trigger (separate from repack).
	CompactReachability       bool   `json:"compact_reachability,omitempty"`
	CompactReachabilityReason string `json:"compact_reachability_reason,omitempty"`
}
