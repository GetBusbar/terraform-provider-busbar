//go:build tools

// Package tools pins the code-generation toolchain (oapi-codegen) as an explicit
// module dependency so `go generate` uses a reproducible version. It is never
// compiled into the provider binary — the `tools` build tag excludes it.
package tools

import (
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
