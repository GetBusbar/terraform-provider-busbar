// terraform-provider-busbar is the Terraform provider for the busbar LLM gateway
// admin API. Slice 1 ships the client + auth plumbing and a single read-only
// data source, busbar_info.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/GetBusbar/terraform-provider-busbar/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	opts := providerserver.ServeOpts{
		// Must match the required_providers source address consumers use.
		Address: "registry.terraform.io/GetBusbar/busbar",
		Debug:   debug,
	}

	if err := providerserver.Serve(context.Background(), provider.New(version), opts); err != nil {
		log.Fatal(err.Error())
	}
}
