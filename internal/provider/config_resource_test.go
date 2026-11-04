package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

// Reuse the exact disposable gateway config so applying the singleton preserves
// the signed hook, identity provider, and authentication signing key.
func testAccConfigDocuments(t *testing.T) (string, string) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC must be enabled for config acceptance")
	}
	data, err := os.ReadFile(os.Getenv("BUSBAR_TEST_CONFIG_FILE"))
	if err != nil {
		t.Fatalf("read BUSBAR_TEST_CONFIG_FILE: %v", err)
	}
	var config map[string]interface{}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatalf("decode acceptance boot config: %v", err)
	}
	document := func() string {
		// Providers is optional: preserve the gateway's current provider catalog.
		payload, err := json.Marshal(map[string]interface{}{"config": config})
		if err != nil {
			t.Fatalf("encode acceptance config: %v", err)
		}
		return fmt.Sprintf("provider \"busbar\" {}\nresource \"busbar_config\" \"test\" {\n document = %s\n}\n", strconv.Quote(string(payload)))
	}
	initial := document()
	models, ok := config["models"].(map[string]interface{})
	if !ok {
		t.Fatal("acceptance config is missing models")
	}
	model, ok := models["test-model"].(map[string]interface{})
	if !ok {
		t.Fatal("acceptance config is missing test-model")
	}
	model["max_concurrent"] = 2
	return initial, document()
}

// TestAccConfigResource exercises the GitOps singleton against a live gateway:
// apply (Create), read-back (config_version surfaced), re-apply (Update bumps the
// version), import, and destroy (a no-op that only drops tracking). Gated on
// TF_ACC + a reachable gateway. NOTE: this bumps config_version, so run it
// against a disposable gateway (the acceptance recipe container).
func TestAccConfigResource(t *testing.T) {
	initial, updated := testAccConfigDocuments(t)
	var appliedVersion int64
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			// Create == apply. An apply bumps the monotonic config_version to a
			// positive integer (exact value depends on prior applies on the gateway).
			{
				Config: initial,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("busbar_config.test", "id", "config"),
					resource.TestCheckResourceAttrWith("busbar_config.test", "config_version", func(value string) error {
						var err error
						appliedVersion, err = strconv.ParseInt(value, 10, 64)
						return err
					}),
					resource.TestMatchResourceAttr("busbar_config.test", "config_version",
						regexp.MustCompile(`^[1-9][0-9]*$`)),
				),
			},
			{
				Config: updated,
				Check: resource.TestCheckResourceAttrWith("busbar_config.test", "config_version", func(value string) error {
					version, err := strconv.ParseInt(value, 10, 64)
					if err != nil {
						return err
					}
					if version <= appliedVersion {
						return fmt.Errorf("config version did not increase after update")
					}
					return nil
				}),
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
