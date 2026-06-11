package auditlog

import (
	"context"
	"log/slog"
	"sort"
	"strings"

	"github.com/bucketvcs/bucketvcs/internal/storage"
)

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
// strictly older than that key. Within a page, objects are read newest-first
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

	// Logger, when non-nil, records objects skipped by Page (Get or decode
	// failure) so an operator investigating missing audit events has a signal.
	Logger *slog.Logger
}

// NewReader builds a Reader over store. logPrefix is the operator-configured
// log root (e.g. "sys/logs"); empty defaults to "sys/logs". The activity
// objects live under "<logPrefix>/activity/". Trailing slashes on logPrefix
// are trimmed. Defaults: ObjectsPerPage=20, MaxBytesPerPage=32 MiB.
func NewReader(store ObjectStore, logPrefix string) *Reader {
	if logPrefix == "" {
		logPrefix = "sys/logs"
	}
	logPrefix = strings.TrimRight(logPrefix, "/")
	return &Reader{
		store:           store,
		prefix:          logPrefix + "/activity/",
		ObjectsPerPage:  20,
		MaxBytesPerPage: 32 << 20,
	}
}

// listKeys returns all activity object keys under the prefix, ascending
// (oldest..newest), paging store.List via ContinuationToken until exhausted.
func (r *Reader) listKeys(ctx context.Context) ([]string, error) {
	var keys []string
	token := ""
	for {
		page, err := r.store.List(ctx, r.prefix, &storage.ListOptions{ContinuationToken: token})
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
	sort.Strings(keys)
	return keys, nil
}

// logSkip records a best-effort-skipped activity object when a Logger is set.
func (r *Reader) logSkip(key, stage string, err error) {
	if r.Logger != nil {
		r.Logger.Warn("auditlog: skipping activity object", "key", key, "stage", stage, "err", err)
	}
}

// Page returns up to ObjectsPerPage objects' worth of filtered events,
// newest-first, starting strictly older than cursor (empty cursor = newest).
// The returned next cursor is the key of the oldest object included on this
// page, or "" when no older objects remain.
//
// A Get or DecodeGz failure on a single object is best-effort skipped: the
// object is counted as consumed and the page continues with older objects.
func (r *Reader) Page(ctx context.Context, f Filter, cursor string) ([]Event, string, error) {
	keys, err := r.listKeys(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(keys) == 0 {
		return nil, "", nil
	}

	// end is the exclusive newest-side index: we consume keys[end-1] down to 0.
	// Empty cursor → start at the newest (end = len). Otherwise resume strictly
	// older than the cursor: SearchStrings gives the first index >= cursor, so
	// keys[:idx] are exactly the strictly-older keys whether or not the cursor
	// object still exists (it may have been swept by retention between pages).
	end := len(keys)
	if cursor != "" {
		end = sort.SearchStrings(keys, cursor)
	}

	var events []Event
	consumed := 0
	oldestIdx := -1
	var bytesUsed int64

	for i := end - 1; i >= 0 && consumed < r.ObjectsPerPage; i-- {
		obj, err := r.store.Get(ctx, keys[i], nil)
		if err != nil {
			// Best-effort skip: advance past this object.
			r.logSkip(keys[i], "get", err)
			oldestIdx = i
			consumed++
			continue
		}
		evs, _, decErr := DecodeGz(obj.Body)
		size := obj.Metadata.Size
		obj.Body.Close()
		if decErr != nil {
			r.logSkip(keys[i], "decode", decErr)
			oldestIdx = i
			consumed++
			continue
		}
		for _, e := range evs {
			if f.Match(e) {
				events = append(events, e)
			}
		}
		bytesUsed += size
		oldestIdx = i
		consumed++
		if r.MaxBytesPerPage > 0 && bytesUsed >= r.MaxBytesPerPage {
			break
		}
	}

	next := ""
	if oldestIdx > 0 && consumed > 0 {
		next = keys[oldestIdx]
	}

	sort.Slice(events, func(a, b int) bool {
		return events[a].Ts.After(events[b].Ts)
	})

	return events, next, nil
}
