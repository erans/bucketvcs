// internal/web/embed.go
package web

import "embed"

//go:embed all:templates static
var assetsFS embed.FS
