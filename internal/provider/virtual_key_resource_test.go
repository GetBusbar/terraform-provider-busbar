package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccVirtualKeyResource exercises the full lifecycle of a governance virtual
// key against a live gateway: create (mints a once-shown secret), read-back,
// import, then destroy (revoke). Gated on TF_ACC and
// a reachable gateway with governance enabled (BUSBAR_ENDPOINT + BUSBAR_ADMIN_TOKEN).
func TestAccVirtualKeyResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		CheckDestroy:             testAccCheckDestroyed("busbar_virtual_key", "keys", "id", true),
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create + Read.
			{
				Config: `
provider "busbar" {}

resource "busbar_virtual_key" "test" {
  name             = "tfacc-key"
  allowed_pools    = []
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "id",
						regexp.MustCompile(`^vk_[0-9a-f]+$`)),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "name", "tfacc-key"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "enabled", "true"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "allowed_pools.#", "0"),
					// The plaintext secret is captured once at create.
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "secret",
						regexp.MustCompile(`^bbk_[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)),
				),
			},
			// Import (secret is create-only, so it is not recovered).
			{
				ResourceName:            "busbar_virtual_key.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret", "aws_access_key_id", "aws_secret_access_key", "issue_aws_credential"},
			},
		},
	})
}
