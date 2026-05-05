# Differential-harness known divergences

Tracked divergences between bucketvcs and upstream git, per spec §40.3.

This is **not** a dumping ground for correctness bugs. Each entry below
must include:

- a `## ` heading with a short title
- `Classification:` one of:
  - `bucketvcs bug`
  - `git quirk to emulate`
  - `intentional documented difference`
  - `unsupported optional capability`
  - `invalid test case`
- `Date:` YYYY-MM-DD
- `Issue:` https URL to the tracking issue

A CI test (`internal/diffharness/divergences_test.go`) parses this file
and fails the build if any entry is missing a required field.

At M2 ship there are no known divergences.

<!-- entries below this line, newest first -->
