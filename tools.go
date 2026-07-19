//go:build tools

// Package tools pins the code-generation and documentation toolchain
// (oapi-codegen for the API client, tfplugindocs for the registry docs) as
// explicit module dependencies so `go generate` uses reproducible versions. It is
// never compiled into the provider binary — the `tools` build tag excludes it.
package tools

import (
	_ "github.com/hashicorp/terraform-plugin-docs/cmd/tfplugindocs"
	_ "github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen"
)
