package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GetBusbar/terraform-provider-busbar/internal/apiclient"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
)

func TestInfoReadbackUsesCurrentGeneratedOperation(t *testing.T) {
	for _, tc := range []struct {
		name, version, uptime string
		wantError             bool
	}{
		{"nullable_uptime", "7", "null", false},
		{"exact_signed_limit", "9223372036854775807", "9007199254740993", false},
		{"unsigned_overflow", "18446744073709551615", "null", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != adminPrefix+"/info" || req.Header.Get("x-admin-token") != "credential" {
					t.Errorf("unexpected generated info request: %s %s", req.Method, req.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"version":"1.6.0","build":{"auth_modules":["admin-tokens"],"hook_plugins":["ranking"],"weighted_floor":true},"uptime_seconds":%s,"started_at":null,"process_boot_nonce":null,"topology":{"pools":2,"models":3,"providers":4},"config_persistence":true,"config_version":%s}`, tc.uptime, tc.version)
			}))
			defer server.Close()
			client, err := apiclient.NewClientWithResponses(server.URL, apiclient.WithHTTPClient(server.Client()), apiclient.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
				req.Header.Set("x-admin-token", "credential")
				return nil
			}))
			if err != nil {
				t.Fatal(err)
			}
			d := infoDataSource{client: client}
			var schemaResponse datasource.SchemaResponse
			d.Schema(ctx, datasource.SchemaRequest{}, &schemaResponse)
			response := datasource.ReadResponse{State: tfsdk.State{Schema: schemaResponse.Schema}}
			d.Read(ctx, datasource.ReadRequest{}, &response)
			if response.Diagnostics.HasError() != tc.wantError {
				t.Fatalf("info diagnostics: %v", response.Diagnostics)
			}
			if tc.wantError {
				return
			}
			var state infoModel
			if diagnostics := response.State.Get(ctx, &state); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			if state.Version.ValueString() != "1.6.0" || !state.WeightedFloor.ValueBool() || state.Pools.ValueInt64() != 2 || state.ConfigVersion.String() != tc.version {
				t.Fatalf("info state changed: %+v", state)
			}
			if tc.uptime == "null" {
				if !state.UptimeSeconds.IsNull() {
					t.Fatalf("null uptime changed: %s", state.UptimeSeconds)
				}
			} else if state.UptimeSeconds.ValueInt64() != 9007199254740993 {
				t.Fatalf("exact uptime changed: %s", state.UptimeSeconds)
			}
		})
	}
}
