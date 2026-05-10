package gc

import "errors"

// ErrInvalidPhaseCombo signals MarkOnly and SweepOnly were both set.
var ErrInvalidPhaseCombo = errors.New("gc: --mark-only and --sweep-only are mutually exclusive")

// ErrNoMarkForSweep signals --sweep-only with no mark records on disk.
var ErrNoMarkForSweep = errors.New("gc: --sweep-only requested but no mark records exist")
