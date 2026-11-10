package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

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
	_ resource.Resource                   = (*virtualKeyResource)(nil)
	_ resource.ResourceWithConfigure      = (*virtualKeyResource)(nil)
	_ resource.ResourceWithImportState    = (*virtualKeyResource)(nil)
	_ resource.ResourceWithValidateConfig = (*virtualKeyResource)(nil)
)

// virtualKeyResource manages a governance virtual key: POST /api/v1/admin/keys
// mints one (returning the plaintext secret exactly once), GET /keys/{id}
// refreshes its metadata, and
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
	Name               string    `json:"name"`
	AllowedPools       *[]string `json:"allowed_pools,omitempty"`
	IssueAWSCredential bool      `json:"issue_aws_credential,omitempty"`
}

// keyView is the GET/POST metadata response (busbar key_meta), plus the
// create-only secret fields.
type keyView struct {
	ID                 string   `json:"id"`
	State              string   `json:"state"`
	Name               string   `json:"name"`
	AllowedPools       []string `json:"allowed_pools"`
	MaxBudgetCents     *int64   `json:"max_budget_cents"`
	BudgetPeriod       string   `json:"budget_period"`
	RPMLimit           *int64   `json:"rpm_limit"`
	TPMLimit           *int64   `json:"tpm_limit"`
	Enabled            bool     `json:"enabled"`
	CreatedAt          int64    `json:"created_at"`
	Secret             *string  `json:"token"`                 // create only
	AWSAccessKeyID     *string  `json:"aws_access_key_id"`     // create only, issue_aws_credential
	AWSSecretAccessKey *string  `json:"aws_secret_access_key"` // create only, issue_aws_credential
}

func (r *virtualKeyResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_virtual_key"
}

func (r *virtualKeyResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A signed, expiring busbar virtual key scoped to pools. The bearer token is returned once at creation and stored as a sensitive value. Budget and rate limits belong to gateway groups in busbar 1.5 and later; legacy key limit attributes are rejected.",
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
				Description: "Pools this key may target. Unset means all pools; an empty list means no pools. Immutable; " +
					"changing it replaces the key (the mint spec is fixed at creation).",
				PlanModifiers: []planmodifier.List{
					listplanmodifier.RequiresReplace(),
				},
			},
			"max_budget_cents": schema.Int64Attribute{
				DeprecationMessage: "Removed by busbar 1.5; configure limits on gateway groups.",
				Optional:           true,
				Description:        "Unsupported since busbar 1.5; configure spend limits on gateway groups.",
			},
			"budget_period": schema.StringAttribute{
				DeprecationMessage: "Removed by busbar 1.5; configure limits on gateway groups.",
				Optional:           true,
				Computed:           true,
				Description:        "Unsupported since busbar 1.5; configure budget windows on gateway groups.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"rpm_limit": schema.Int64Attribute{
				DeprecationMessage: "Removed by busbar 1.5; configure limits on gateway groups.",
				Optional:           true,
				Description:        "Unsupported since busbar 1.5; configure rate limits on gateway groups.",
			},
			"tpm_limit": schema.Int64Attribute{
				DeprecationMessage: "Removed by busbar 1.5; configure limits on gateway groups.",
				Optional:           true,
				Description:        "Unsupported since busbar 1.5; configure rate limits on gateway groups.",
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
				Description: "The signed bearer token. Returned only at creation; stored in state and never re-read.",
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

	if !validateKeyLimits(&plan, resp.Diagnostics.AddError) {
		return
	}
	body := createKeyReq{
		Name:               plan.Name.ValueString(),
		IssueAWSCredential: plan.IssueAWSCredential.ValueBool(),
	}
	if !plan.AllowedPools.IsNull() && !plan.AllowedPools.IsUnknown() {
		var pools []string
		resp.Diagnostics.Append(plan.AllowedPools.ElementsAs(ctx, &pools, false)...)
		if pools == nil {
			pools = []string{}
		}
		body.AllowedPools = &pools
		if resp.Diagnostics.HasError() {
			return
		}
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

	if view.ID == "" {
		resp.Diagnostics.AddError("Invalid virtual key create response", "The gateway omitted the key ID.")
		return
	}
	// Fold the metadata into state, then stamp the once-shown secrets.
	applyKeyView(ctx, &plan, &view, resp.Diagnostics.AddError)
	plan.Secret = optString(view.Secret)
	plan.AWSAccessKeyID = optString(view.AWSAccessKeyID)
	plan.AWSSecretAccessKey = optString(view.AWSSecretAccessKey)
	if err := validateMintedCredentials(&view, plan.IssueAWSCredential.ValueBool()); err != nil {
		resp.Diagnostics.AddError("Incomplete virtual key create response", err.Error())
	}
	// Keep the returned ID even on an incomplete credential response so Terraform can clean up the minted key.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *virtualKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state virtualKeyModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	httpResp, err := r.client.do(ctx, http.MethodGet, "/keys/"+url.PathEscape(state.ID.ValueString()), nil, nil)
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

	if view.State == "tombstoned" {
		resp.State.RemoveResource(ctx)
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

	if !validateKeyLimits(&plan, resp.Diagnostics.AddError) {
		return
	}

	// Re-read to fold the authoritative post-update metadata into state.
	httpResp, err := r.client.do(ctx, http.MethodGet, "/keys/"+url.PathEscape(state.ID.ValueString()), nil, nil)
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

	httpResp, err := r.client.do(ctx, http.MethodDelete, "/keys/"+url.PathEscape(state.ID.ValueString()), nil, nil)
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
	// Null grants all pools; an explicit empty array grants none.
	if v.AllowedPools == nil {
		m.AllowedPools = types.ListNull(types.StringType)
	} else {
		m.AllowedPools = pools
	}
	// Deprecated limit attributes are rejected by ValidateConfig and never sent to
	// the server. Do not refresh them from the API: writing back a non-null server
	// value when config is null would cause a perpetual diff.
	m.Enabled = types.BoolValue(v.Enabled)
	m.CreatedAt = types.Int64Value(v.CreatedAt)
}

func optString(v *string) types.String {
	if v == nil {
		return types.StringNull()
	}
	return types.StringValue(*v)
}

// ValidateConfig rejects removed pre-1.5 key limits before any remote mutation.
func (r *virtualKeyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg virtualKeyModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for name, present := range map[string]bool{
		"max_budget_cents": !cfg.MaxBudgetCents.IsNull() && !cfg.MaxBudgetCents.IsUnknown(),
		"rpm_limit":        !cfg.RPMLimit.IsNull() && !cfg.RPMLimit.IsUnknown(),
		"tpm_limit":        !cfg.TPMLimit.IsNull() && !cfg.TPMLimit.IsUnknown(),
		"budget_period":    !cfg.BudgetPeriod.IsNull() && !cfg.BudgetPeriod.IsUnknown(),
	} {
		if present {
			resp.Diagnostics.AddAttributeError(path.Root(name), "Unsupported key limit", "busbar 1.5 and later enforce budgets and rates on groups, not keys. Remove this legacy attribute and configure limits on the gateway group.")
		}
	}
}

func validateKeyLimits(m *virtualKeyModel, addError func(string, string)) bool {
	set := (!m.MaxBudgetCents.IsNull() && !m.MaxBudgetCents.IsUnknown()) ||
		(!m.RPMLimit.IsNull() && !m.RPMLimit.IsUnknown()) ||
		(!m.TPMLimit.IsNull() && !m.TPMLimit.IsUnknown()) ||
		(!m.BudgetPeriod.IsNull() && !m.BudgetPeriod.IsUnknown())
	if set {
		addError("Unsupported key limits", "busbar 1.5 and later enforce budgets and rates on groups. Remove legacy key limit attributes.")
		return false
	}
	return true
}

func validateMintedCredentials(v *keyView, issueAWS bool) error {
	if v.Secret == nil || *v.Secret == "" {
		return fmt.Errorf("the gateway omitted the once-only bearer token; the key cannot be used")
	}
	if issueAWS && (v.AWSAccessKeyID == nil || *v.AWSAccessKeyID == "" || v.AWSSecretAccessKey == nil || *v.AWSSecretAccessKey == "") {
		return fmt.Errorf("the gateway omitted requested once-only AWS credentials")
	}
	return nil
}
