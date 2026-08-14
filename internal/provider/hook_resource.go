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

// hookResource manages a routing/ranking hook against the busbar 1.5.0 admin
// API: a tap or gate backed by a signed `kind: hook` plugin (named by `plugin`),
// wired into the request pipeline. POST /hooks registers, GET/PUT/DELETE
// /hooks/{name} read/replace/remove. The read shape (HookView) projects the
// plugin as a transport{kind:"plugin",target:<plugin name>} pair and drops the
// write-only on_empty/default fields, so this resource models the full write
// surface and projects reads back onto it.
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
	Plugin    types.String `tfsdk:"plugin"`
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
	Plugin    string          `json:"plugin"`
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
//
// Since busbar 1.5.3 the settings bag is REDACTED on every read: HookView
// carries `settings_keys` (the sorted KEY NAMES, no values) instead of the old
// `settings` object, because a hook's settings bag is a SecretRef carrier by
// design. The provider therefore keeps the last-applied `settings` value in
// state and uses `settings_keys` only to detect drift at key granularity.
type hookView struct {
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Transport    hookTransport `json:"transport"`
	Prompt       string        `json:"prompt"`
	User         string        `json:"user"`
	Priority     int64         `json:"priority"`
	At           *string       `json:"at"`
	OnError      string        `json:"on_error"`
	TimeoutMS    int64         `json:"timeout_ms"`
	SettingsKeys []string      `json:"settings_keys"`
	Global       bool          `json:"global"`
}

type hookTransport struct {
	Kind   string  `json:"kind"`   // plugin | none
	Target *string `json:"target"` // the plugin name; null when kind is none
}

func (r *hookResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_hook"
}

func (r *hookResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A routing hook (busbar >= 1.5.0): a tap or gate backed by a signed `kind: hook` " +
			"plugin, wired into busbar's request/ranking pipeline (POST/GET/PUT/DELETE " +
			"/api/v1/admin/hooks). The grant fields (kind, prompt, user) are immutable once " +
			"registered — changing them replaces the hook.",
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
			"plugin": schema.StringAttribute{
				Required: true,
				Description: "The signed `kind: hook` plugin this hook dispatches to (its NAME from the " +
					"gateway's plugin catalog, e.g. a compiled-in plugin such as `ranking`).",
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
				Optional: true,
				Computed: true,
				Description: "Opaque per-hook settings as a JSON object string (<= 64KiB, <= 256 keys). Defaults to {}. " +
					"The bag may carry SecretRefs, so busbar redacts it on every read (only the key names are " +
					"echoed, as `settings_keys`); the provider keeps the last value it applied and detects " +
					"drift by key names. Values changed outside Terraform with the SAME key set are invisible.",
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
	cfg := hookCfg{Kind: m.Kind.ValueString(), Plugin: m.Plugin.ValueString()}
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
	// settings is never echoed back (redacted to settings_keys since busbar
	// 1.5.3), so state keeps the exact value this apply sent. When the config
	// left it unset the server defaulted the bag to {}, which is the value the
	// Computed attribute must resolve to.
	if plan.Settings.IsNull() || plan.Settings.IsUnknown() {
		plan.Settings = types.StringValue("{}")
	}
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
	// The read is redacted: settings comes back as key names only
	// (settings_keys). Keep the last-applied value when its key set still
	// matches; on a key-set mismatch the bag drifted outside Terraform, so
	// null it to surface an update in the next plan. After import there is no
	// prior value at all — resolve an empty server bag to "{}" and otherwise
	// leave it null (the values are unrecoverable by API contract).
	if state.Settings.IsNull() || state.Settings.IsUnknown() {
		if len(view.SettingsKeys) == 0 {
			state.Settings = types.StringValue("{}")
		}
	} else if !settingsKeysMatch(state.Settings.ValueString(), view.SettingsKeys) {
		state.Settings = types.StringNull()
	}
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
	// Same contract as Create: the response redacts settings, so the applied
	// plan value (or the server's {} default when unset) is the state value.
	if plan.Settings.IsNull() || plan.Settings.IsUnknown() {
		plan.Settings = types.StringValue("{}")
	}
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

// ImportState brings an existing hook under management by name. The write-only
// fields (on_empty, default) are not echoed by reads and stay null after import.
// settings values are likewise unrecoverable (reads redact them to key names),
// so a non-empty imported bag stays null until the first apply rewrites it.
func (r *hookResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("name"), req, resp)
}

// applyHookView folds a HookView projection back onto the model. The read shape
// carries the plugin as transport{kind:"plugin",target}, so project it back and
// leave the write-only fields (on_empty, default) untouched. settings is also
// left untouched here: reads redact it to settings_keys, and each caller
// resolves state.settings against that projection (see Create/Read/Update).
func applyHookView(m *hookModel, v *hookView) {
	m.Name = types.StringValue(v.Name)
	m.Kind = types.StringValue(v.Kind)
	if v.Transport.Kind == "plugin" {
		m.Plugin = optString(v.Transport.Target)
	} else {
		m.Plugin = types.StringNull()
	}
	m.TimeoutMS = types.Int64Value(v.TimeoutMS)
	m.OnError = types.StringValue(v.OnError)
	m.Prompt = types.StringValue(v.Prompt)
	m.User = types.StringValue(v.User)
	m.Priority = types.Int64Value(v.Priority)
	m.At = optString(v.At)
	m.Global = types.BoolValue(v.Global)
}

// settingsKeysMatch reports whether the JSON object in the state's settings
// string has exactly the key set busbar's redacted settings_keys projection
// reports. A non-object bag (busbar forwards those verbatim) has no comparable
// key projection, so it is treated as in-sync rather than perpetually drifted.
func settingsKeysMatch(settings string, keys []string) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settings), &obj); err != nil {
		return true
	}
	if len(obj) != len(keys) {
		return false
	}
	for _, k := range keys {
		if _, ok := obj[k]; !ok {
			return false
		}
	}
	return true
}
