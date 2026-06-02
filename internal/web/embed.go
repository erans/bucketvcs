// internal/web/embed.go
package web

import "embed"

//go:embed templates static
var assetsFS embed.FS
