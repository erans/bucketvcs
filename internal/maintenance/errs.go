package maintenance

import "errors"

// ErrInvalidFlags is returned when RunOptions has mutually-exclusive or
// otherwise invalid combinations (e.g. repo and all-repos both set).
var ErrInvalidFlags = errors.New("maintenance: invalid flags")

// ErrCASExhausted is returned when the manifest CAS-merge loop exhausts
// its retry budget. The uploaded pack and indexes remain in the bucket
// and become orphan candidates for the next M8 GC run.
var ErrCASExhausted = errors.New("maintenance: cas retry budget exhausted")

// ErrCorruptInput is returned when the materialized bare repo fails
// fsck — the source canonical packs (or the manifest's ref tips) are
// inconsistent with each other and the run cannot proceed safely.
var ErrCorruptInput = errors.New("maintenance: corrupt input (fsck failed)")

// ErrNoRefs is returned when the manifest has no refs at run start;
// there is nothing to repack. Treated as a no-op success at the CLI.
var ErrNoRefs = errors.New("maintenance: manifest has no refs")
