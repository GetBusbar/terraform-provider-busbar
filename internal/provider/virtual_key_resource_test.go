package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccVirtualKeyResource exercises the full lifecycle of a governance virtual
// key against a live busbar >= 1.5.0 gateway: create (mints a once-shown signed
// token), read-back, in-place update (PATCH enabled + group unbind), import,
// then destroy (revoke). Gated on TF_ACC and a reachable gateway with governance
// enabled (BUSBAR_ENDPOINT + BUSBAR_ADMIN_TOKEN). The gateway's config must
// define a `tfacc-group` entry in its top-level `groups:` block (the acceptance
// boot config does).
func TestAccVirtualKeyResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create + Read: bind to an existing group, scope pools, set expiry.
			{
				Config: `
provider "busbar" {}

resource "busbar_virtual_key" "test" {
  name          = "tfacc-key"
  group         = "tfacc-group"
  allowed_pools = ["smart"]
  expires_in    = "24h"
  labels        = { team = "tfacc" }
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "id",
						regexp.MustCompile(`^vk_[0-9a-f]+$`)),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "name", "tfacc-key"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "group", "tfacc-group"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "allowed_pools.0", "smart"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "labels.team", "tfacc"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "enabled", "true"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "state", "active"),
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "expires_at",
						regexp.MustCompile(`^[1-9][0-9]*$`)),
					// The signed token is captured once at create.
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "token",
						regexp.MustCompile(`^bbk_[A-Za-z0-9_\-.]+$`)),
				),
			},
			// Update in place (PATCH): disable the key and unbind its group.
			{
				Config: `
provider "busbar" {}

resource "busbar_virtual_key" "test" {
  name          = "tfacc-key"
  allowed_pools = ["smart"]
  expires_in    = "24h"
  labels        = { team = "tfacc" }
  enabled       = false
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "enabled", "false"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "state", "disabled"),
					resource.TestCheckNoResourceAttr("busbar_virtual_key.test", "group"),
				),
			},
			// Import (mint-only fields are not recoverable from a read).
			{
				ResourceName:      "busbar_virtual_key.test",
				ImportState:       true,
				ImportStateVerify: true,
				ImportStateVerifyIgnore: []string{
					"token", "expires_in", "expires_at", "group_provisioned",
					"issue_aws_credential", "aws_access_key_id", "aws_secret_access_key",
				},
			},
		},
	})
}
