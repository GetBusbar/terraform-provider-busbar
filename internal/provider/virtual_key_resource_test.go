package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccVirtualKeyResource exercises the full lifecycle of a governance virtual
// key against a live gateway: create (mints a once-shown secret), read-back,
// in-place cap update (PATCH), import, then destroy (revoke). Gated on TF_ACC and
// a reachable gateway with governance enabled (BUSBAR_ENDPOINT + BUSBAR_ADMIN_TOKEN).
func TestAccVirtualKeyResource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create + Read.
			{
				Config: `
provider "busbar" {}

resource "busbar_virtual_key" "test" {
  name             = "tfacc-key"
  budget_period    = "daily"
  max_budget_cents = 1000
  rpm_limit        = 10
  allowed_pools    = ["smart"]
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "id",
						regexp.MustCompile(`^vk_[0-9a-f]+$`)),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "name", "tfacc-key"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "budget_period", "daily"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "max_budget_cents", "1000"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "rpm_limit", "10"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "enabled", "true"),
					// The plaintext secret is captured once at create.
					resource.TestMatchResourceAttr("busbar_virtual_key.test", "secret",
						regexp.MustCompile(`^sk-bb-[0-9a-f]+$`)),
				),
			},
			// Update the mutable caps in place (PATCH).
			{
				Config: `
provider "busbar" {}

resource "busbar_virtual_key" "test" {
  name             = "tfacc-key"
  budget_period    = "daily"
  max_budget_cents = 5000
  rpm_limit        = 25
  tpm_limit        = 100000
  allowed_pools    = ["smart"]
}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "max_budget_cents", "5000"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "rpm_limit", "25"),
					resource.TestCheckResourceAttr("busbar_virtual_key.test", "tpm_limit", "100000"),
				),
			},
			// Import (secret is create-only, so it is not recovered).
			{
				ResourceName:            "busbar_virtual_key.test",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret", "aws_access_key_id", "aws_secret_access_key", "allowed_pools", "issue_aws_credential"},
			},
		},
	})
}
