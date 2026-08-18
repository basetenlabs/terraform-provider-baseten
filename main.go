package main

import (
	"context"
	"flag"
	"log"

	"github.com/basetenlabs/terraform-provider-baseten/internal/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
)

// version is overwritten at release time with the tag being built.
var version = "dev"

func main() {
	var debug bool
	flag.BoolVar(&debug, "debug", false, "run the provider with support for debuggers like delve")
	flag.Parse()

	err := providerserver.Serve(context.Background(), provider.New(version), providerserver.ServeOpts{
		Address: "registry.terraform.io/basetenlabs/baseten",
		Debug:   debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
