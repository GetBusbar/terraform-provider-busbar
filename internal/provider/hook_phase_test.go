package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestHookPhaseSchema(t *testing.T) {
	var response resource.SchemaResponse
	(&hookResource{}).Schema(context.Background(), resource.SchemaRequest{}, &response)
	if _, exists := response.Schema.Attributes["at"]; exists {
		t.Fatal("retired at attribute remains in the resource schema")
	}
	phase, ok := response.Schema.Attributes["phase"].(schema.ListAttribute)
	if !ok || !phase.Optional || !phase.Computed || !phase.ElementType.Equal(types.StringType) {
		t.Fatal("phase must be an optional/computed list of strings")
	}
}

func hookPhaseValue(stages ...string) types.List {
	values := make([]attr.Value, 0, len(stages))
	for _, stage := range stages {
		values = append(values, types.StringValue(stage))
	}
	return types.ListValueMust(types.StringType, values)
}

// Exercise real resource operations and the HTTP JSON seam. phase is the configured
// list; fires_at may differ in ordering/deduplication or expand an empty default.
func TestHookPhaseRequestsAndReadback(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, tc := range []struct {
			name       string
			phase      types.List
			configJSON string
			phaseJSON  string
			firesJSON  string
			wantPhase  types.List
		}{
			{
				name:       "configured",
				phase:      hookPhaseValue("response", "request", "response"),
				configJSON: `{"kind":"tap","module":"observer","phase":["response","request","response"]}`,
				phaseJSON:  `["response","request","response"]`,
				firesJSON:  `["request","response"]`,
				wantPhase:  hookPhaseValue("response", "request", "response"),
			},
			{
				name:       "empty",
				phase:      hookPhaseValue(),
				configJSON: `{"kind":"tap","module":"observer","phase":[]}`,
				phaseJSON:  `[]`,
				firesJSON:  `["request","candidate","routing","response"]`,
				wantPhase:  hookPhaseValue(),
			},
			{
				name:       "omitted",
				phase:      types.ListNull(types.StringType),
				configJSON: `{"kind":"tap","module":"observer"}`,
				phaseJSON:  `[]`,
				firesJSON:  `["request","candidate","routing","response"]`,
				wantPhase:  hookPhaseValue(),
			},
			{
				name:       "computed",
				phase:      types.ListUnknown(types.StringType),
				configJSON: `{"kind":"tap","module":"observer"}`,
				phaseJSON:  `[]`,
				firesJSON:  `["request","candidate","routing","response"]`,
				wantPhase:  hookPhaseValue(),
			},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				calls := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					calls++
					wantPath := adminPrefix + "/hooks/observer"
					if calls == 1 && method == http.MethodPost {
						wantPath = adminPrefix + "/hooks"
					}
					if req.URL.Path != wantPath {
						t.Errorf("path = %q, want %q", req.URL.Path, wantPath)
					}
					if calls == 1 {
						if req.Method != method {
							t.Errorf("method = %q, want %q", req.Method, method)
						}
						raw, err := io.ReadAll(req.Body)
						if err != nil {
							t.Error(err)
						}
						want := `{"config":` + tc.configJSON + `}`
						if method == http.MethodPost {
							want = `{"name":"observer","config":` + tc.configJSON + `}`
						}
						var gotJSON, wantJSON any
						if err := json.Unmarshal(raw, &gotJSON); err != nil {
							t.Error(err)
						}
						if err := json.Unmarshal([]byte(want), &wantJSON); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(gotJSON, wantJSON) {
							t.Errorf("request = %s, want %s", raw, want)
						}
					} else if req.Method != http.MethodGet {
						t.Errorf("refresh method = %q, want GET", req.Method)
					}
					if calls == 1 && method == http.MethodPost {
						w.WriteHeader(http.StatusCreated)
					}
					_, _ = io.WriteString(w, `{"name":"observer","kind":"tap","transport":{"kind":"plugin","target":"observer"},"phase":`+tc.phaseJSON+`,"fires_at":`+tc.firesJSON+`}`)
				}))
				defer server.Close()
				r := hookResource{client: newAdminClient(server.URL, "token", server.Client())}
				var schemaResponse resource.SchemaResponse
				r.Schema(ctx, resource.SchemaRequest{}, &schemaResponse)
				plan := tfsdk.Plan{Schema: schemaResponse.Schema}
				model := hookModel{Name: types.StringValue("observer"), Kind: types.StringValue("tap"), Module: types.StringValue("observer"), Phase: tc.phase}
				if diagnostics := plan.Set(ctx, &model); diagnostics.HasError() {
					t.Fatal(diagnostics)
				}
				state := tfsdk.State{Schema: schemaResponse.Schema}
				if method == http.MethodPost {
					response := resource.CreateResponse{State: state}
					r.Create(ctx, resource.CreateRequest{Plan: plan}, &response)
					if response.Diagnostics.HasError() {
						t.Fatal(response.Diagnostics)
					}
					state = response.State
				} else {
					response := resource.UpdateResponse{State: state}
					r.Update(ctx, resource.UpdateRequest{Plan: plan}, &response)
					if response.Diagnostics.HasError() {
						t.Fatal(response.Diagnostics)
					}
					state = response.State
				}
				assertPhase := func(state tfsdk.State) {
					t.Helper()
					var model hookModel
					if diagnostics := state.Get(ctx, &model); diagnostics.HasError() {
						t.Fatal(diagnostics)
					}
					if !model.Phase.Equal(tc.wantPhase) {
						t.Fatalf("phase = %s, want %s", model.Phase, tc.wantPhase)
					}
				}
				assertPhase(state)
				readResponse := resource.ReadResponse{State: state}
				r.Read(ctx, resource.ReadRequest{State: state}, &readResponse)
				if readResponse.Diagnostics.HasError() {
					t.Fatal(readResponse.Diagnostics)
				}
				assertPhase(readResponse.State)
				if calls != 2 {
					t.Fatalf("HTTP requests = %d, want mutation + read", calls)
				}
			})
		}
	}
}

func TestHookPhaseRejectsInvalidStages(t *testing.T) {
	for _, stage := range []types.String{types.StringNull(), types.StringUnknown(), types.StringValue("unknown"), types.StringValue("")} {
		model := hookModel{
			Kind:   types.StringValue("tap"),
			Module: types.StringValue("observer"),
			Phase:  types.ListValueMust(types.StringType, []attr.Value{stage}),
		}
		if _, err := (&hookResource{}).buildCfg(&model); err == nil {
			t.Fatalf("invalid phase stage %s accepted", stage)
		}
	}
}
