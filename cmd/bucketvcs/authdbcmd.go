package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/benbjohnson/litestream"
	"github.com/superfly/ltx"

	"github.com/bucketvcs/bucketvcs/internal/auth/sqlitestore"
	"github.com/bucketvcs/bucketvcs/internal/authreplica"
	"github.com/bucketvcs/bucketvcs/internal/storage"
)

const authdbUsage = `usage: bucketvcs authdb <action> [flags]

actions:
  restore         restore the authdb from a replica (optionally point-in-time)
  replica-status  show replica levels, latest TXIDs and lease holder
`

func runAuthDBCmd(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, authdbUsage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, authdbUsage)
		return 0
	case "restore":
		return runAuthDBRestore(ctx, args[1:], stdout, stderr)
	case "replica-status":
		return runAuthDBReplicaStatus(ctx, args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "authdb: unknown action %q\n", args[0])
		fmt.Fprint(stderr, authdbUsage)
		return 2
	}
}

// openReplicaTarget resolves --replica/--store into (ObjectStore, prefix).
// --replica accepts a storage URL directly, or "auto" to reuse --store with
// the reserved DefaultPrefix (mirroring serve's --auth-db-replica=auto).
func openReplicaTarget(replica, storeURL string) (storage.ObjectStore, string, error) {
	switch strings.TrimSpace(replica) {
	case "", "off":
		return nil, "", fmt.Errorf("--replica is required (\"auto\" or a storage URL)")
	case "auto":
		if storeURL == "" {
			return nil, "", fmt.Errorf("--replica=auto requires --store")
		}
		st, err := openStore(storeURL)
		if err != nil {
			return nil, "", err
		}
		return st, authreplica.DefaultPrefix, nil
	default:
		st, err := openStore(replica)
		if err != nil {
			return nil, "", err
		}
		return st, authreplica.DefaultPrefix, nil
	}
}

func runAuthDBRestore(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("authdb restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replica := fs.String("replica", "", `replica location: "auto" (with --store) or a storage URL`)
	storeURL := fs.String("store", "", "system store URL (with --replica=auto)")
	authDB := fs.String("auth-db", "", "target authdb path (default: standard resolution)")
	output := fs.String("output", "", "restore to this path instead of the authdb path")
	timestamp := fs.String("timestamp", "", "point-in-time upper bound (RFC3339)")
	txid := fs.String("txid", "", "TXID upper bound (hex)")
	ifNotExists := fs.Bool("if-not-exists", false, "exit 0 without restoring if the target exists")
	force := fs.Bool("force", false, "overwrite an existing target")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *timestamp != "" && *txid != "" {
		fmt.Fprintln(stderr, "authdb restore: --timestamp and --txid are mutually exclusive")
		return 2
	}

	target := *output
	if target == "" {
		p, err := resolveAuthDB(*authDB, realEnv())
		if err != nil {
			fmt.Fprintf(stderr, "authdb restore: %v\n", err)
			return 1
		}
		target = sqlitestore.SQLitePath(p)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		if *ifNotExists {
			fmt.Fprintf(stdout, "authdb restore: %s exists; nothing to do\n", target)
			return 0
		}
		if !*force {
			fmt.Fprintf(stderr, "authdb restore: %s exists; pass --force to overwrite or --if-not-exists to no-op\n", target)
			return 2
		}
		if err := os.Remove(target); err != nil {
			fmt.Fprintf(stderr, "authdb restore: %v\n", err)
			return 1
		}
		// A leftover -wal/-shm from the PREVIOUS database would be replayed
		// over the freshly restored file on the next WAL-mode open, silently
		// corrupting it. Remove them (and litestream's -txid sidecar).
		for _, sfx := range []string{"-wal", "-shm", "-txid"} {
			if err := os.Remove(target + sfx); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(stderr, "authdb restore: %v\n", err)
				return 1
			}
		}
	} else if !os.IsNotExist(statErr) {
		// Don't silently treat an unreadable target as absent — the restore
		// would bypass the --force guard and sidecar cleanup, then fail anyway.
		fmt.Fprintf(stderr, "authdb restore: stat %s: %v\n", target, statErr)
		return 1
	}

	st, prefix, err := openReplicaTarget(*replica, *storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "authdb restore: %v\n", err)
		return 2
	}
	defer closeStore(st)
	client := authreplica.NewClient(st, prefix)

	// lsdb is never Open()ed — Replica.Restore is the only operation performed,
	// so there are no goroutines/resources to Close. Revisit if a litestream
	// upgrade starts allocating in NewDB.
	lsdb := litestream.NewDB(target)
	r := litestream.NewReplicaWithClient(lsdb, client)
	opt := litestream.NewRestoreOptions()
	opt.OutputPath = target
	if *timestamp != "" {
		ts, err := time.Parse(time.RFC3339, *timestamp)
		if err != nil {
			fmt.Fprintf(stderr, "authdb restore: --timestamp: %v\n", err)
			return 2
		}
		opt.Timestamp = ts
	}
	if *txid != "" {
		v, err := strconv.ParseUint(strings.TrimPrefix(*txid, "0x"), 16, 64)
		if err != nil {
			fmt.Fprintf(stderr, "authdb restore: --txid: %v\n", err)
			return 2
		}
		opt.TXID = ltx.TXID(v)
	}
	if err := r.Restore(ctx, opt); err != nil {
		fmt.Fprintf(stderr, "authdb restore: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "restored %s\n", target)
	return 0
}

func runAuthDBReplicaStatus(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("authdb replica-status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	replica := fs.String("replica", "", `replica location: "auto" (with --store) or a storage URL`)
	storeURL := fs.String("store", "", "system store URL (with --replica=auto)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	st, prefix, err := openReplicaTarget(*replica, *storeURL)
	if err != nil {
		fmt.Fprintf(stderr, "authdb replica-status: %v\n", err)
		return 2
	}
	defer closeStore(st)
	client := authreplica.NewClient(st, prefix)

	enc := json.NewEncoder(stdout)

	// Lease holder first, when present. Absent lease is not an error: a
	// replica that has never been served by a live node has no lease.json.
	if doc, ok := readLeaseHolder(ctx, st, prefix, stderr); ok {
		_ = enc.Encode(map[string]any{"lease": map[string]any{
			"instance_id": doc.InstanceID,
			"hostname":    doc.Hostname,
			"pid":         doc.PID,
			"renewed_at":  doc.RenewedAt.UTC().Format(time.RFC3339),
			"ttl_s":       doc.TTLSeconds,
		}})
	}

	// Levels 0..9: 0-2 are the configured compaction levels; 9 is
	// litestream's snapshot level (observed in live runs).
	for level := 0; level <= 9; level++ {
		itr, err := client.LTXFiles(ctx, level, 0, false)
		if err != nil {
			fmt.Fprintf(stderr, "authdb replica-status: level %d: %v\n", level, err)
			return 1
		}
		var n int
		var maxTXID ltx.TXID
		var latest time.Time
		var bytes int64
		// No itr.Err() check needed: LTXFiles returns a fully-materialized
		// slice iterator (errors surface synchronously from LTXFiles itself).
		// Revisit if Client moves to streaming iteration.
		for itr.Next() {
			it := itr.Item()
			n++
			bytes += it.Size
			if it.MaxTXID > maxTXID {
				maxTXID = it.MaxTXID
			}
			if it.CreatedAt.After(latest) {
				latest = it.CreatedAt
			}
		}
		itr.Close()
		if n == 0 {
			continue
		}
		_ = enc.Encode(map[string]any{
			"level": level, "files": n, "bytes": bytes,
			"max_txid": fmt.Sprintf("%016x", uint64(maxTXID)),
			"latest":   latest.UTC().Format(time.RFC3339),
		})
	}
	return 0
}

// readLeaseHolder fetches and parses <prefix>/lease.json. A missing lease
// returns (_, false) with no error — it just means no node currently (or
// recently) held the replication lease. Any other Get error is surfaced as a
// one-line warning to stderr and the lease line is omitted.
func readLeaseHolder(ctx context.Context, st storage.ObjectStore, prefix string, stderr io.Writer) (authreplica.LeaseDoc, bool) {
	key := path.Join(prefix, "lease.json")
	obj, err := st.Get(ctx, key, nil)
	if err != nil {
		if !errors.Is(err, storage.ErrNotFound) {
			fmt.Fprintf(stderr, "authdb replica-status: lease: %v\n", err)
		}
		return authreplica.LeaseDoc{}, false
	}
	defer obj.Body.Close()
	b, err := io.ReadAll(obj.Body)
	if err != nil {
		fmt.Fprintf(stderr, "authdb replica-status: lease: %v\n", err)
		return authreplica.LeaseDoc{}, false
	}
	var doc authreplica.LeaseDoc
	if err := json.Unmarshal(b, &doc); err != nil {
		fmt.Fprintf(stderr, "authdb replica-status: lease: %v\n", err)
		return authreplica.LeaseDoc{}, false
	}
	return doc, true
}
