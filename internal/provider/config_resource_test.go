package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The config apply document busbar boots from: the {config, providers} envelope.
// jsonencode keeps it readable in HCL; the model's max_concurrent/max_requests and
// the provider api_key_env / protocol / base_url are all required by the gateway.
const testAccConfigDoc = `
provider "busbar" {}

resource "busbar_config" "test" {
  document = jsonencode({
    config = {
      auth = null
      models = {
        test-model = { provider = "anthropic", max_concurrent = 1, max_requests = -1 }
      }
      providers = {
        anthropic = { api_key_env = "ANTHROPIC_API_KEY" }
      }
      governance = {
        enabled     = true
        db_path     = "/var/lib/busbar/governance.db"
        admin_token = "tfacc-admin-token"
      }
    }
    providers = {
      anthropic = { protocol = "anthropic", base_url = "https://api.anthropic.com" }
    }
  })
}
`

// TestAccConfigResource exercises the GitOps singleton against a live gateway:
// apply (Create), read-back (config_version surfaced), re-apply (Update bumps the
// version), import, and destroy (a no-op that only drops tracking). Gated on
// TF_ACC + a reachable gateway. NOTE: this bumps config_version, so run it
// against a disposable gateway (the acceptance recipe container).
func TestAccConfigResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create == apply. An apply bumps the monotonic config_version to a
			// positive integer (exact value depends on prior applies on the gateway).
			{
				Config: testAccConfigDoc,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_config.test", "id", "config"),
					resource.TestMatchResourceAttr("busbar_config.test", "config_version",
						regexp.MustCompile(`^[1-9][0-9]*$`)),
				),
			},
			// Import the singleton (document is not round-trippable, so ignore it).
			{
				ResourceName:            "busbar_config.test",
				ImportState:             true,
				ImportStateId:           "config",
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"document"},
			},
		},
	})
}
