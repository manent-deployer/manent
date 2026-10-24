// Command terraform-provider-manent is the Terraform provider of manent: releases (resource
// manent_deployment), inventory and checks of the environment configuration as code.
package main

import (
	"context"
	"flag"
	"log"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"

	"github.com/joeg-ita/manent/integrations/terraform/internal/provider"
	"github.com/joeg-ita/manent/internal/version"
)

func main() {
	debug := flag.Bool("debug", false, "starts the provider in debug mode (for debuggers such as delve)")
	flag.Parse()
	err := providerserver.Serve(context.Background(), provider.New(version.Version), providerserver.ServeOpts{
		Address: "registry.terraform.io/joeg-ita/manent",
		Debug:   *debug,
	})
	if err != nil {
		log.Fatal(err)
	}
}
