package provider

import (
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// TestAccInfoDataSource reads busbar_info against a live gateway and asserts the
// version looks like a semantic version. Gated on TF_ACC; requires a reachable
// gateway via BUSBAR_ENDPOINT + BUSBAR_ADMIN_TOKEN (see testAccPreCheck). The
// provider block itself is populated from those env fallbacks.
func TestAccInfoDataSource(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
provider "busbar" {}

data "busbar_info" "test" {}
`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr("data.busbar_info.test", "version",
						regexp.MustCompile(`^\d+\.\d+\.\d+`)),
					// weighted_floor is compiled in unconditionally — always true.
					resource.TestCheckResourceAttr("data.busbar_info.test", "weighted_floor", "true"),
				),
			},
		},
	})
}
