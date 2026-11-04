package provider

import (
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccHookResource exercises the full lifecycle of a routing hook against a
// live gateway: register (POST), read-back, replace-in-place (PUT), import, then
// destroy (DELETE). Gated on TF_ACC + a reachable gateway.
func TestAccHookResource(t *testing.T) {
	module := os.Getenv("BUSBAR_TEST_HOOK_MODULE")
	if module == "" {
		if os.Getenv("TF_ACC") != "" {
			t.Fatal("BUSBAR_TEST_HOOK_MODULE is required when TF_ACC is enabled")
		}
		t.Skip("set BUSBAR_TEST_HOOK_MODULE to an installed signed gate plugin for hook acceptance")
	}
	resource.Test(t, resource.TestCase{
		CheckDestroy:             testAccCheckDestroyed("busbar_hook", "hooks", "name", false),
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Register + Read.
			{
				Config: strings.ReplaceAll(`
provider "busbar" {}

resource "busbar_hook" "test" {
  name       = "tfacc-hook"
  kind       = "gate"
  module     = "HOOK_MODULE"
  timeout_ms = 50
  priority   = 3
  settings   = jsonencode({ threshold = 0.5 })
}
`, "HOOK_MODULE", module),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_hook.test", "name", "tfacc-hook"),
					resource.TestCheckResourceAttr("busbar_hook.test", "kind", "gate"),
					resource.TestCheckResourceAttr("busbar_hook.test", "module", module),
					resource.TestCheckResourceAttr("busbar_hook.test", "timeout_ms", "50"),
					resource.TestCheckResourceAttr("busbar_hook.test", "priority", "3"),
					resource.TestCheckResourceAttr("busbar_hook.test", "prompt", "no"),
					resource.TestCheckResourceAttr("busbar_hook.test", "on_error", "nothing"),
				),
			},
			// Replace in place (PUT): change timeout, priority, and settings.
			{
				Config: strings.ReplaceAll(`
provider "busbar" {}

resource "busbar_hook" "test" {
  name       = "tfacc-hook"
  kind       = "gate"
  module     = "HOOK_MODULE"
  timeout_ms = 120
  priority   = 9
  on_error   = "reject"
  settings   = jsonencode({ threshold = 0.9 })
}
`, "HOOK_MODULE", module),
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
				ImportStateVerifyIgnore:              []string{"on_empty", "default", "settings"},
			},
		},
	})
}
