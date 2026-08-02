package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// The config apply document, in busbar 1.5.0 config syntax: the {config,
// providers} envelope POSTed to /api/v1/admin/config/apply. It deliberately
// mirrors the acceptance gateway's boot config (same auth chain, admin-tokens
// env, providers, models, and the tfacc-group `groups:` entry the virtual-key
// test binds to) so applying it never locks the suite out of the admin plane.
// listen/admin_listen are omitted: an apply keeps the running listeners.
const testAccConfigDoc = `
provider "busbar" {}

resource "busbar_config" "test" {
  document = jsonencode({
    config = {
      auth = {
        chain = ["keys"]
        admin_auth = [
          { admin-tokens = { token = { env = "BUSBAR_ADMIN_TOKEN" } } }
        ]
      }
      providers = {
        anthropic = { api_key = { env = "ANTHROPIC_API_KEY" } }
      }
      models = {
        test-model = { provider = "anthropic" }
      }
      groups = {
        tfacc-group = {}
      }
    }
    providers = {
      anthropic = { protocol = "anthropic", base_url = "https://api.anthropic.com" }
    }
  })
}
`

// TestAccConfigResource exercises the GitOps singleton against a live gateway:
// apply (Create), read-back (config_version surfaced), import, and destroy (a
// no-op that only drops tracking). Gated on TF_ACC + a reachable gateway. NOTE:
// an apply replaces the RUNNING config wholesale (runtime-registered hooks are
// dropped) and bumps config_version, so run it against a disposable gateway
// (the acceptance recipe boot).
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
