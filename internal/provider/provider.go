// Package provider implements the busbar Terraform provider (slice 1: auth +
// the busbar_info data source). It configures a typed admin-API client from the
// generated apiclient package and hands it to data sources via Configure.
package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"github.com/GetBusbar/terraform-provider-busbar/internal/apiclient"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Environment-variable fallbacks for the two required attributes.
const (
	envEndpoint = "BUSBAR_ENDPOINT"
	envToken    = "BUSBAR_ADMIN_TOKEN"
)

// adminTokenHeader is the apiKey header name from the schema's `adminToken` scheme.
const adminTokenHeader = "x-admin-token"

// Ensure the provider satisfies the framework interface.
var _ provider.Provider = (*busbarProvider)(nil)

// busbarProvider is the provider implementation.
type busbarProvider struct {
	// version is set by the build (via New) and surfaced in the user agent /
	// provider metadata.
	version string
}

// busbarProviderModel maps the provider `busbar { ... }` block to Go values.
type busbarProviderModel struct {
	Endpoint      types.String `tfsdk:"endpoint"`
	Token         types.String `tfsdk:"token"`
	ClientCertPEM types.String `tfsdk:"client_cert_pem"`
	ClientKeyPEM  types.String `tfsdk:"client_key_pem"`
	CACertPEM     types.String `tfsdk:"ca_cert_pem"`
	Insecure      types.Bool   `tfsdk:"insecure"`
}

// New returns a provider factory for the given build version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &busbarProvider{version: version}
	}
}

func (p *busbarProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "busbar"
	resp.Version = p.version
}

func (p *busbarProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Manage a busbar LLM gateway through its admin API.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true, // required, but env fallback means we validate in Configure
				Description: "Admin listener URL, e.g. https://busbar-admin.internal:8081. " +
					"May also be set with the " + envEndpoint + " environment variable.",
			},
			"token": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				Description: "Operator admin token, sent as the " + adminTokenHeader + " header. " +
					"May also be set with the " + envToken + " environment variable.",
			},
			"client_cert_pem": schema.StringAttribute{
				Optional:    true,
				Description: "PEM-encoded client certificate for mTLS against a cert-gated admin plane.",
			},
			"client_key_pem": schema.StringAttribute{
				Optional:    true,
				Sensitive:   true,
				Description: "PEM-encoded private key for client_cert_pem.",
			},
			"ca_cert_pem": schema.StringAttribute{
				Optional:    true,
				Description: "PEM-encoded CA certificate to trust the admin server's certificate (e.g. a private CA).",
			},
			"insecure": schema.BoolAttribute{
				Optional:    true,
				Description: "Skip TLS certificate verification. Development only; never use against production.",
			},
		},
	}
}

func (p *busbarProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg busbarProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Reject unknown (not-yet-computed) values with a clear message rather than
	// letting them fall through to a confusing runtime error.
	if cfg.Endpoint.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"),
			"Unknown busbar endpoint",
			"The provider cannot be configured with an unknown endpoint value.")
	}
	if cfg.Token.IsUnknown() {
		resp.Diagnostics.AddAttributeError(path.Root("token"),
			"Unknown busbar token",
			"The provider cannot be configured with an unknown token value.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// Attribute value wins; fall back to the environment.
	endpoint := firstNonEmpty(cfg.Endpoint.ValueString(), os.Getenv(envEndpoint))
	token := firstNonEmpty(cfg.Token.ValueString(), os.Getenv(envToken))

	if endpoint == "" {
		resp.Diagnostics.AddAttributeError(path.Root("endpoint"),
			"Missing busbar endpoint",
			"Set the `endpoint` attribute or the "+envEndpoint+" environment variable.")
	}
	if token == "" {
		resp.Diagnostics.AddAttributeError(path.Root("token"),
			"Missing busbar admin token",
			"Set the `token` attribute or the "+envToken+" environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	httpClient, err := buildHTTPClient(cfg)
	if err != nil {
		resp.Diagnostics.AddError("Invalid TLS configuration", err.Error())
		return
	}

	// The x-admin-token header is injected on every request via a request editor,
	// so callers never have to thread auth through each operation.
	authEditor := func(_ context.Context, r *http.Request) error {
		r.Header.Set(adminTokenHeader, token)
		return nil
	}

	client, err := apiclient.NewClientWithResponses(
		endpoint,
		apiclient.WithHTTPClient(httpClient),
		apiclient.WithRequestEditorFn(authEditor),
	)
	if err != nil {
		resp.Diagnostics.AddError("Failed to construct busbar API client", err.Error())
		return
	}

	// Hand the configured client to data sources (and, later, resources).
	resp.DataSourceData = client
	resp.ResourceData = client
}

func (p *busbarProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewInfoDataSource,
	}
}

func (p *busbarProvider) Resources(_ context.Context) []func() resource.Resource {
	// Slice 1 ships no resources — config/hooks/keys land in later slices.
	return nil
}

// buildHTTPClient assembles an *http.Client honoring mTLS, a custom CA, and the
// insecure escape hatch. When no TLS knobs are set it returns a default client.
func buildHTTPClient(cfg busbarProviderModel) (*http.Client, error) {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	tlsConfigured := false

	if cfg.Insecure.ValueBool() {
		tlsCfg.InsecureSkipVerify = true
		tlsConfigured = true
	}

	// Client certificate for mTLS: both PEMs must be present together.
	certPEM := cfg.ClientCertPEM.ValueString()
	keyPEM := cfg.ClientKeyPEM.ValueString()
	switch {
	case certPEM != "" && keyPEM != "":
		cert, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			return nil, fmt.Errorf("loading client certificate/key: %w", err)
		}
		tlsCfg.Certificates = []tls.Certificate{cert}
		tlsConfigured = true
	case certPEM != "" || keyPEM != "":
		return nil, fmt.Errorf("client_cert_pem and client_key_pem must be set together")
	}

	// Custom CA to trust a private admin server certificate.
	if caPEM := cfg.CACertPEM.ValueString(); caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, fmt.Errorf("ca_cert_pem did not contain any valid PEM certificates")
		}
		tlsCfg.RootCAs = pool
		tlsConfigured = true
	}

	if !tlsConfigured {
		return &http.Client{}, nil
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsCfg
	return &http.Client{Transport: transport}, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
