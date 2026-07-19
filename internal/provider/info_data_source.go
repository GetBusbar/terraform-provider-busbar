package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/GetBusbar/terraform-provider-busbar/internal/apiclient"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Ensure the data source satisfies the framework interfaces.
var (
	_ datasource.DataSource              = (*infoDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*infoDataSource)(nil)
)

// infoDataSource reads GET /api/v1/admin/info: version, the compiled-in plugin
// proof, uptime, and topology counts.
type infoDataSource struct {
	client *apiclient.ClientWithResponses
}

// NewInfoDataSource is the data-source factory registered on the provider.
func NewInfoDataSource() datasource.DataSource {
	return &infoDataSource{}
}

// infoModel maps the /info response into Terraform state. The `/info` operation
// carries no response schema in the OpenAPI contract, so the shape is modeled
// here against busbar's InfoView (docs/admin-api.md, crate admin::v1::contract).
type infoModel struct {
	Version           types.String `tfsdk:"version"`
	UptimeSeconds     types.Int64  `tfsdk:"uptime_seconds"`
	StartedAt         types.Int64  `tfsdk:"started_at"`
	ConfigPersistence types.Bool   `tfsdk:"config_persistence"`
	ConfigVersion     types.Int64  `tfsdk:"config_version"`
	AuthModules       types.List   `tfsdk:"auth_modules"`
	HookPlugins       types.List   `tfsdk:"hook_plugins"`
	WeightedFloor     types.Bool   `tfsdk:"weighted_floor"`
	Pools             types.Int64  `tfsdk:"pools"`
	Models            types.Int64  `tfsdk:"models"`
	Providers         types.Int64  `tfsdk:"providers"`
}

// infoResponse is the wire shape decoded from /info (busbar InfoView).
type infoResponse struct {
	Version           string `json:"version"`
	UptimeSeconds     *int64 `json:"uptime_seconds"`
	StartedAt         *int64 `json:"started_at"`
	ConfigPersistence bool   `json:"config_persistence"`
	ConfigVersion     int64  `json:"config_version"`
	Build             struct {
		AuthModules   []string `json:"auth_modules"`
		HookPlugins   []string `json:"hook_plugins"`
		WeightedFloor bool     `json:"weighted_floor"`
	} `json:"build"`
	Topology struct {
		Pools     int64 `json:"pools"`
		Models    int64 `json:"models"`
		Providers int64 `json:"providers"`
	} `json:"topology"`
}

func (d *infoDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_info"
}

func (d *infoDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Read-only view of the target busbar gateway: version, the compiled-in " +
			"plugin proof, uptime, and topology counts (GET /api/v1/admin/info).",
		Attributes: map[string]schema.Attribute{
			"version": schema.StringAttribute{
				Computed:    true,
				Description: "busbar semantic version reported by the gateway.",
			},
			"uptime_seconds": schema.Int64Attribute{
				Computed:    true,
				Description: "Seconds since the gateway process started; null if never stamped.",
			},
			"started_at": schema.Int64Attribute{
				Computed: true,
				Description: "Epoch seconds of process start — the boot-epoch marker. A changed " +
					"value means a restart (config_version resets), never a config revert.",
			},
			"config_persistence": schema.BoolAttribute{
				Computed: true,
				Description: "Whether API-applied config changes are durable across restarts " +
					"(BUSBAR_CONFIG_OVERLAY set) versus live-only.",
			},
			"config_version": schema.Int64Attribute{
				Computed: true,
				Description: "Monotonic config version — 0 at boot, +1 per API config apply. " +
					"Process-local; resets on restart.",
			},
			"auth_modules": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Auth modules compiled into this binary (the compliance-by-compilation proof).",
			},
			"hook_plugins": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				Description: "Hook plugins compiled into this binary.",
			},
			"weighted_floor": schema.BoolAttribute{
				Computed:    true,
				Description: "The inline SWRR floor — always true (compiled in unconditionally).",
			},
			"pools": schema.Int64Attribute{
				Computed:    true,
				Description: "Number of configured pools.",
			},
			"models": schema.Int64Attribute{
				Computed:    true,
				Description: "Number of configured model lanes.",
			},
			"providers": schema.Int64Attribute{
				Computed:    true,
				Description: "Number of configured providers.",
			},
		},
	}
}

func (d *infoDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return // provider not yet configured; framework calls again later
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected data source configure type",
			fmt.Sprintf("Expected *providerData, got %T. This is a provider bug.", req.ProviderData),
		)
		return
	}
	d.client = data.Generated
}

func (d *infoDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	httpResp, err := d.client.GetApiV1AdminInfo(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read busbar info", err.Error())
		return
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		resp.Diagnostics.AddError("Failed to read busbar info response body", err.Error())
		return
	}

	if httpResp.StatusCode != http.StatusOK {
		resp.Diagnostics.AddError(
			"busbar admin API returned an error",
			fmt.Sprintf("GET /api/v1/admin/info returned %s: %s", httpResp.Status, string(body)),
		)
		return
	}

	var info infoResponse
	if err := json.Unmarshal(body, &info); err != nil {
		resp.Diagnostics.AddError("Failed to decode busbar info response", err.Error())
		return
	}

	authModules, diags := types.ListValueFrom(ctx, types.StringType, info.Build.AuthModules)
	resp.Diagnostics.Append(diags...)
	hookPlugins, diags := types.ListValueFrom(ctx, types.StringType, info.Build.HookPlugins)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	state := infoModel{
		Version:           types.StringValue(info.Version),
		UptimeSeconds:     optInt64(info.UptimeSeconds),
		StartedAt:         optInt64(info.StartedAt),
		ConfigPersistence: types.BoolValue(info.ConfigPersistence),
		ConfigVersion:     types.Int64Value(info.ConfigVersion),
		AuthModules:       authModules,
		HookPlugins:       hookPlugins,
		WeightedFloor:     types.BoolValue(info.Build.WeightedFloor),
		Pools:             types.Int64Value(info.Topology.Pools),
		Models:            types.Int64Value(info.Topology.Models),
		Providers:         types.Int64Value(info.Topology.Providers),
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// optInt64 maps a nullable wire integer to a framework value (null when absent).
func optInt64(v *int64) types.Int64 {
	if v == nil {
		return types.Int64Null()
	}
	return types.Int64Value(*v)
}
