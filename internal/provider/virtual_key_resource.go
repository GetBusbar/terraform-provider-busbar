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

// virtualKeyResource manages a governance virtual key: POST /api/v1/admin/keys
// mints one (returning the plaintext secret exactly once), GET /keys/{id}
// refreshes its metadata, PATCH /keys/{id} adjusts the mutable caps, and
// DELETE /keys/{id} revokes it.
type virtualKeyResource struct {
	client *adminClient
}

// NewVirtualKeyResource is the resource factory registered on the provider.
func NewVirtualKeyResource() resource.Resource {
	return &virtualKeyResource{}
}

// virtualKeyModel maps busbar_virtual_key state. The secret (and the optional AWS
// secret access key) are Sensitive and only ever populated at create — the read
// API never returns them, so they are preserved across refreshes, never
// re-fetched.
type virtualKeyModel struct {
	ID                 types.String `tfsdk:"id"`
	Name               types.String `tfsdk:"name"`
	AllowedPools       types.List   `tfsdk:"allowed_pools"`
	MaxBudgetCents     types.Int64  `tfsdk:"max_budget_cents"`
	BudgetPeriod       types.String `tfsdk:"budget_period"`
	RPMLimit           types.Int64  `tfsdk:"rpm_limit"`
	TPMLimit           types.Int64  `tfsdk:"tpm_limit"`
	IssueAWSCredential types.Bool   `tfsdk:"issue_aws_credential"`
	Enabled            types.Bool   `tfsdk:"enabled"`
	CreatedAt          types.Int64  `tfsdk:"created_at"`
	Secret             types.String `tfsdk:"secret"`
	AWSAccessKeyID     types.String `tfsdk:"aws_access_key_id"`
	AWSSecretAccessKey types.String `tfsdk:"aws_secret_access_key"`
}

// createKeyReq is the POST /keys body (busbar CreateKeyReq).
type createKeyReq struct {
	Name               string   `json:"name"`
	AllowedPools       []string `json:"allowed_pools,omitempty"`
	MaxBudgetCents     *int64   `json:"max_budget_cents,omitempty"`
	BudgetPeriod       *string  `json:"budget_period,omitempty"`
	RPMLimit           *int64   `json:"rpm_limit,omitempty"`
	TPMLimit           *int64   `json:"tpm_limit,omitempty"`
	IssueAWSCredential bool     `json:"issue_aws_credential,omitempty"`
}

// updateKeyReq is the PATCH /keys/{id} body. The three cap fields use pointers so
// an omitted field leaves the stored value unchanged (the server treats absent as
// "leave alone" and JSON null as "clear"). We always send the current planned
// value, so a cleared (null) plan attribute maps to an explicit null via
// json.Marshal of a nil pointer only when the field is intentionally omitted;
// see buildUpdate for the null-clear handling.
type updateKeyReq map[string]any

// keyView is the GET/POST metadata response (busbar key_meta), plus the
// create-only secret fields.
type keyView struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	AllowedPools       []string `json:"allowed_pools"`
	MaxBudgetCents     *int64   `json:"max_budget_cents"`
	BudgetPeriod       string   `json:"budget_period"`
	RPMLimit           *int64   `json:"rpm_limit"`
	TPMLimit           *int64   `json:"tpm_limit"`
	Enabled            bool     `json:"enabled"`
	CreatedAt          int64    `json:"created_at"`
	Secret             *string  `json:"secret"`                // create only
	AWSAccessKeyID     *string  `json:"aws_access_key_id"`     // create only, issue_aws_credential
	AWSSecretAccessKey *string  `json:"aws_secret_access_key"` // create only, issue_aws_credential
}

func (r *virtualKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_virtual_key"
}

func (r *virtualKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A governance virtual key: a mintable, revocable credential with budget and " +
			"rate caps scoped to a set of pools (POST/GET/PATCH/DELETE /api/v1/admin/keys). The " +
			"plaintext secret is returned by busbar only once, at creation, and is stored in state " +
			"as a sensitive value; refreshes update metadata (budget/limits/enabled) but never the " +
			"secret. Requires `governance:` to be enabled on the gateway.",
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
				Description: "Pools this key may target. Empty/unset means unrestricted. Immutable; " +
					"changing it replaces the key (the mint spec is fixed at creation).",
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"max_budget_cents": schema.Int64Attribute{
				Optional: true,
				Description: "Spend cap in cents over the budget window (>= 0). Omit for unlimited. " +
					"Mutable via PATCH.",
			},
			"budget_period": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Budget window: one of total, daily, monthly. Defaults to total. Immutable; changing it replaces the key.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"rpm_limit": schema.Int64Attribute{
				Optional:    true,
				Description: "Requests-per-minute cap (>= 1). Omit for unlimited. Mutable via PATCH.",
			},
			"tpm_limit": schema.Int64Attribute{
				Optional:    true,
				Description: "Tokens-per-minute cap (>= 1). Omit for unlimited. Mutable via PATCH.",
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
				Computed:    true,
				Description: "Whether the key currently resolves. A key is created enabled; disable it out-of-band via the admin API.",
			},
			"created_at": schema.Int64Attribute{
				Computed:    true,
				Description: "Epoch seconds the key was minted.",
				PlanModifiers: []planmodifier.Int64{
					int64planmodifier.UseStateForUnknown(),
				},
			},
			"secret": schema.StringAttribute{
				Computed:    true,
				Sensitive:   true,
				Description: "The plaintext bearer secret (sk-bb-...). Returned only at creation; stored in state and never re-read.",
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
		resp.Diagnostics.Append(plan.AllowedPools.ElementsAs(ctx, &body.AllowedPools, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	if !plan.MaxBudgetCents.IsNull() {
		body.MaxBudgetCents = plan.MaxBudgetCents.ValueInt64Pointer()
	}
	if !plan.BudgetPeriod.IsNull() && !plan.BudgetPeriod.IsUnknown() {
		body.BudgetPeriod = plan.BudgetPeriod.ValueStringPointer()
	}
	if !plan.RPMLimit.IsNull() {
		body.RPMLimit = plan.RPMLimit.ValueInt64Pointer()
	}
	if !plan.TPMLimit.IsNull() {
		body.TPMLimit = plan.TPMLimit.ValueInt64Pointer()
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

	// Fold the metadata into state, then stamp the once-shown secrets.
	applyKeyView(ctx, &plan, &view, resp.Diagnostics.AddError)
	plan.Secret = optString(view.Secret)
	plan.AWSAccessKeyID = optString(view.AWSAccessKeyID)
	plan.AWSSecretAccessKey = optString(view.AWSSecretAccessKey)

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

	// Refresh metadata only; secrets are create-only and preserved as-is.
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

	// Only the mutable caps flow through PATCH. RequiresReplace on the immutable
	// attributes means Update only ever sees cap changes. A null plan attribute
	// where state had a value clears the cap (JSON null); a value sets it.
	body := updateKeyReq{}
	addCap(body, "max_budget_cents", plan.MaxBudgetCents, state.MaxBudgetCents)
	addCap(body, "rpm_limit", plan.RPMLimit, state.RPMLimit)
	addCap(body, "tpm_limit", plan.TPMLimit, state.TPMLimit)

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

	// Carry the create-only secrets forward from prior state.
	plan.Secret = state.Secret
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

// ImportState brings an existing key under management by id. The plaintext secret
// cannot be recovered (it is create-only), so it stays null after an import.
func (r *virtualKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

// applyKeyView folds a keyView's metadata into the model (leaving secrets alone).
func applyKeyView(ctx context.Context, m *virtualKeyModel, v *keyView, addError func(string, string)) {
	m.ID = types.StringValue(v.ID)
	m.Name = types.StringValue(v.Name)
	pools, diags := types.ListValueFrom(ctx, types.StringType, v.AllowedPools)
	if diags.HasError() {
		for _, d := range diags.Errors() {
			addError(d.Summary(), d.Detail())
		}
		return
	}
	// The server always returns an array; model an empty one as null so a config
	// that omits allowed_pools stays consistent with what comes back.
	if len(v.AllowedPools) == 0 {
		m.AllowedPools = types.ListNull(types.StringType)
	} else {
		m.AllowedPools = pools
	}
	m.MaxBudgetCents = optInt64(v.MaxBudgetCents)
	m.BudgetPeriod = types.StringValue(v.BudgetPeriod)
	m.RPMLimit = optInt64(v.RPMLimit)
	m.TPMLimit = optInt64(v.TPMLimit)
	m.Enabled = types.BoolValue(v.Enabled)
	m.CreatedAt = types.Int64Value(v.CreatedAt)
}

// addCap writes the PATCH entry for a three-state cap: unchanged -> omit;
// plan null (state had value) -> explicit JSON null (clear); plan value -> set.
func addCap(body updateKeyReq, field string, plan, state types.Int64) {
	switch {
	case plan.IsNull() && state.IsNull():
		// unchanged (both unlimited)
	case plan.IsNull():
		body[field] = nil // clear back to unlimited
	case !state.IsNull() && plan.ValueInt64() == state.ValueInt64():
		// unchanged value
	default:
		body[field] = plan.ValueInt64()
	}
}

func optString(v *string) types.String {
	if v == nil {
		return types.StringNull()
	}
	return types.StringValue(*v)
}
