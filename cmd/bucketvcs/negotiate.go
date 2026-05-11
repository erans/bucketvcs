package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/gitproto/uploadpack"
	"github.com/bucketvcs/bucketvcs/internal/pack"
	"github.com/bucketvcs/bucketvcs/internal/reachability"
	"github.com/bucketvcs/bucketvcs/internal/repo"
	"github.com/bucketvcs/bucketvcs/internal/repo/keys"
	"github.com/bucketvcs/bucketvcs/internal/repo/manifest"
)

const negotiateUsage = `Usage: bucketvcs negotiate --store=<URL> --repo=<tenant>/<repo> --wants=<oid>[,...] [flags]

Debug tool: compute the shipping plan (set of commits to send) for a
hypothetical fetch negotiation against a bucketvcs repo using the
pure-Go reachability index. Does not materialise or touch the mirror.

Flags:
  --store=<URL>             Storage URL (required)
  --repo=<tenant>/<repo>    Repo identifier (required)
  --wants=<oid>[,...]       Comma-separated list of want OIDs (required)
  --haves=<oid>[,...]       Comma-separated list of have OIDs (optional)
  --output=text|json        Output format (default text)
  --help                    Show this help

Exit codes:
  0  success
  1  operational error (store, repo, reachability index)
  2  usage / flag error
  3  unknown want OID (client asked for a commit not in the index)
`

func runNegotiate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("negotiate", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, negotiateUsage) }

	storeURL := fs.String("store", "", "Storage URL (required)")
	repoFlag := fs.String("repo", "", "<tenant>/<repo> (required)")
	wantsFlag := fs.String("wants", "", "Comma-separated want OIDs (required)")
	havesFlag := fs.String("haves", "", "Comma-separated have OIDs (optional)")
	output := fs.String("output", "text", "text|json")
	help := fs.Bool("help", false, "Show this help")
	fs.BoolVar(help, "h", false, "alias for --help")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *help {
		fmt.Fprint(stdout, negotiateUsage)
		return 0
	}

	if *storeURL == "" {
		fmt.Fprintln(stderr, "negotiate: --store is required")
		return 2
	}
	if *repoFlag == "" {
		fmt.Fprintln(stderr, "negotiate: --repo is required")
		return 2
	}
	if *wantsFlag == "" {
		fmt.Fprintln(stderr, "negotiate: --wants is required")
		return 2
	}
	if *output != "text" && *output != "json" {
		fmt.Fprintf(stderr, "negotiate: --output must be text or json (got %q)\n", *output)
		return 2
	}

	tenantID, repoID, err := splitTenantRepo(*repoFlag)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: %v\n", err)
		return 2
	}

	wants, err := parseOIDList(*wantsFlag)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: --wants: %v\n", err)
		return 2
	}
	if *wantsFlag != "" && len(wants) == 0 {
		fmt.Fprintln(stderr, "negotiate: --wants resolved to zero OIDs")
		return 2
	}
	var haves []pack.OID
	if *havesFlag != "" {
		haves, err = parseOIDList(*havesFlag)
		if err != nil {
			fmt.Fprintf(stderr, "negotiate: --haves: %v\n", err)
			return 2
		}
	}

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: open store: %v\n", err)
		return 1
	}
	defer closeStore(store)

	r, err := repo.Open(ctx, store, tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: open repo: %v\n", err)
		return 1
	}

	view, err := r.ReadRoot(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: read manifest: %v\n", err)
		return 1
	}

	var body manifest.Body
	if err := json.Unmarshal(view.Body, &body); err != nil {
		fmt.Fprintf(stderr, "negotiate: parse manifest: %v\n", err)
		return 1
	}

	k, err := keys.NewRepo(tenantID, repoID)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: keys: %v\n", err)
		return 1
	}

	set, err := reachability.Load(ctx, store, k, body)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: load reachability index: %v\n", err)
		return 1
	}

	plan, err := uploadpack.Negotiate(ctx, set, uploadpack.NegotiateInput{
		Wants: wants,
		Haves: haves,
		Done:  true,
	})
	if err != nil {
		if errors.Is(err, uploadpack.ErrUnknownWant) {
			fmt.Fprintf(stderr, "negotiate: %v\n", err)
			return 3
		}
		fmt.Fprintf(stderr, "negotiate: %v\n", err)
		return 1
	}

	code, err := emitNegotiateResult(stdout, *output, plan)
	if err != nil {
		fmt.Fprintf(stderr, "negotiate: %v\n", err)
	}
	return code
}

// emitNegotiateResult writes the shipping plan to stdout in the requested format.
// Returns an error if JSON encoding fails; callers should write it to stderr.
func emitNegotiateResult(w io.Writer, format string, plan uploadpack.ShippingPlan) (int, error) {
	switch format {
	case "json":
		commits := make([]string, len(plan.Commits))
		for i, c := range plan.Commits {
			commits[i] = c.String()
		}
		refs := make(map[string]string, len(plan.Refs))
		for name, oid := range plan.Refs {
			refs[name] = oid.String()
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"commits": commits,
			"refs":    refs,
		}); err != nil {
			return 1, err
		}
	default:
		fmt.Fprintf(w, "Shipping plan: %d commit(s)\n", len(plan.Commits))
		for _, c := range plan.Commits {
			fmt.Fprintf(w, "  %s\n", c.String())
		}
		if len(plan.Refs) > 0 {
			fmt.Fprintf(w, "Refs:\n")
			for name, oid := range plan.Refs {
				fmt.Fprintf(w, "  %s -> %s\n", name, oid.String())
			}
		}
	}
	return 0, nil
}

// parseOIDList parses a comma-separated list of hex OID strings.
// Empty parts (including all-whitespace) are skipped. If every part is
// empty after trimming, (nil, nil) is returned to match the semantics of
// an unset flag (zero haves / zero wants).
func parseOIDList(s string) ([]pack.OID, error) {
	parts := strings.Split(s, ",")
	out := make([]pack.OID, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		o, err := pack.ParseOID(p)
		if err != nil {
			return nil, fmt.Errorf("parse OID %q: %w", p, err)
		}
		out = append(out, o)
	}
	// Treat all-empty as zero haves (matches empty-flag semantics).
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
