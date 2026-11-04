package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

func testAccCheckDestroyed(resourceType, collection, identity string, tombstones bool) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		client, err := buildHTTPClient(busbarProviderModel{})
		if err != nil {
			return err
		}
		defer client.CloseIdleConnections()
		admin := newAdminClient(os.Getenv(envEndpoint), os.Getenv(envToken), client)
		checked := false
		for _, item := range state.RootModule().Resources {
			if item.Type != resourceType || item.Primary == nil {
				continue
			}
			id := item.Primary.Attributes[identity]
			if id == "" {
				return fmt.Errorf("destroy check has no resource identity")
			}
			checked = true
			response, err := admin.do(context.Background(), http.MethodGet, "/"+collection+"/"+url.PathEscape(id), nil, nil)
			if err != nil {
				return err
			}
			if response.StatusCode == http.StatusNotFound {
				continue
			}
			if tombstones && response.StatusCode == http.StatusOK {
				var view struct {
					State string `json:"state"`
				}
				if err := response.decode(&view); err != nil {
					return err
				}
				if view.State == "tombstoned" {
					continue
				}
			}
			return fmt.Errorf("%s still exists after destroy (HTTP %d)", resourceType, response.StatusCode)
		}
		if !checked {
			return fmt.Errorf("destroy check found no %s identity in prior state", resourceType)
		}
		return nil
	}
}
