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
					// Reads redact settings to key names (settings_keys), so
					// state must carry exactly the value this apply sent.
					resource.TestCheckResourceAttr("busbar_hook.test", "settings", `{"threshold":0.5}`),
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
					resource.TestCheckResourceAttr("busbar_hook.test", "settings", `{"threshold":0.9}`),
				),
			},
			// Import by name (the hook's identity is its name, not a synthetic id).
			{
				ResourceName:                         "busbar_hook.test",
				ImportState:                          true,
				ImportStateId:                        "tfacc-hook",
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "name",
				// on_empty/default are write-only. settings VALUES are
				// unrecoverable on import by API contract: since busbar 1.5.3
				// every hook read redacts the bag to key names (settings_keys)
				// because it may carry SecretRefs, so the imported state cannot
				// contain the values the pre-import state had. The value
				// round-trip itself is still fully asserted by the
				// TestCheckResourceAttr("settings", ...) checks in steps 1-2.
				ImportStateVerifyIgnore: []string{"on_empty", "default", "settings"},
			},
		},
	})
}
