package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/bucketvcs/bucketvcs/internal/repo"
)

// classifyOpenErr returns the canonical exit code (2/3/1) and writes a
// uniform message for errors from repo.Open or repo.Repo.ReadRoot in
// inspect-manifest. Returns 0 if err == nil.
func classifyOpenErr(err error, tenantID, repoID string, stderr io.Writer) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, repo.ErrRepoNotFound):
		fmt.Fprintf(stderr, "inspect-manifest: repo %s/%s not found\n", tenantID, repoID)
		return 2
	case errors.Is(err, repo.ErrUnsupportedSchema):
		fmt.Fprintln(stderr, "inspect-manifest:", err)
		return 3
	default:
		fmt.Fprintln(stderr, "inspect-manifest:", err)
		return 1
	}
}

// runInspect is the body of `bucketvcs inspect-manifest`.
func runInspect(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("inspect-manifest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("store", "", `Store URL (e.g. "localfs:/var/lib/bucketvcs")`)
	asJSON := fs.Bool("json", false, "Print the raw root manifest as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *storeURL == "" {
		fmt.Fprintln(stderr, "inspect-manifest: --store is required")
		return 2
	}
	pos := fs.Args()
	if len(pos) != 2 {
		fmt.Fprintf(stderr, "inspect-manifest: want exactly 2 positional args (tenant repo), got %d\n", len(pos))
		return 2
	}
	tenantID, repoID := pos[0], pos[1]

	store, err := openStore(*storeURL)
	if err != nil {
		fmt.Fprintln(stderr, "inspect-manifest:", err)
		return 1
	}
	defer closeStore(store)

	r, err := repo.Open(ctx, store, tenantID, repoID)
	if code := classifyOpenErr(err, tenantID, repoID, stderr); code != 0 {
		return code
	}
	view, err := r.ReadRoot(ctx)
	if code := classifyOpenErr(err, tenantID, repoID, stderr); code != 0 {
		return code
	}

	if *asJSON {
		var bodyMap map[string]json.RawMessage
		if err := json.Unmarshal(view.Body, &bodyMap); err != nil {
			fmt.Fprintln(stderr, "inspect-manifest: parse body:", err)
			return 1
		}
		headerJSON, _ := json.Marshal(view.Header)
		var headerMap map[string]json.RawMessage
		_ = json.Unmarshal(headerJSON, &headerMap)
		for k, v := range headerMap {
			bodyMap[k] = v
		}
		out, _ := json.MarshalIndent(bodyMap, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return 0
	}

	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "schema_version\t%d\n", view.Header.SchemaVersion)
	fmt.Fprintf(w, "min_reader_version\t%s\n", view.Header.MinReaderVersion)
	fmt.Fprintf(w, "repo_id\t%s\n", view.Header.RepoID)
	fmt.Fprintf(w, "object_format\t%s\n", view.Header.RepoFormat.ObjectFormat)
	fmt.Fprintf(w, "manifest_version\t%d\n", view.Header.ManifestVersion)
	fmt.Fprintf(w, "latest_tx\t%s\n", view.Header.LatestTx)
	fmt.Fprintf(w, "created_at\t%s\n", view.Header.CreatedAt.Format("2006-01-02T15:04:05Z07:00"))
	fmt.Fprintf(w, "updated_at\t%s\n", view.Header.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))

	counts, _ := bodyCounts(view.Body)
	fmt.Fprintf(w, "refs\t%d entries\n", counts["refs"])
	fmt.Fprintf(w, "packs\t%d entries\n", counts["packs"])
	fmt.Fprintf(w, "indexes\t%d entries\n", counts["indexes"])
	fmt.Fprintf(w, "bundles\t%d entries\n", counts["bundles"])
	w.Flush()
	return 0
}

// bodyCounts returns the cardinality of well-known body collections.
// For object-typed fields ("refs", "indexes") the count is len(map);
// for array-typed fields ("packs", "bundles") it is len(slice).
// Unknown fields are skipped — M1 deliberately doesn't enforce body
// schema.
func bodyCounts(body json.RawMessage) (map[string]int, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	out := map[string]int{}
	for k, v := range m {
		switch k {
		case "refs", "indexes":
			var obj map[string]json.RawMessage
			if json.Unmarshal(v, &obj) == nil {
				out[k] = len(obj)
			}
		case "packs", "bundles":
			var arr []json.RawMessage
			if json.Unmarshal(v, &arr) == nil {
				out[k] = len(arr)
			}
		}
	}
	return out, nil
}
