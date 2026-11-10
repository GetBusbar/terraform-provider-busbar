package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestAdminRedirectDoesNotDiscloseToken(t *testing.T) {
	var received atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { received.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(adminTokenHeader) != "credential" {
			t.Error("admin token missing from initial request")
		}
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	c, err := buildHTTPClient(busbarProviderModel{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Timeout <= 0 {
		t.Fatal("admin requests need a finite timeout")
	}
	resp, err := newAdminClient(source.URL, "credential", c).do(context.Background(), http.MethodGet, "/info", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTemporaryRedirect || received.Load() {
		t.Fatal("admin request followed redirect")
	}
}

type repeatedByteReader struct{}

func (repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}
func TestAdminResponseBound(t *testing.T) {
	if _, err := readAdminBody(io.LimitReader(repeatedByteReader{}, maxAdminBody)); err != nil {
		t.Fatal(err)
	}
	if _, err := readAdminBody(repeatedByteReader{}); err == nil {
		t.Fatal("unbounded response accepted")
	}
}
func TestAdminErrorDoesNotEchoSecrets(t *testing.T) {
	for _, body := range []string{`secret`, `{"error":{"message":"secret","code":"invalid_request"}}`, `{"error":{"message":"secret","code":"secret"}}`} {
		if strings.Contains((&adminResponse{Body: []byte(body)}).errorMessage(), "secret") {
			t.Fatal("secret echoed")
		}
	}
}
func TestKeyPoolsAndSignedTokenContract(t *testing.T) {
	empty := []string{}
	for _, tc := range []struct {
		pools *[]string
		want  string
	}{{nil, `{"name":"key"}`}, {&empty, `{"name":"key","allowed_pools":[]}`}} {
		b, err := json.Marshal(createKeyReq{Name: "key", AllowedPools: tc.pools})
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != tc.want {
			t.Fatalf("got %s want %s", b, tc.want)
		}
	}
	var view keyView
	if err := json.Unmarshal([]byte(`{"token":"signed-token","allowed_pools":[]}`), &view); err != nil {
		t.Fatal(err)
	}
	if view.Secret == nil || *view.Secret != "signed-token" {
		t.Fatal("minted token lost")
	}
	var m virtualKeyModel
	applyKeyView(context.Background(), &m, &view, func(a, b string) { t.Fatal(a, b) })
	if m.AllowedPools.IsNull() || len(m.AllowedPools.Elements()) != 0 {
		t.Fatal("empty allowlist changed to unrestricted")
	}
	view.AllowedPools = nil
	applyKeyView(context.Background(), &m, &view, func(a, b string) { t.Fatal(a, b) })
	if !m.AllowedPools.IsNull() {
		t.Fatal("null allowlist changed")
	}
}
func TestHookRedactedSettingsPreserved(t *testing.T) {
	m := hookModel{Settings: types.StringValue(`{ "token": "sensitive" }`)}
	applyHookView(&m, &hookView{})
	if m.Settings.ValueString() != `{ "token": "sensitive" }` {
		t.Fatal("redacted read destroyed configured settings")
	}
}
func TestConfigDocumentSensitive(t *testing.T) {
	var response resource.SchemaResponse
	(&configResource{}).Schema(context.Background(), resource.SchemaRequest{}, &response)
	if !response.Schema.Attributes["document"].IsSensitive() {
		t.Fatal("config credentials exposed in plans")
	}
}

func TestUnknownTLSRefused(t *testing.T) {
	for _, cfg := range []busbarProviderModel{{Insecure: types.BoolUnknown()}, {CACertPEM: types.StringUnknown()}, {ClientCertPEM: types.StringUnknown()}, {ClientKeyPEM: types.StringUnknown()}} {
		if _, err := buildHTTPClient(cfg); err == nil {
			t.Fatal("unknown TLS configuration accepted")
		}
	}
}
func TestHookModuleAndSettingsContract(t *testing.T) {
	r := hookResource{}
	m := hookModel{Kind: types.StringValue("gate"), Module: types.StringValue("installed-hook"), Settings: types.StringValue(`{"credential":"value"}`)}
	cfg, err := r.buildCfg(&m)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"module":"installed-hook"`) || strings.Contains(string(raw), "socket") || strings.Contains(string(raw), "webhook") {
		t.Fatalf("incorrect hook payload: %s", raw)
	}
	for _, settings := range []string{`[]`, `null`, `"scalar"`} {
		m.Settings = types.StringValue(settings)
		if _, err := r.buildCfg(&m); err == nil {
			t.Fatal("non-object settings accepted")
		}
	}
	m.Settings = types.StringNull()
	m.Webhook = types.StringValue("https://old.example")
	if _, err := r.buildCfg(&m); err == nil {
		t.Fatal("retired webhook accepted")
	}
}
func TestLegacyKeyCapsRefused(t *testing.T) {
	for _, m := range []virtualKeyModel{{MaxBudgetCents: types.Int64Value(1)}, {RPMLimit: types.Int64Value(1)}, {TPMLimit: types.Int64Value(1)}, {BudgetPeriod: types.StringValue("total")}} {
		called := false
		if validateKeyLimits(&m, func(string, string) { called = true }) || !called {
			t.Fatal("removed limit accepted")
		}
	}
}

func TestHookReadEscapesIdentity(t *testing.T) {
	var got atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.URL.EscapedPath())
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	r := hookResource{client: newAdminClient(server.URL, "token", server.Client())}
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	state := tfsdk.State{Schema: schemaResp.Schema}
	m := hookModel{Name: types.StringValue("name?query#fragment"), Phase: types.ListNull(types.StringType)}
	if diags := state.Set(ctx, &m); diags.HasError() {
		t.Fatal(diags)
	}
	response := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if got.Load() != "/api/v1/admin/hooks/name%3Fquery%23fragment" {
		t.Fatalf("wrong resource path %v", got.Load())
	}
}

func TestConfigApplyRequiresAcknowledgement(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"applied":false,"config_version":7}`))
	}))
	defer server.Close()
	r := configResource{client: newAdminClient(server.URL, "token", server.Client())}
	var diagnostics diag.Diagnostics
	if _, ok := r.apply(context.Background(), `{"config":{}}`, &diagnostics); ok || !diagnostics.HasError() {
		t.Fatal("unacknowledged apply succeeded")
	}
}
func TestConfigImportRejectsOtherIdentity(t *testing.T) {
	var response resource.ImportStateResponse
	(&configResource{}).ImportState(context.Background(), resource.ImportStateRequest{ID: "wrong"}, &response)
	if !response.Diagnostics.HasError() {
		t.Fatal("invalid singleton identity accepted")
	}
}

func TestHookSettingsSensitive(t *testing.T) {
	var response resource.SchemaResponse
	(&hookResource{}).Schema(context.Background(), resource.SchemaRequest{}, &response)
	if !response.Schema.Attributes["settings"].IsSensitive() {
		t.Fatal("hook credentials exposed in plans")
	}
}

func TestMintedCredentialsMustBePresent(t *testing.T) {
	token := "bearer"
	id := "access"
	secret := "private"
	for _, v := range []keyView{{}, {Secret: new(string)}} {
		if validateMintedCredentials(&v, false) == nil {
			t.Fatal("missing token accepted")
		}
	}
	v := keyView{Secret: &token}
	if validateMintedCredentials(&v, false) != nil {
		t.Fatal("ordinary bearer rejected")
	}
	if validateMintedCredentials(&v, true) == nil {
		t.Fatal("missing AWS credentials accepted")
	}
	v.AWSAccessKeyID = &id
	v.AWSSecretAccessKey = &secret
	if err := validateMintedCredentials(&v, true); err != nil {
		t.Fatal(err)
	}
}

func TestIncompleteMintReportsErrorAndRetainsCleanupID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"vk_created","name":"key","allowed_pools":null,"created_at":1,"enabled":true}`))
	}))
	defer server.Close()
	r := virtualKeyResource{client: newAdminClient(server.URL, "token", server.Client())}
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	m := virtualKeyModel{Name: types.StringValue("key"), AllowedPools: types.ListNull(types.StringType), BudgetPeriod: types.StringUnknown(), ID: types.StringUnknown(), Secret: types.StringUnknown(), CreatedAt: types.Int64Unknown(), Enabled: types.BoolUnknown(), AWSAccessKeyID: types.StringUnknown(), AWSSecretAccessKey: types.StringUnknown()}
	if diagnostics := plan.Set(ctx, &m); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	response := resource.CreateResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: plan}, &response)
	if !response.Diagnostics.HasError() {
		t.Fatal("missing token reported as successful create")
	}
	var state virtualKeyModel
	if diagnostics := response.State.Get(ctx, &state); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	if state.ID.ValueString() != "vk_created" {
		t.Fatal("minted key identity lost, preventing cleanup")
	}
}

func TestDecodeErrorDoesNotEchoResponseValues(t *testing.T) {
	const secretNumber = "999999999999999999999999999999999999999"
	response := adminResponse{StatusCode: 200, Body: []byte(`{"created_at":` + secretNumber + `}`)}
	var v keyView
	err := response.decode(&v)
	if err == nil || strings.Contains(err.Error(), secretNumber) {
		t.Fatalf("unsafe decode error: %v", err)
	}
}

type failingAdminReader struct{}

func (failingAdminReader) Read([]byte) (int, error) { return 0, errors.New("private-response-value") }
func TestTransportErrorsDoNotEchoPeerText(t *testing.T) {
	if strings.Contains(adminRequestError(errors.New("private-response-value")).Error(), "private-response-value") {
		t.Fatal("peer text leaked")
	}
	if _, err := readAdminBody(failingAdminReader{}); err == nil || strings.Contains(err.Error(), "private-response-value") {
		t.Fatal("body error leaked peer text")
	}
}

func TestMalformedHTTPStatusDoesNotLeakPeerText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		_, _ = io.WriteString(connection, "private-response-value\r\n\r\n")
	}))
	defer server.Close()
	_, err := newAdminClient(server.URL, "credential", server.Client()).do(context.Background(), http.MethodGet, "/info", nil, nil)
	if err == nil || strings.Contains(err.Error(), "private-response-value") {
		t.Fatalf("unsafe request error: %v", err)
	}
}

func TestConfigUpdateSendsPlannedDocument(t *testing.T) {
	const document = `{"config":{"models":{"model":{"max_concurrent":2}}}}`
	var received atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		received.Store(string(body))
		_, _ = w.Write([]byte(`{"applied":true,"config_version":42}`))
	}))
	defer server.Close()
	r := configResource{client: newAdminClient(server.URL, "token", server.Client())}
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	plan := tfsdk.Plan{Schema: schemaResp.Schema}
	m := configModel{ID: types.StringValue("config"), Document: types.StringValue(document), ConfigVersion: types.Int64Unknown()}
	if diagnostics := plan.Set(ctx, &m); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	response := resource.UpdateResponse{State: tfsdk.State{Schema: schemaResp.Schema}}
	r.Update(ctx, resource.UpdateRequest{Plan: plan}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if received.Load() != document {
		t.Fatal("updated document did not reach gateway")
	}
	var state configModel
	if diagnostics := response.State.Get(ctx, &state); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	if state.ConfigVersion.ValueInt64() != 42 {
		t.Fatal("new config version was not recorded")
	}
}

func TestHookDeleteIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		wantError  bool
		wantEscape bool
	}{
		{"success", http.StatusNoContent, false, true},
		{"already_gone", http.StatusNotFound, false, true},
		{"server_error", http.StatusInternalServerError, true, true},
		{"forbidden", http.StatusForbidden, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("method = %q, want DELETE", r.Method)
				}
				gotPath.Store(r.URL.EscapedPath())
				if tc.status == http.StatusInternalServerError || tc.status == http.StatusForbidden {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":{"code":"internal"}}`))
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			r := hookResource{client: newAdminClient(server.URL, "token", server.Client())}
			ctx := context.Background()
			var schemaResp resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
			state := tfsdk.State{Schema: schemaResp.Schema}
			m := hookModel{Name: types.StringValue("hook/special"), Phase: types.ListNull(types.StringType)}
			if diagnostics := state.Set(ctx, &m); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			response := resource.DeleteResponse{State: state}
			r.Delete(ctx, resource.DeleteRequest{State: state}, &response)
			if response.Diagnostics.HasError() != tc.wantError {
				t.Fatalf("diagnostics error = %v, want %v: %v", response.Diagnostics.HasError(), tc.wantError, response.Diagnostics)
			}
			if tc.wantEscape {
				if gotPath.Load() != "/api/v1/admin/hooks/hook%2Fspecial" {
					t.Fatalf("wrong delete path %v", gotPath.Load())
				}
			}
		})
	}
}

func TestVirtualKeyDeleteIdempotent(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantError bool
	}{
		{"revoked", http.StatusNoContent, false},
		{"already_gone", http.StatusNotFound, false},
		{"server_error", http.StatusInternalServerError, true},
		{"forbidden", http.StatusForbidden, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotMethod, gotPath atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod.Store(r.Method)
				gotPath.Store(r.URL.EscapedPath())
				if tc.status >= 400 {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(`{"error":{"code":"internal"}}`))
					return
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()
			r := virtualKeyResource{client: newAdminClient(server.URL, "token", server.Client())}
			ctx := context.Background()
			var schemaResp resource.SchemaResponse
			r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
			state := tfsdk.State{Schema: schemaResp.Schema}
			m := virtualKeyModel{ID: types.StringValue("vk_abc123"), Name: types.StringValue("key"), AllowedPools: types.ListNull(types.StringType)}
			if diagnostics := state.Set(ctx, &m); diagnostics.HasError() {
				t.Fatal(diagnostics)
			}
			response := resource.DeleteResponse{State: state}
			r.Delete(ctx, resource.DeleteRequest{State: state}, &response)
			if response.Diagnostics.HasError() != tc.wantError {
				t.Fatalf("diagnostics error = %v, want %v: %v", response.Diagnostics.HasError(), tc.wantError, response.Diagnostics)
			}
			if gotMethod.Load() != http.MethodDelete {
				t.Fatalf("method = %v, want DELETE", gotMethod.Load())
			}
			if gotPath.Load() != "/api/v1/admin/keys/vk_abc123" {
				t.Fatalf("wrong delete path %v", gotPath.Load())
			}
		})
	}
}

func TestConfigReadRefreshesVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != adminPrefix+"/config" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"version":42}`))
	}))
	defer server.Close()
	r := configResource{client: newAdminClient(server.URL, "token", server.Client())}
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	state := tfsdk.State{Schema: schemaResp.Schema}
	m := configModel{
		ID:            types.StringValue("config"),
		Document:      types.StringValue(`{"config":{}}`),
		ConfigVersion: types.Int64Value(1),
	}
	if diagnostics := state.Set(ctx, &m); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	response := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	var result configModel
	if diagnostics := response.State.Get(ctx, &result); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	if result.ConfigVersion.ValueInt64() != 42 {
		t.Fatalf("config version = %d, want 42", result.ConfigVersion.ValueInt64())
	}
	if result.Document.ValueString() != `{"config":{}}` {
		t.Fatal("document was modified during read")
	}
	if result.ID.ValueString() != "config" {
		t.Fatal("id was modified during read")
	}
}

func TestKeyReadRemovesTombstone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"vk_deleted","name":"key","state":"tombstoned","enabled":false}`))
	}))
	defer server.Close()
	r := virtualKeyResource{client: newAdminClient(server.URL, "token", server.Client())}
	ctx := context.Background()
	var schemaResp resource.SchemaResponse
	r.Schema(ctx, resource.SchemaRequest{}, &schemaResp)
	state := tfsdk.State{Schema: schemaResp.Schema}
	model := virtualKeyModel{ID: types.StringValue("vk_deleted"), Name: types.StringValue("key"), AllowedPools: types.ListNull(types.StringType)}
	if diagnostics := state.Set(ctx, &model); diagnostics.HasError() {
		t.Fatal(diagnostics)
	}
	response := resource.ReadResponse{State: state}
	r.Read(ctx, resource.ReadRequest{State: state}, &response)
	if response.Diagnostics.HasError() {
		t.Fatal(response.Diagnostics)
	}
	if !response.State.Raw.IsNull() {
		t.Fatal("deleted key remained managed after tombstone read")
	}
}
