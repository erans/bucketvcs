// Package auth defines bucketvcs's transport-neutral authentication and
// authorization model.
//
// This package contains only types and pure logic. It has no HTTP, no SSH,
// and no SQL imports. Storage and transport live at the edges:
//
//   - internal/auth/sqlitestore    persistent Store implementation
//   - internal/gateway             HTTP authentication middleware
//   - cmd/bucketvcs                admin CLI
//
// The single allow/deny decision is auth.Decide. The Store interface is the
// only seam with persistent state.
package auth
