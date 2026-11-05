package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestHookFallbackFieldsReadback(t *testing.T) {
	ctx := context.Background()
	var schemaResponse resource.SchemaResponse
	(&hookResource{}).Schema(ctx, resource.SchemaRequest{}, &schemaResponse)
	for _, field := range []string{"on_empty", "default"} {
		attribute := schemaResponse.Schema.Attributes[field]
		if !attribute.IsOptional() || !attribute.IsComputed() {
			t.Fatalf("%s must be optional/computed so an omitted configuration can read back the server default", field)
		}
	}
	if _, exists := schemaResponse.Schema.Attributes["signals"]; exists {
		t.Fatal("signals is outside the existing Terraform hook attribute surface")
	}

	for _, tc := range []struct {
		name        string
		fields      string
		wantOnEmpty types.String
		wantDefault types.Bool
	}{
		{"weighted", `"on_empty":"weighted","default":true`, types.StringValue("weighted"), types.BoolValue(true)},
		{"reject", `"on_empty":"reject","default":false`, types.StringValue("reject"), types.BoolValue(false)},
		{"first", `"on_empty":"first","default":true`, types.StringValue("first"), types.BoolValue(true)},
		{"defaults", `"on_empty":null,"default":false`, types.StringNull(), types.BoolValue(false)},
		{"absent_fields", `"global":false`, types.StringNull(), types.BoolValue(false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != adminPrefix+"/hooks/observer" {
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
				_, _ = io.WriteString(w, `{"name":"observer","kind":"gate","transport":{"kind":"plugin","target":"observer"},"phase":[],`+tc.fields+`}`)
			}))
			defer server.Close()
			r := hookResource{client: newAdminClient(server.URL, "token", server.Client())}
			state := tfsdk.State{Schema: schemaResponse.Schema}
			// A refresh must replace prior values, including clearing a prior policy
			// when the current response reports null or the default false.
			model := hookModel{
				Name:    types.StringValue("observer"),
				Phase:   hookPhaseValue("request"),
				OnEmpty: types.StringValue("stale-policy"),
				Default: types.BoolValue(true),
			}
			if diagnostics := state.Set(ctx, &model); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			response := resource.ReadResponse{State: state}
			r.Read(ctx, resource.ReadRequest{State: state}, &response)
			if response.Diagnostics.HasError() {
				t.Fatal(response.Diagnostics)
			}
			if diagnostics := response.State.Get(ctx, &model); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			if !model.OnEmpty.Equal(tc.wantOnEmpty) || !model.Default.Equal(tc.wantDefault) {
				t.Fatalf("readback on_empty=%s default=%s, want on_empty=%s default=%s", model.OnEmpty, model.Default, tc.wantOnEmpty, tc.wantDefault)
			}
		})
	}
}
