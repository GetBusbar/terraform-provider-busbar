package provider

import (
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccHookResource exercises the full lifecycle of a routing hook against a
// live busbar >= 1.5.0 gateway: register (POST), read-back, replace-in-place
// (PUT), import, then destroy (DELETE). 1.5.0 hooks dispatch to a signed
// `kind: hook` plugin named by `plugin`; the compiled-in `ranking` plugin is
// always present, so the test targets it. Gated on TF_ACC + a reachable gateway.
func TestAccHookResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Register + Read.
			{
				Config: `
provider "busbar" {}

resource "busbar_hook" "test" {
  name       = "tfacc-hook"
  kind       = "gate"
  plugin     = "ranking"
  timeout_ms = 50
  priority   = 3
  settings   = jsonencode({ threshold = 0.5 })
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_hook.test", "name", "tfacc-hook"),
					resource.TestCheckResourceAttr("busbar_hook.test", "kind", "gate"),
					resource.TestCheckResourceAttr("busbar_hook.test", "plugin", "ranking"),
					resource.TestCheckResourceAttr("busbar_hook.test", "timeout_ms", "50"),
					resource.TestCheckResourceAttr("busbar_hook.test", "priority", "3"),
					resource.TestCheckResourceAttr("busbar_hook.test", "prompt", "no"),
					resource.TestCheckResourceAttr("busbar_hook.test", "on_error", "nothing"),
				),
			},
			// Replace in place (PUT): change timeout, priority, and settings.
			{
				Config: `
provider "busbar" {}

resource "busbar_hook" "test" {
  name       = "tfacc-hook"
  kind       = "gate"
  plugin     = "ranking"
  timeout_ms = 120
  priority   = 9
  on_error   = "reject"
  settings   = jsonencode({ threshold = 0.9 })
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_hook.test", "timeout_ms", "120"),
					resource.TestCheckResourceAttr("busbar_hook.test", "priority", "9"),
					resource.TestCheckResourceAttr("busbar_hook.test", "on_error", "reject"),
				),
			},
			// Import by name (the hook's identity is its name, not a synthetic id).
			{
				ResourceName:                         "busbar_hook.test",
				ImportState:                          true,
				ImportStateId:                        "tfacc-hook",
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "name",
				ImportStateVerifyIgnore:              []string{"on_empty", "default"},
			},
		},
	})
}
