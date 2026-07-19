package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

// testAccProtoV6ProviderFactories wires the in-process provider server used by
// acceptance tests. Consumers reference the provider as "busbar".
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"busbar": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck asserts the environment is set up for a live acceptance run.
// Acceptance tests only run when TF_ACC is set (see the testacc make target);
// they need a reachable gateway via BUSBAR_ENDPOINT + BUSBAR_ADMIN_TOKEN.
func testAccPreCheck(t *testing.T) {
	t.Helper()
	for _, env := range []string{"BUSBAR_ENDPOINT", "BUSBAR_ADMIN_TOKEN"} {
		if v := os.Getenv(env); v == "" {
			t.Fatalf("%s must be set for acceptance tests", env)
		}
	}
}
