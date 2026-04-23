package web

import "embed"

//go:embed static
//go:embed static/vendor
var StaticFS embed.FS
