// Package web holds spore's UI assets. They are embedded into the binary, so
// there is no build step, no Node toolchain and nothing to install: the
// single file that gets scp'd to a server carries its own front end.
//
// The package exists at the repository root rather than inside
// internal/daemon because go:embed cannot reach outside its own directory,
// and because spec section 2 puts web/ here.
package web

import "embed"

//go:embed index.html style.css app.js
var FS embed.FS
