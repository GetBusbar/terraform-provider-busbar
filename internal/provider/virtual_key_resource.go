package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the resource satisfies the framework interfaces.
var (
	_ resource.Resource                = (*virtualKeyResource)(nil)
	_ resource.ResourceWithConfigure   = (*virtualKeyResource)(nil)
	_ resource.ResourceWithImportState = (*virtualKeyResource)(nil)
)

// virtualKeyResource manages a governance virtual key against the busbar 1.5.0
// admin API. A 1.5.0 key is PURE AUTH: a busbar-signed expiring token, returned
// exactly once at mint, optionally bound to a `groups:` bucket (all budget/rate
// enforcement lives on the group, not the key). POST /api/v1/admin/keys mints,
// GET /keys/{id} refreshes metadata, PATCH /keys/{id} flips enabled or
// rebinds/unbinds the group, and DELETE /keys/{id} revokes (tombstones) it.
type virtualKeyResource struct {
	client *adminClient
}

// NewVirtualKeyResource is the resource factory registered on the provider.
func NewVirtualKeyResource() resource.Resource {
	return &virtualKeyResource{}
}

// virtualKeyModel maps busbar_virtual_key state. The signed token (and the
// optional AWS secret access key) are Sensitive and only ever populated at
// create — the read API never returns them, so they are preserved across
// refreshes, never re-fetched.
type virtualKeyModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	AllowedPools       types.List   `tfsdk:"allowed_pools"`
	Group              types.String `tfsdk:"group"`
	Parent             types.String `tfsdk:"parent"`
	ExpiresIn          types.String `tfsdk:"expires_in"`
	ExpiresAt          types.Int64  `tfsdk:"expires_at"`
	Labels             types.Map    `tfsdk:"labels"`
	IssueAWSCredential types.Bool   `tfsdk:"issue_aws_credential"`
	Enabled            types.Bool   `tfsdk:"enabled"`
	State              types.String `tfsdk:"state"`
	CreatedAt          types.Int64  `tfsdk:"created_at"`
	GroupProvisioned   types.Bool   `tfsdk:"group_provisioned"`
	Token              types.String `tfsdk:"token"`
	AWSAccessKeyID     types.String `tfsdk:"aws_access_key_id"`
	AWSSecretAccessKey types.String `tfsdk:"aws_secret_access_key"`
}

// createKeyReq is the POST /keys body (busbar 1.5.0 CreateKeyReq). The server is
// #[serde(deny_unknown_fields)], so only these keys may be sent; the retired
// 1.4.x cap fields (max_budget_cents/rpm_limit/tpm_limit/budget_period) are gone.
type createKeyReq struct {
	Name               string            `json:"name"`
	AllowedPools       []string          `json:"allowed_pools,omitempty"`
	Group              *string           `json:"group,omitempty"`
	Parent             *string           `json:"parent,omitempty"`
	ExpiresIn          *string           `json:"expires_in,omitempty"`
	ExpiresAt          *int64            `json:"expires_at,omitempty"`
	Labels             map[string]string `json:"labels,omitempty"`
	IssueAWSCredential bool              `json:"issue_aws_credential,omitempty"`
}

// updateKeyReq is the PATCH /keys/{id} body (busbar 1.5.0 UpdateKeyReq): only
// `enabled` and `group` are mutable. `group` is three-state (absent = unchanged,
// null = unbind, value = rebind), which a map models exactly.
type updateKeyReq map[string]any

// keyView is the GET/PATCH metadata response (busbar 1.5.0 KeyView), plus the
// create-only CreatedKeyView fields (token, expires_at, group_provisioned, AWS
// credential pair).
type keyView struct {
	ID               string            `json:"id"`
	Name             string            `json:"name"`
	AllowedPools     []string          `json:"allowed_pools"`
	Group            *string           `json:"group"`
	Enabled          bool              `json:"enabled"`
	State            string            `json:"state"`
	CreatedAt        int64             `json:"created_at"`
	Labels           map[string]string `json:"labels"`
	ExpiresAt        *int64            `json:"expires_at"`            // create only
	GroupProvisioned *bool             `json:"group_provisioned"`     // create only
	Token            *string           `json:"token"`                 // create only
	AWSAccessKeyID   *string           `json:"aws_access_key_id"`     // create only, issue_aws_credential
	AWSSecretKey     *string           `json:"aws_secret_access_key"` // create only, issue_aws_credential
}

func (r *virtualKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_virtual_key"
}

func (r *virtualKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A governance virtual key (busbar >= 1.5.0): a mintable, revocable, EXPIRING " +
			"signed-token credential, optionally bound to a `groups:` bucket that carries all budget " +
			"and rate enforcement (POST/GET/PATCH/DELETE /api/v1/admin/keys). The signed token is " +
			"returned by busbar only once, at creation, and is stored in state as a sensitive value; " +
			"refreshes update metadata (group/enabled/state) but never the token. Requires governance " +
			"to be enabled on the gateway.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:    true,
				Description: "Server-assigned key id (e.g. vk_0123456789abcdef).",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Human-readable label (<= 256 chars). Immutable; changing it replaces the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"allowed_pools": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Pools this key may target. Omitted/null means ALL pools; an explicit empty " +
					"list means NO pools. Immutable; changing it replaces the key.",
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"group": schema.StringAttribute{
				Optional: true,
				Description: "The `groups:` bucket this key binds to (at most one); all budget/rate " +
					"enforcement flows through the group. The group must already exist unless `parent` is " +
					"set (auto-provision). Omit for an authed-but-unlimited key. Mutable via PATCH: " +
					"changing it rebinds, removing it unbinds.",
			},
			"parent": schema.StringAttribute{
				Optional: true,
				Description: "Auto-provision target: the EXISTING parent group under which `group` is " +
					"created as a leaf when it does not yet exist. Write-only mint directive (never echoed " +
					"by reads). Changing it replaces the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"expires_in": schema.StringAttribute{
				Optional: true,
				Description: "Token lifetime as a duration string (e.g. `7d`, `24h`, `30m`, `3600s`); the " +
					"token's expiry is mint-time + this. Mutually exclusive with `expires_at`. Omitting both " +
					"applies the server default TTL. Write-only mint directive; changing it replaces the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"expires_at": schema.Int64Attribute{
				Optional: true,
				Computed: true,
				Description: "Token expiry as absolute Unix seconds. May be set at mint (mutually exclusive " +
					"with `expires_in`) and is always computed from the mint response. Changing it replaces the key.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.RequiresReplace(),
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"labels": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				Description: "Mint-time labels echoed onto this key's metric series (Prometheus-safe names; " +
					"never interpreted by enforcement). Immutable; changing them replaces the key.",
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
			"issue_aws_credential": schema.BoolAttribute{
				Optional: true,
				Description: "When true, also mint an AWS-style access-key-id + secret access key " +
					"(SigV4/Bedrock inbound auth). Both are returned only at creation. Immutable.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.RequiresReplace(),
				},
			},
			"enabled": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Description: "Whether the key currently resolves. Defaults to true at mint. Mutable via " +
					"PATCH (false = reversible disable; the key's `state` reads \"disabled\").",
			},
			"state": schema.StringAttribute{
				Computed: true,
				Description: "Lifecycle state derived by the server: active, disabled, revoked, or " +
					"tombstoned. A tombstoned/revoked key is treated as gone and planned for recreation.",
			},
			"created_at": schema.Int64Attribute{
				Computed:    true,
				Description: "Epoch seconds the key was minted.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"group_provisioned": schema.BoolAttribute{
				Computed: true,
				Description: "Whether the mint auto-provisioned its bound group leaf (self-service). " +
					"Known only at creation.",
				PlanModifiers: []planmodifier.Bool{
					boolplanmodifier.UseStateForUnknown(),
				},
			},
			"token": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "The busbar-signed bearer token (bbk_...) — the key credential. Returned only at creation; stored in state and never re-read.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"aws_access_key_id": schema.StringAttribute{
				Computed:    true,
				Description: "AWS-style access key id, when issue_aws_credential is true. Returned only at creation.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"aws_secret_access_key": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "AWS-style secret access key, when issue_aws_credential is true. Returned only at creation; stored in state and never re-read.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *virtualKeyResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *virtualKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan virtualKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := createKeyReq{
		Name:               plan.Name.ValueString(),
		IssueAWSCredential: plan.IssueAWSCredential.ValueBool(),
	}
	if !plan.AllowedPools.IsNull() && !plan.AllowedPools.IsUnknown() {
		pools := []string{}
		resp.Diagnostics.Append(plan.AllowedPools.ElementsAs(ctx, &pools, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.AllowedPools = pools
	}
	if !plan.Group.IsNull() && !plan.Group.IsUnknown() {
		body.Group = plan.Group.ValueStringPointer()
	}
	if !plan.Parent.IsNull() && !plan.Parent.IsUnknown() {
		body.Parent = plan.Parent.ValueStringPointer()
	}
	if !plan.ExpiresIn.IsNull() && !plan.ExpiresIn.IsUnknown() {
		body.ExpiresIn = plan.ExpiresIn.ValueStringPointer()
	}
	if !plan.ExpiresAt.IsNull() && !plan.ExpiresAt.IsUnknown() {
		body.ExpiresAt = plan.ExpiresAt.ValueInt64Pointer()
	}
	if !plan.Labels.IsNull() && !plan.Labels.IsUnknown() {
		labels := map[string]string{}
		resp.Diagnostics.Append(plan.Labels.ElementsAs(ctx, &labels, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.Labels = labels
	}

	httpResp, err := r.client.do(ctx, http.MethodPost, "/keys", body, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to create virtual key", err.Error())
		return
	}
	if httpResp.StatusCode != http.StatusCreated {
		resp.Diagnostics.AddError(
			"busbar rejected the virtual key create",
			fmt.Sprintf("POST /api/v1/admin/keys returned %d: %s", httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}

	var view keyView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode virtual key create response", err.Error())
		return
	}

	// A mint is always enabled; honor an explicit `enabled = false` in config
	// with an immediate PATCH so the applied state matches the plan.
	if !plan.Enabled.IsNull() && !plan.Enabled.IsUnknown() && !plan.Enabled.ValueBool() {
		patchResp, err := r.client.do(ctx, http.MethodPatch, "/keys/"+view.ID, updateKeyReq{"enabled": false}, nil)
		if err != nil {
			resp.Diagnostics.AddError("Failed to disable virtual key after create", err.Error())
			return
		}
		if patchResp.StatusCode != http.StatusOK {
			resp.Diagnostics.AddError(
				"busbar rejected disabling the virtual key after create",
				fmt.Sprintf("PATCH /api/v1/admin/keys/%s returned %d: %s", view.ID, patchResp.StatusCode, patchResp.errorMessage()),
			)
			return
		}
		mintOnly := view // PATCH's KeyView drops the mint-only fields; keep them.
		if err := patchResp.decode(&view); err != nil {
			resp.Diagnostics.AddError("Failed to decode virtual key disable response", err.Error())
			return
		}
		view.Token = mintOnly.Token
		view.ExpiresAt = mintOnly.ExpiresAt
		view.GroupProvisioned = mintOnly.GroupProvisioned
		view.AWSAccessKeyID = mintOnly.AWSAccessKeyID
		view.AWSSecretKey = mintOnly.AWSSecretKey
	}

	// Fold the metadata into state, then stamp the once-shown mint-only fields.
	applyKeyView(ctx, &plan, &view, resp.Diagnostics.AddError)
	plan.Token = optString(view.Token)
	plan.ExpiresAt = optInt64(view.ExpiresAt)
	plan.GroupProvisioned = optBool(view.GroupProvisioned)
	plan.AWSAccessKeyID = optString(view.AWSAccessKeyID)
	plan.AWSSecretAccessKey = optString(view.AWSSecretKey)

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *virtualKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state virtualKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	httpResp, err := r.client.do(ctx, http.MethodGet, "/keys/"+state.ID.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read virtual key", err.Error())
		return
	}
	// Key deleted out-of-band: drop it from state so Terraform plans a recreate.
	if httpResp.StatusCode == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar returned an error reading the virtual key",
			fmt.Sprintf("GET /api/v1/admin/keys/%s returned %d: %s", state.ID.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}

	var view keyView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode virtual key read response", err.Error())
		return
	}
	// 1.5.0 keeps deleted/revoked rows readable for audit (state tombstoned /
	// revoked); both are permanent, so treat them as gone and plan a recreate.
	if view.State == "tombstoned" || view.State == "revoked" {
		resp.State.RemoveResource(ctx)
		return
	}

	// Refresh metadata only; the token and AWS credential are create-only and
	// preserved as-is.
	applyKeyView(ctx, &state, &view, resp.Diagnostics.AddError)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *virtualKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state virtualKeyModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// 1.5.0's mutable surface is auth-shaped only: `enabled` and the group
	// binding. RequiresReplace on everything else means Update only sees these.
	body := updateKeyReq{}
	if !plan.Enabled.IsUnknown() && !plan.Enabled.IsNull() && !plan.Enabled.Equal(state.Enabled) {
		body["enabled"] = plan.Enabled.ValueBool()
	}
	switch {
	case plan.Group.IsUnknown() || plan.Group.Equal(state.Group):
		// unchanged
	case plan.Group.IsNull():
		body["group"] = nil // unbind (JSON null)
	default:
		body["group"] = plan.Group.ValueString() // rebind
	}

	if len(body) > 0 {
		httpResp, err := r.client.do(ctx, http.MethodPatch, "/keys/"+state.ID.ValueString(), body, nil)
		if err != nil {
			resp.Diagnostics.AddError("Failed to update virtual key", err.Error())
			return
		}
		if httpResp.StatusCode != http.StatusOK {
			resp.Diagnostics.AddError(
				"busbar rejected the virtual key update",
				fmt.Sprintf("PATCH /api/v1/admin/keys/%s returned %d: %s", state.ID.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
			)
			return
		}
	}

	// Re-read to fold the authoritative post-update metadata into state.
	httpResp, err := r.client.do(ctx, http.MethodGet, "/keys/"+state.ID.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to re-read virtual key after update", err.Error())
		return
	}
	if httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar returned an error re-reading the virtual key after update",
			fmt.Sprintf("GET /api/v1/admin/keys/%s returned %d: %s", state.ID.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}
	var view keyView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode virtual key re-read response", err.Error())
		return
	}

	// Carry the create-only fields forward from prior state.
	plan.Token = state.Token
	plan.ExpiresAt = state.ExpiresAt
	plan.GroupProvisioned = state.GroupProvisioned
	plan.AWSAccessKeyID = state.AWSAccessKeyID
	plan.AWSSecretAccessKey = state.AWSSecretAccessKey
	applyKeyView(ctx, &plan, &view, resp.Diagnostics.AddError)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *virtualKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state virtualKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	httpResp, err := r.client.do(ctx, http.MethodDelete, "/keys/"+state.ID.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to delete virtual key", err.Error())
		return
	}
	// 204 = revoked; 404 = already gone (treat as success, converge to absent).
	if httpResp.StatusCode != http.StatusNoContent && httpResp.StatusCode != http.StatusNotFound {
		resp.Diagnostics.AddError(
			"busbar rejected the virtual key delete",
			fmt.Sprintf("DELETE /api/v1/admin/keys/%s returned %d: %s", state.ID.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
		)
	}
}

// ImportState brings an existing key under management by id. The signed token
// cannot be recovered (it is create-only), so it stays null after an import.
func (r *virtualKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyKeyView folds a keyView's metadata into the model (leaving the mint-only
// token/expiry/AWS fields alone).
func applyKeyView(ctx context.Context, m *virtualKeyModel, v *keyView, addError func(string, string)) {
	m.ID = types.StringValue(v.ID)
	m.Name = types.StringValue(v.Name)
	// 1.5.0 semantics: null = all pools, [] = no pools. Preserve the distinction.
	if v.AllowedPools == nil {
		m.AllowedPools = types.ListNull(types.StringType)
	} else {
		pools, diags := types.ListValueFrom(ctx, types.StringType, v.AllowedPools)
		if diags.HasError() {
			for _, d := range diags.Errors() {
				addError(d.Summary(), d.Detail())
			}
			return
		}
		m.AllowedPools = pools
	}
	m.Group = optString(v.Group)
	// The server echoes an empty labels object for an unlabeled key; keep an
	// omitted config consistent by modeling empty as null.
	if len(v.Labels) == 0 {
		m.Labels = types.MapNull(types.StringType)
	} else {
		labels, diags := types.MapValueFrom(ctx, types.StringType, v.Labels)
		if diags.HasError() {
			for _, d := range diags.Errors() {
				addError(d.Summary(), d.Detail())
			}
			return
		}
		m.Labels = labels
	}
	m.Enabled = types.BoolValue(v.Enabled)
	m.State = types.StringValue(v.State)
	m.CreatedAt = types.Int64Value(v.CreatedAt)
}

func optString(v *string) types.String {
	if v == nil {
		return types.StringNull()
	}
	return types.StringValue(*v)
}

func optBool(v *bool) types.Bool {
	if v == nil {
		return types.BoolNull()
	}
	return types.BoolValue(*v)
}
