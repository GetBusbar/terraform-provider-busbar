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
	_ resource.Resource                = (*hookResource)(nil)
	_ resource.ResourceWithConfigure   = (*hookResource)(nil)
	_ resource.ResourceWithImportState = (*hookResource)(nil)
)

// hookResource manages a routing/ranking hook: an external tap or gate reached
// over a unix socket or a webhook, wired into the request pipeline. POST /hooks
// registers, GET/PUT/DELETE /hooks/{name} read/replace/remove. The read shape
// (HookView) collapses socket/webhook into a transport{kind,target} pair and
// drops the write-only on_empty/default fields, so this resource models the
// full write surface and projects reads back onto it.
type hookResource struct {
	client *adminClient
}

// NewHookResource is the resource factory registered on the provider.
func NewHookResource() resource.Resource {
	return &hookResource{}
}

// hookModel maps busbar_hook state.
type hookModel struct {
	Name      types.String `tfsdk:"name"`
	Kind      types.String `tfsdk:"kind"`
	Socket    types.String `tfsdk:"socket"`
	Webhook   types.String `tfsdk:"webhook"`
	TimeoutMS types.Int64  `tfsdk:"timeout_ms"`
	OnError   types.String `tfsdk:"on_error"`
	Prompt    types.String `tfsdk:"prompt"`
	User      types.String `tfsdk:"user"`
	Priority  types.Int64  `tfsdk:"priority"`
	At        types.String `tfsdk:"at"`
	OnEmpty   types.String `tfsdk:"on_empty"`
	Settings  types.String `tfsdk:"settings"`
	Global    types.Bool   `tfsdk:"global"`
	Default   types.Bool   `tfsdk:"default"`
}

// hookCfg is the write-side config object (busbar HookCfg). deny_unknown_fields
// on the server means only these keys may be sent; omitempty keeps optional keys
// off the wire so the server defaults apply.
type hookCfg struct {
	Kind      string          `json:"kind"`
	Socket    *string         `json:"socket,omitempty"`
	Webhook   *string         `json:"webhook,omitempty"`
	TimeoutMS *int64          `json:"timeout_ms,omitempty"`
	OnError   *string         `json:"on_error,omitempty"`
	Prompt    *string         `json:"prompt,omitempty"`
	User      *string         `json:"user,omitempty"`
	Priority  *int64          `json:"priority,omitempty"`
	At        *string         `json:"at,omitempty"`
	OnEmpty   *string         `json:"on_empty,omitempty"`
	Settings  json.RawMessage `json:"settings,omitempty"`
	Global    *bool           `json:"global,omitempty"`
	Default   *bool           `json:"default,omitempty"`
}

// registerHookReq is the POST /hooks body.
type registerHookReq struct {
	Name   string  `json:"name"`
	Config hookCfg `json:"config"`
}

// putHookReq is the PUT /hooks/{name} body (name rides the path).
type putHookReq struct {
	Config hookCfg `json:"config"`
}

// hookView is the read/mutation response projection (busbar HookView).
type hookView struct {
	Name      string          `json:"name"`
	Kind      string          `json:"kind"`
	Transport hookTransport   `json:"transport"`
	Prompt    string          `json:"prompt"`
	User      string          `json:"user"`
	Priority  int64           `json:"priority"`
	At        *string         `json:"at"`
	OnError   string          `json:"on_error"`
	TimeoutMS int64           `json:"timeout_ms"`
	Settings  json.RawMessage `json:"settings"`
	Global    bool            `json:"global"`
}

type hookTransport struct {
	Kind   string  `json:"kind"`   // socket | webhook | none
	Target *string `json:"target"` // path or URL; null when neither set
}

func (r *hookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hook"
}

func (r *hookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A routing hook: an external tap or gate reached over a unix socket or webhook, " +
			"wired into busbar's request/ranking pipeline (POST/GET/PUT/DELETE /api/v1/admin/hooks). " +
			"Exactly one of socket or webhook must be set. The grant fields (kind, prompt, user) are " +
			"immutable once registered — changing them replaces the hook.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				Required:    true,
				Description: "Unique hook name (<= 256 chars; not a reserved terminal name). Immutable; changing it replaces the hook.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"kind": schema.StringAttribute{
				Required: true,
				Description: "Transport contract: tap (fire-and-forget, non-blocking) or gate " +
					"(blocking, may rewrite/reject). Immutable grant; changing it replaces the hook.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"socket": schema.StringAttribute{
				Optional:    true,
				Description: "Unix socket path to the hook process. Exactly one of socket or webhook.",
			},
			"webhook": schema.StringAttribute{
				Optional:    true,
				Description: "Webhook URL for the hook. Exactly one of socket or webhook.",
			},
			"timeout_ms": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Per-call timeout in milliseconds. Defaults to 1.",
			},
			"on_error": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Behavior when the hook errors/times out: a terminal (weighted, reject, first, nothing) or another hook name. Defaults to nothing.",
			},
			"prompt": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Description: "Prompt-content access grant: no, ro, or rw. Defaults to no. Immutable grant; " +
					"changing it replaces the hook. (rw is invalid on a tap.)",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"user": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Caller-identity access grant: no or ro. Defaults to no. Immutable grant; changing it replaces the hook.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"priority": schema.Int64Attribute{
				Optional:    true,
				Computed:    true,
				Description: "Ordering priority within a stage. Defaults to 0.",
			},
			"at": schema.StringAttribute{
				Optional:    true,
				Description: "Pipeline stage: request, route, attempt, or completion. Null lets busbar place it by kind.",
			},
			"on_empty": schema.StringAttribute{
				Optional:    true,
				Description: "Fallback policy when the hook yields an empty ranking: weighted, reject, or first. Write-only (not echoed by reads).",
			},
			"settings": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Opaque per-hook settings as a JSON object string (<= 64KiB, <= 256 keys). Defaults to {}.",
			},
			"global": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Whether the hook applies globally (all pools). Defaults to false. May read back true if wired via global_hooks.",
			},
			"default": schema.BoolAttribute{
				Optional:    true,
				Description: "Whether this hook is the default for its stage. Write-only (not echoed by reads).",
			},
		},
	}
}

func (r *hookResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// buildCfg projects the model onto the write-side HookCfg.
func (r *hookResource) buildCfg(m *hookModel) (hookCfg, error) {
	cfg := hookCfg{Kind: m.Kind.ValueString()}
	if !m.Socket.IsNull() && !m.Socket.IsUnknown() {
		cfg.Socket = m.Socket.ValueStringPointer()
	}
	if !m.Webhook.IsNull() && !m.Webhook.IsUnknown() {
		cfg.Webhook = m.Webhook.ValueStringPointer()
	}
	if !m.TimeoutMS.IsNull() && !m.TimeoutMS.IsUnknown() {
		cfg.TimeoutMS = m.TimeoutMS.ValueInt64Pointer()
	}
	if !m.OnError.IsNull() && !m.OnError.IsUnknown() {
		cfg.OnError = m.OnError.ValueStringPointer()
	}
	if !m.Prompt.IsNull() && !m.Prompt.IsUnknown() {
		cfg.Prompt = m.Prompt.ValueStringPointer()
	}
	if !m.User.IsNull() && !m.User.IsUnknown() {
		cfg.User = m.User.ValueStringPointer()
	}
	if !m.Priority.IsNull() && !m.Priority.IsUnknown() {
		cfg.Priority = m.Priority.ValueInt64Pointer()
	}
	if !m.At.IsNull() && !m.At.IsUnknown() {
		cfg.At = m.At.ValueStringPointer()
	}
	if !m.OnEmpty.IsNull() && !m.OnEmpty.IsUnknown() {
		cfg.OnEmpty = m.OnEmpty.ValueStringPointer()
	}
	if !m.Global.IsNull() && !m.Global.IsUnknown() {
		cfg.Global = m.Global.ValueBoolPointer()
	}
	if !m.Default.IsNull() && !m.Default.IsUnknown() {
		cfg.Default = m.Default.ValueBoolPointer()
	}
	if !m.Settings.IsNull() && !m.Settings.IsUnknown() && m.Settings.ValueString() != "" {
		raw := json.RawMessage(m.Settings.ValueString())
		if !json.Valid(raw) {
			return cfg, fmt.Errorf("settings must be a valid JSON object")
		}
		cfg.Settings = raw
	}
	return cfg, nil
}

func (r *hookResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan hookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cfg, err := r.buildCfg(&plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid hook configuration", err.Error())
		return
	}
	body := registerHookReq{Name: plan.Name.ValueString(), Config: cfg}

	httpResp, err := r.client.do(ctx, http.MethodPost, "/hooks", body, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to register hook", err.Error())
		return
	}
	// New name -> 201; a same-grant re-register -> 200. Both are success.
	if httpResp.StatusCode != http.StatusCreated && httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar rejected the hook registration",
			fmt.Sprintf("POST /api/v1/admin/hooks returned %d: %s", httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}

	var view hookView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode hook registration response", err.Error())
		return
	}
	applyHookView(&plan, &view)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hookResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state hookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	httpResp, err := r.client.do(ctx, http.MethodGet, "/hooks/"+state.Name.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read hook", err.Error())
		return
	}
	if httpResp.StatusCode == http.StatusNotFound {
		resp.State.RemoveResource(ctx)
		return
	}
	if httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar returned an error reading the hook",
			fmt.Sprintf("GET /api/v1/admin/hooks/%s returned %d: %s", state.Name.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}

	var view hookView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode hook read response", err.Error())
		return
	}
	applyHookView(&state, &view)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *hookResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan hookModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	cfg, err := r.buildCfg(&plan)
	if err != nil {
		resp.Diagnostics.AddError("Invalid hook configuration", err.Error())
		return
	}
	body := putHookReq{Config: cfg}

	httpResp, err := r.client.do(ctx, http.MethodPut, "/hooks/"+plan.Name.ValueString(), body, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to update hook", err.Error())
		return
	}
	if httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar rejected the hook update",
			fmt.Sprintf("PUT /api/v1/admin/hooks/%s returned %d: %s", plan.Name.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
		)
		return
	}

	var view hookView
	if err := httpResp.decode(&view); err != nil {
		resp.Diagnostics.AddError("Failed to decode hook update response", err.Error())
		return
	}
	applyHookView(&plan, &view)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *hookResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state hookModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	httpResp, err := r.client.do(ctx, http.MethodDelete, "/hooks/"+state.Name.ValueString(), nil, nil)
	if err != nil {
		resp.Diagnostics.AddError("Failed to delete hook", err.Error())
		return
	}
	if httpResp.StatusCode != http.StatusNoContent && httpResp.StatusCode != http.StatusNotFound {
		resp.Diagnostics.AddError(
			"busbar rejected the hook delete",
			fmt.Sprintf("DELETE /api/v1/admin/hooks/%s returned %d: %s", state.Name.ValueString(), httpResp.StatusCode, httpResp.errorMessage()),
		)
	}
}

// ImportState brings an existing hook under management by name. Write-only fields
// (on_empty, default) and the socket/webhook split are recovered from the read
// projection where possible; on_empty/default stay null after an import.
func (r *hookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

// applyHookView folds a HookView projection back onto the model. The read shape
// collapses socket/webhook into transport{kind,target}, so split it back out and
// leave the write-only fields (on_empty, default) untouched.
func applyHookView(m *hookModel, v *hookView) {
	m.Name = types.StringValue(v.Name)
	m.Kind = types.StringValue(v.Kind)
	switch v.Transport.Kind {
	case "socket":
		m.Socket = optString(v.Transport.Target)
		m.Webhook = types.StringNull()
	case "webhook":
		m.Webhook = optString(v.Transport.Target)
		m.Socket = types.StringNull()
	default:
		m.Socket = types.StringNull()
		m.Webhook = types.StringNull()
	}
	m.TimeoutMS = types.Int64Value(v.TimeoutMS)
	m.OnError = types.StringValue(v.OnError)
	m.Prompt = types.StringValue(v.Prompt)
	m.User = types.StringValue(v.User)
	m.Priority = types.Int64Value(v.Priority)
	m.At = optString(v.At)
	m.Global = types.BoolValue(v.Global)
	// settings: normalize to a compact JSON string; empty object stays as "{}".
	if len(v.Settings) == 0 || string(v.Settings) == "null" {
		m.Settings = types.StringValue("{}")
	} else {
		m.Settings = types.StringValue(string(v.Settings))
	}
}
