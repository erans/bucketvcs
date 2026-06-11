package auditlog

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

// maxDayListsPerPage bounds how many day-partition List calls one Page()
// makes. A very sparse prefix (long empty gaps between partitions) stops at
// the budget and returns a synthetic day-boundary cursor so pagination always
// terminates; the next page resumes the walk.
const maxDayListsPerPage = 100

// ObjectStore is the minimal storage slice the Reader needs: prefix listing
// plus object reads. The real *storage.ObjectStore satisfies it.
type ObjectStore interface {
	List(ctx context.Context, prefix string, opts *storage.ListOptions) (*storage.ListPage, error)
	Get(ctx context.Context, key string, opts *storage.GetOptions) (*storage.Object, error)
}

// Reader paginates newest-first over the shipped activity objects under a log
// prefix, decoding each object's gzipped NDJSON and applying a Filter.
//
// Pagination is object-cursor based: the cursor is the storage key of the
// oldest object included on the previous page; the next page consumes objects
// strictly older than that key. Key discovery walks the date-sharded
// partition layout (<prefix>/YYYY/MM/DD/) backward from the cursor's day
// (or today) to a floor day learned from one MaxKeys=1 probe, so a page
// costs a handful of small partition listings instead of a full-prefix scan.
// Within a page, objects are read newest-first
// and the resulting events are sorted by timestamp descending.
type Reader struct {
	store  ObjectStore
	prefix string

	// ObjectsPerPage caps how many activity objects one Page() call reads.
	ObjectsPerPage int

	// MaxBytesPerPage soft-caps a page by cumulative compressed object size;
	// the hard per-object decompressed guard lives in DecodeGz.
	//
	// Zero disables the page-level guard.
	MaxBytesPerPage int64

	// MaxEventsPerPage stops consuming further objects once this many matched
	// events have accumulated (the cursor advances to the last fully-consumed
	// object, so nothing is lost). Objects can decompress to 64 MiB each; this
	// bounds how much decoded event data one page can hold in memory.
	//
	// Zero disables the guard.
	MaxEventsPerPage int

	// Logger, when non-nil, records objects skipped by Page (Get or decode
	// failure) so an operator investigating missing audit events has a signal.
	Logger *slog.Logger
}

// NewReader builds a Reader over store. logPrefix is the operator-configured
// log root (e.g. "sys/logs"); empty defaults to "sys/logs". The activity
// objects live under "<logPrefix>/activity/". Trailing slashes on logPrefix
// are trimmed. Defaults: ObjectsPerPage=20, MaxBytesPerPage=32 MiB,
// MaxEventsPerPage=5000.
func NewReader(store ObjectStore, logPrefix string) *Reader {
	if logPrefix == "" {
		logPrefix = "sys/logs"
	}
	logPrefix = strings.TrimRight(logPrefix, "/")
	return &Reader{
		store:            store,
		prefix:           logPrefix + "/activity/",
		ObjectsPerPage:   20,
		MaxBytesPerPage:  32 << 20,
		MaxEventsPerPage: 5000,
	}
}

// dayLayout is the partition segment of shipped activity keys
// (<prefix><YYYY>/<MM>/<DD>/<HHMMSS>-<instance>-<seq>.ndjson.gz, written by
// internal/shiplog bucketKeyForPending).
const dayLayout = "2006/01/02"

// dayOf extracts the "YYYY/MM/DD" partition from a key under r.prefix.
func (r *Reader) dayOf(key string) (string, bool) {
	rest, ok := strings.CutPrefix(key, r.prefix)
	if !ok || len(rest) < len(dayLayout)+1 {
		return "", false
	}
	day := rest[:len(dayLayout)]
	if _, err := time.Parse(dayLayout, day); err != nil {
		return "", false
	}
	return day, true
}

// oldestDay probes the prefix with one MaxKeys=1 List. List returns keys in
// ascending lexicographic order (ObjectStore.List contract), so the first key
// is the oldest partition. Returns ("", nil) on an empty prefix and an error
// for a key that does not match the shiplog layout (junk under the log
// prefix must fail loudly, not be silently unreachable).
func (r *Reader) oldestDay(ctx context.Context) (string, error) {
	page, err := r.store.List(ctx, r.prefix, &storage.ListOptions{MaxKeys: 1})
	if err != nil {
		return "", err
	}
	if len(page.Objects) == 0 {
		return "", nil
	}
	day, ok := r.dayOf(page.Objects[0].Key)
	if !ok {
		return "", fmt.Errorf("auditlog: key %q under %q does not match the date-sharded activity layout", page.Objects[0].Key, r.prefix)
	}
	return day, nil
}

// listDay returns all keys under one day partition, descending (newest
// first), paging the store listing until exhausted.
func (r *Reader) listDay(ctx context.Context, day string) ([]string, error) {
	var keys []string
	token := ""
	for {
		page, err := r.store.List(ctx, r.prefix+day+"/", &storage.ListOptions{ContinuationToken: token})
		if err != nil {
			return nil, err
		}
		for _, o := range page.Objects {
			keys = append(keys, o.Key)
		}
		if page.NextToken == "" {
			break
		}
		token = page.NextToken
	}
	sort.Sort(sort.Reverse(sort.StringSlice(keys)))
	return keys, nil
}

// prevDay steps a "YYYY/MM/DD" partition back one calendar day.
func prevDay(day string) string {
	t, _ := time.Parse(dayLayout, day)
	return t.AddDate(0, 0, -1).Format(dayLayout)
}

// logSkip records a best-effort-skipped activity object when a Logger is set.
func (r *Reader) logSkip(key, stage string, err error) {
	if r.Logger != nil {
		r.Logger.Warn("auditlog: skipping activity object", "key", key, "stage", stage, "err", err)
	}
}

// Page returns up to ObjectsPerPage objects' worth of filtered events,
// newest-first, starting strictly older than cursor (empty cursor = newest).
// The cursor is a raw activity object key; callers that echo it to
// semi-privileged viewers (the per-repo audit tab) expose deployment-wide
// shipping metadata (timestamps/instance ids/sequence numbers — names only,
// never contents). Accepted for v1; opacify (HMAC) if that ever matters.
// The returned next cursor is the key of the oldest object included on this
// page, or "" when no older objects remain.
//
// A Get or DecodeGz failure on a single object is best-effort skipped: the
// object is counted as consumed and the page continues with older objects.
func (r *Reader) Page(ctx context.Context, f Filter, cursor string) ([]Event, string, error) {
	floorDay, err := r.oldestDay(ctx)
	if err != nil {
		return nil, "", err
	}
	if floorDay == "" {
		return nil, "", nil
	}

	// Start at the cursor's partition (resume) or today; date filters
	// narrow both ends of the walk. Day strings compare lexicographically
	// = chronologically.
	startDay := time.Now().UTC().Format(dayLayout)
	if cursor != "" {
		if d, ok := r.dayOf(cursor); ok {
			startDay = d
		} else {
			return nil, "", fmt.Errorf("auditlog: cursor %q does not match the date-sharded activity layout", cursor)
		}
	}
	if !f.Until.IsZero() {
		if u := f.Until.UTC().Format(dayLayout); u < startDay {
			startDay = u
		}
	}
	if !f.Since.IsZero() {
		if s := f.Since.UTC().Format(dayLayout); s > floorDay {
			floorDay = s
		}
	}
	if startDay < floorDay {
		return nil, "", nil
	}

	perPage := r.ObjectsPerPage
	if perPage <= 0 {
		perPage = 20
	}

	var events []Event
	consumed := 0
	var bytesUsed int64
	oldestKey := "" // oldest object consumed on this page
	dayLists := 0   // budget against maxDayListsPerPage
	day := startDay
	capped := false      // an object/byte/event cap fired
	lastOfFloor := false // cap fired on the floor day's oldest key -> nothing older

walk:
	for day >= floorDay {
		if dayLists >= maxDayListsPerPage {
			// Sparse prefix: terminate the page with a synthetic cursor.
			// day has NOT been listed yet, so the cursor must sort ABOVE
			// every key in day (resume includes it) and BELOW day+1:
			// "~" (0x7E) sorts above the key charset ([0-9a-z.-]).
			return r.finish(events, r.prefix+day+"/~")
		}
		keys, err := r.listDay(ctx, day)
		if err != nil {
			return nil, "", err
		}
		dayLists++
		for i, key := range keys {
			if cursor != "" && key >= cursor {
				continue // resume day: strictly-older only
			}
			obj, err := r.store.Get(ctx, key, nil)
			if err != nil {
				r.logSkip(key, "get", err)
				oldestKey = key
				consumed++
			} else {
				evs, skippedLines, decErr := DecodeGz(obj.Body)
				size := obj.Metadata.Size
				obj.Body.Close()
				if decErr != nil {
					r.logSkip(key, "decode", decErr)
					oldestKey = key
					consumed++
				} else {
					if skippedLines > 0 && r.Logger != nil {
						r.Logger.Warn("auditlog: skipped malformed lines in activity object",
							"key", key, "skipped_lines", skippedLines)
					}
					for _, e := range evs {
						if f.Match(e) {
							events = append(events, e)
						}
					}
					bytesUsed += size
					oldestKey = key
					consumed++
				}
			}
			if consumed >= perPage ||
				(r.MaxBytesPerPage > 0 && bytesUsed >= r.MaxBytesPerPage) ||
				(r.MaxEventsPerPage > 0 && len(events) >= r.MaxEventsPerPage) {
				capped = true
				// A cap on the floor day's last (oldest) key means the
				// walk is complete anyway -> empty next cursor, matching
				// the previous implementation's oldestIdx>0 rule.
				lastOfFloor = day == floorDay && i == len(keys)-1
				break walk
			}
		}
		if day == floorDay {
			break
		}
		day = prevDay(day)
	}

	next := ""
	if capped && !lastOfFloor && oldestKey != "" {
		next = oldestKey
	}
	return r.finish(events, next)
}

// finish applies the stable newest-first sort and returns the page.
func (r *Reader) finish(events []Event, next string) ([]Event, string, error) {
	sort.SliceStable(events, func(a, b int) bool {
		return events[a].Ts.After(events[b].Ts)
	})
	return events, next, nil
}
