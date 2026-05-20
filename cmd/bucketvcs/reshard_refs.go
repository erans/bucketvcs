package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/bucketvcs/bucketvcs/internal/maintenance"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
)

const reshardRefsUsage = `usage: bucketvcs reshard-refs --store=<URL> --repo=<tenant>/<repo> [--actor=<u_*>]

Convert one repo from inline refs (v1) to sharded refs (v2).

This is a one-shot manual migration. After it succeeds, every push goes
through the sharded code path, and the root manifest no longer carries
the full ref list. Below ~10k refs, inline mode is faster — operators
should opt in only when scale warrants it.

The migration tolerates concurrent pushes but may fail with
"concurrent mutation" if a push wins the root CAS race; in that case
operators retry. Already-sharded repos are a no-op.

Flags:
  --store      Storage URL (e.g. localfs:/path, s3://bucket, gcs://bucket).
  --repo       Repo identifier in <tenant>/<repo> form.
  --actor      Principal recorded in the tx record. Defaults to u_op.
  --json       Emit the run report as JSON on stdout instead of a text summary.
`

func runReshardRefs(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reshard-refs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, reshardRefsUsage) }

	storeURL := fs.String("store", "", "")
	repoPath := fs.String("repo", "", "")
	actor := fs.String("actor", "u_op", "")
	asJSON := fs.Bool("json", false, "")
	helpShort := fs.Bool("h", false, "")
	helpLong := fs.Bool("help", false, "")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *helpShort || *helpLong {
		fmt.Fprint(stdout, reshardRefsUsage)
		return 0
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "reshard-refs: --store is required")
		return 2
	}
	if *repoPath == "" {
		fmt.Fprintln(stderr, "reshard-refs: --repo is required")
		return 2
	}
	tenant, repoID, err := splitTenantRepo(*repoPath)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: --repo=%q must be <tenant>/<repo>: %v\n", *repoPath, err)
		return 2
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: open store: %v\n", err)
		return 1
	}
	defer closeStore(store)

	k, err := keys.NewRepo(tenant, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: keys: %v\n", err)
		return 2
	}
	r, err := repo.Open(ctx, store, tenant, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "reshard-refs: open repo: %v\n", err)
		return 1
	}

	report, reshardErr := maintenance.Reshard(ctx, store, r, k, maintenance.ReshardOptions{Actor: *actor})

	if *asJSON {
		// JSON consumers get a structured payload on both success and
		// failure paths so scripted callers can branch on Outcome
		// rather than parsing stderr text. The error (if any) is also
		// included as a string field for diagnostic purposes.
		payload := struct {
			Outcome             string `json:"outcome"`
			RefCount            int    `json:"ref_count"`
			ShardCount          int    `json:"shard_count"`
			ManifestVersionFrom uint64 `json:"manifest_version_from"`
			ManifestVersionTo   uint64 `json:"manifest_version_to"`
			DurationMS          int64  `json:"duration_ms"`
			Error               string `json:"error,omitempty"`
		}{
			Outcome:             report.Outcome,
			RefCount:            report.RefCount,
			ShardCount:          report.ShardCount,
			ManifestVersionFrom: report.ManifestVersionFrom,
			ManifestVersionTo:   report.ManifestVersionTo,
			DurationMS:          report.DurationMS,
		}
		if reshardErr != nil {
			payload.Error = reshardErr.Error()
		}
		out, err := json.Marshal(payload)
		if err != nil {
			fmt.Fprintf(stderr, "reshard-refs: marshal json: %v\n", err)
			return 1
		}
		if _, err := fmt.Fprintf(stdout, "%s\n", out); err != nil {
			fmt.Fprintf(stderr, "reshard-refs: write json: %v\n", err)
			return 1
		}
		if reshardErr != nil {
			return 1
		}
		return 0
	}

	if reshardErr != nil {
		if errors.Is(reshardErr, maintenance.ErrConcurrentMutation) {
			fmt.Fprintf(stderr, "reshard-refs: aborted due to concurrent mutation; retry the command\n")
			return 1
		}
		fmt.Fprintf(stderr, "reshard-refs: %v\n", reshardErr)
		return 1
	}
	fmt.Fprintf(stdout, "reshard-refs %s/%s: %s (refs=%d shards=%d v%d→v%d %dms)\n",
		tenant, repoID, report.Outcome, report.RefCount, report.ShardCount,
		report.ManifestVersionFrom, report.ManifestVersionTo, report.DurationMS)
	return 0
}
