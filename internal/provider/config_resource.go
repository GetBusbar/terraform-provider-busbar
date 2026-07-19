package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the resource satisfies the framework interfaces.
var (
	_ resource.Resource                = (*configResource)(nil)
	_ resource.ResourceWithConfigure   = (*configResource)(nil)
	_ resource.ResourceWithImportState = (*configResource)(nil)
)

// configSingletonID is the fixed import id for the one-of-a-kind config resource.
const configSingletonID = "config"

// configResource is a GitOps singleton: it owns the running config document and
// applies it wholesale. POST /api/v1/admin/config/apply installs the document and
// bumps config_version; GET /config surfaces the current version. The apply body
// is the full config document ({config, providers}) that busbar boots from — too
// broad to model as typed attributes, so the operator supplies it as a JSON
// string and this resource tracks the resulting config_version.
//
// Apply semantics: Create == apply, Update == re-apply. Because busbar has no
// "unapply", Delete is a documented no-op: it drops the resource from Terraform
// state without mutating the gateway (the running config stays live until the
// next apply/reload/restart). config_version is process-local and resets to 0 on
// a gateway restart.
type configResource struct {
	client *adminClient
}

// NewConfigResource is the resource factory registered on the provider.
func NewConfigResource() resource.Resource {
	return &configResource{}
}

// configModel maps busbar_config state.
type configModel struct {
	ID            types.String `tfsdk:"id"`
	Document      types.String `tfsdk:"document"`
	ConfigVersion types.Int64  `tfsdk:"config_version"`
}

// applyConfigView is the POST /config/apply response (busbar ConfigApplyView).
type applyConfigView struct {
	Applied       bool   `json:"applied"`
	ConfigVersion int64  `json:"config_version"`
	Note          string `json:"note"`
}

// effectiveConfigView is the GET /config response; only its version is tracked.
type effectiveConfigView struct {
	Version int64 `json:"version"`
}

func (r *configResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_config"
}

func (r *configResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A GitOps singleton that owns the running busbar config and applies it wholesale " +
			"(POST /api/v1/admin/config/apply). Manage at most ONE of these per gateway. The document " +
			"is the full JSON config payload busbar boots from ({\"config\": {...}, \"providers\": {...}}); " +
			"applying it bumps config_version. Applies are LIVE-ONLY by default — they revert to disk " +
			"truth on the next reload or restart unless the gateway persists an overlay. Destroying this " +
			"resource is a no-op on the gateway (there is no unapply); it only drops Terraform's tracking.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Fixed singleton id (always \"config\").",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"document": schema.StringAttribute{
				Required: true,
				Description: "The full config apply document as a JSON string: {\"config\": {DeployCfg}, " +
					"\"providers\": {name: ProviderDef}}. The `config` key is required; `providers` is optional. " +
					"This is the write model (the boot-file shape), NOT the redacted read projection returned by GET /config.",
			},
			"config_version": schema.Int64Attribute{
				Computed:    true,
				Description: "The monotonic config version after the last apply. Process-local; resets to 0 on gateway restart.",
			},
		},
	}
}

func (r *configResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected resource configure type",
			fmt.Sprintf("Expected *providerData, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	r.client = data.Admin
}

// apply POSTs the document to /config/apply and returns the new config_version.
func (r *configResource) apply(ctx context.Context, document string, diags interface {
	AddError(string, string)
}) (int64, bool) {
	var payload json.RawMessage = json.RawMessage(document)
	if !json.Valid(payload) {
		diags.AddError("Invalid config document", "document must be valid JSON")
		return 0, false
	}

	httpResp, err := r.client.do(ctx, http.MethodPost, "/config/apply", payload, nil)
	if err != nil {
		diags.AddError("Failed to apply config", err.Error())
		return 0, false
	}
	if httpResp.StatusCode != http.StatusOK {
		diags.AddError(
			"busbar rejected the config apply",
			fmt.Sprintf("POST /api/v1/admin/config/apply returned %d: %s", httpResp.StatusCode, httpResp.errorMessage()),
		)
		return 0, false
	}

	var view applyConfigView
	if err := httpResp.decode(&view); err != nil {
		diags.AddError("Failed to decode config apply response", err.Error())
		return 0, false
	}
	return view.ConfigVersion, true
}

func (r *configResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan configModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	version, ok := r.apply(ctx, plan.Document.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	plan.ID = types.StringValue(configSingletonID)
	plan.ConfigVersion = types.Int64Value(version)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *configResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state configModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The apply document is the operator's desired input, which the gateway does
	// not round-trip (GET /config is a redacted projection), so keep `document`
	// as-is and refresh only config_version — the observable effect of an apply.
	httpResp, err := r.client.do(ctx, http.MethodGet, "/config", nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read config", err.Error())
		return
	}
	if httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar returned an error reading config",
			fmt.Sprintf("GET /api/v1/admin/config returned %d: %s", httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}

	var view effectiveConfigView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode config read response", err.Error())
		return
	}
	if state.ID.IsNull() {
		state.ID = types.StringValue(configSingletonID)
	}
	state.ConfigVersion = types.Int64Value(view.Version)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *configResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan configModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	version, ok := r.apply(ctx, plan.Document.ValueString(), &resp.Diagnostics)
	if !ok {
		return
	}
	plan.ID = types.StringValue(configSingletonID)
	plan.ConfigVersion = types.Int64Value(version)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete is a documented no-op: busbar has no "unapply". The running config stays
// live; only Terraform's tracking is dropped.
func (r *configResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

// ImportState adopts the singleton. The document cannot be recovered (GET /config
// is a redacted, non-round-trippable projection), so the operator must supply the
// matching `document` in config after import; only config_version is read live.
func (r *configResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
