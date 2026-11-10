package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hashicorp/terraform-plugin-framework/attr"
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

// hookResource manages an installed signed hook plugin. Reads expose the module
// through transport{kind,target} and redact settings values, which stay in state.
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
	Module    types.String `tfsdk:"module"`
	Kind      types.String `tfsdk:"kind"`
	Socket    types.String `tfsdk:"socket"`
	Webhook   types.String `tfsdk:"webhook"`
	TimeoutMS types.Int64  `tfsdk:"timeout_ms"`
	OnError   types.String `tfsdk:"on_error"`
	Prompt    types.String `tfsdk:"prompt"`
	User      types.String `tfsdk:"user"`
	Priority  types.Int64  `tfsdk:"priority"`
	Phase     types.List   `tfsdk:"phase"`
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
	Module    string          `json:"module"`
	TimeoutMS *int64          `json:"timeout_ms,omitempty"`
	OnError   *string         `json:"on_error,omitempty"`
	Prompt    *string         `json:"prompt,omitempty"`
	User      *string         `json:"user,omitempty"`
	Priority  *int64          `json:"priority,omitempty"`
	Phase     *[]string       `json:"phase,omitempty"`
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
	Phase     []string        `json:"phase"`
	OnError   string          `json:"on_error"`
	OnEmpty   *string         `json:"on_empty"`
	Default   bool            `json:"default"`
	TimeoutMS int64           `json:"timeout_ms"`
	Settings  json.RawMessage `json:"settings"`
	Global    bool            `json:"global"`
}

type hookTransport struct {
	Kind   string  `json:"kind"`   // plugin | none
	Target *string `json:"target"` // installed module name; null when no transport
}

func (r *hookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hook"
}

func (r *hookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A routing hook backed by an installed signed plugin (POST/GET/PUT/DELETE /api/v1/admin/hooks). The grant fields kind, prompt, and user are immutable; changing them replaces the hook. Settings are sensitive and write-only on current gateways; import cannot recover them.",
		Attributes: map[string]schema.Attribute{
			"module": schema.StringAttribute{Required: true, Description: "Signed hook plugin name or alias installed on the gateway."},
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
				DeprecationMessage: "Removed in busbar 1.5; use module and plugin settings.",
				Optional:           true,
				Description:        "Unsupported since busbar 1.5; use module and plugin settings.",
			},
			"webhook": schema.StringAttribute{
				DeprecationMessage: "Removed in busbar 1.5; use module and plugin settings.",
				Optional:           true,
				Description:        "Unsupported since busbar 1.5; use module and plugin settings.",
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
			"phase": schema.ListAttribute{
				ElementType: types.StringType,
				Optional:    true,
				Computed:    true,
				Description: "Configured pipeline stages: request, candidate, routing, or response. Omitted or empty selects all four core stages. Reads preserve the configured list.",
			},
			"on_empty": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Fallback policy when the hook yields an empty ranking: weighted, reject, or first. Unset reads back null.",
			},
			"settings": schema.StringAttribute{
				Sensitive:   true,
				Optional:    true,
				Computed:    true,
				Description: "Opaque per-hook settings as a JSON object string (<= 64KiB, <= 256 keys). Defaults to {}.",
			},
			"global": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Whether the hook applies globally (all pools). Defaults to false. May read back true if attached globally.",
			},
			"default": schema.BoolAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Whether this hook is the default for its stage. Defaults to false.",
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
	cfg := hookCfg{Kind: m.Kind.ValueString(), Module: m.Module.ValueString()}
	if cfg.Module == "" {
		return cfg, fmt.Errorf("module must name an installed hook plugin")
	}
	if !m.Socket.IsNull() && !m.Socket.IsUnknown() || !m.Webhook.IsNull() && !m.Webhook.IsUnknown() {
		return cfg, fmt.Errorf("socket and webhook were removed in busbar 1.5; use module and plugin settings")
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
	if !m.Phase.IsNull() && !m.Phase.IsUnknown() {
		phase := make([]string, 0, len(m.Phase.Elements()))
		for _, element := range m.Phase.Elements() {
			stage, ok := element.(types.String)
			if !ok || stage.IsNull() || stage.IsUnknown() {
				return cfg, fmt.Errorf("phase must contain known, non-null stage names")
			}
			switch stage.ValueString() {
			case "request", "candidate", "routing", "response":
				phase = append(phase, stage.ValueString())
			default:
				return cfg, fmt.Errorf("phase stages must be request, candidate, routing, or response")
			}
		}
		cfg.Phase = &phase
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
	if !m.Settings.IsNull() && !m.Settings.IsUnknown() {
		raw := json.RawMessage(m.Settings.ValueString())
		if !json.Valid(raw) || len(raw) == 0 || rawFirstByte(raw) != '{' {
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

	httpResp, err := r.client.do(ctx, http.MethodGet, "/hooks/"+url.PathEscape(state.Name.ValueString()), nil, nil)
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

	httpResp, err := r.client.do(ctx, http.MethodPut, "/hooks/"+url.PathEscape(plan.Name.ValueString()), body, nil)
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

	httpResp, err := r.client.do(ctx, http.MethodDelete, "/hooks/"+url.PathEscape(state.Name.ValueString()), nil, nil)
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

// ImportState brings an existing hook under management by name. Settings remain
// write-only; the other attributes are recovered from the read projection.
func (r *hookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

// applyHookView folds a HookView projection back onto the model. The read shape
// collapses socket/webhook into transport{kind,target}, so split it back out.
func applyHookView(m *hookModel, v *hookView) {
	m.Name = types.StringValue(v.Name)
	m.Kind = types.StringValue(v.Kind)
	switch v.Transport.Kind {
	case "plugin":
		// Only overwrite Module when the server provides a non-nil target;
		// a nil target would null out the Required attribute.
		if v.Transport.Target != nil {
			m.Module = types.StringValue(*v.Transport.Target)
		}
		m.Socket = types.StringNull()
		m.Webhook = types.StringNull()
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
	m.OnEmpty = optString(v.OnEmpty)
	m.Default = types.BoolValue(v.Default)
	m.Prompt = types.StringValue(v.Prompt)
	m.User = types.StringValue(v.User)
	m.Priority = types.Int64Value(v.Priority)
	// Phase is Optional+Computed: preserve the configured list when it is already
	// set so that a semantically equivalent server response (e.g. expanded default
	// stages for an empty configured list) does not cause an inconsistent-result
	// error. Only populate from the server when the model has no prior value.
	if m.Phase.IsNull() || m.Phase.IsUnknown() {
		phase := make([]attr.Value, 0, len(v.Phase))
		for _, stage := range v.Phase {
			phase = append(phase, types.StringValue(stage))
		}
		m.Phase = types.ListValueMust(types.StringType, phase)
	}
	m.Global = types.BoolValue(v.Global)
	// Current gateways redact settings values. Keep the configured write-only bag.
	if len(v.Settings) != 0 {
		if m.Settings.IsNull() || m.Settings.IsUnknown() {
			m.Settings = types.StringValue(string(v.Settings))
		}
	} else if m.Settings.IsUnknown() {
		m.Settings = types.StringValue("{}")
	}
}

func rawFirstByte(raw []byte) byte {
	for _, b := range raw {
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b
		}
	}
	return 0
}
