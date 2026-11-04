package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/GetBusbar/terraform-provider-busbar/internal/apiclient"
)

// adminPrefix is the frozen v1 admin resource prefix every operation hangs off.
const adminPrefix = "/api/v1/admin"

// providerData is the shared handle the provider hands to every data source and
// resource via Configure. Slice 1 exposed the generated *ClientWithResponses
// directly; slices 2–4 need to POST/PUT/PATCH request bodies that the committed
// OpenAPI contract does not model (the admin handlers read raw bytes), so the
// mutating resources speak a thin raw-HTTP seam (adminClient) that reuses the
// exact same endpoint, auth, and TLS the generated client was built with. The
// generated client is still carried for the read-only data sources.
type providerData struct {
	// Generated typed client — used by the read-only data sources (busbar_info).
	Generated *apiclient.ClientWithResponses
	// Raw-HTTP admin seam — used by the CRUD resources.
	Admin *adminClient
}

// adminClient is a minimal admin-API HTTP client: it prefixes the admin path,
// injects the x-admin-token header on every request, and returns decoded JSON.
// It intentionally mirrors slice 1's manual decode style (the /info data source
// decodes by hand because that operation carries no response schema either).
type adminClient struct {
	httpClient *http.Client
	baseURL    string // endpoint with any trailing slash trimmed
	token      string
}

func newAdminClient(endpoint, token string, httpClient *http.Client) *adminClient {
	return &adminClient{
		httpClient: httpClient,
		baseURL:    strings.TrimRight(endpoint, "/"),
		token:      token,
	}
}

// adminResponse is the decoded result of an admin call: the raw status, the
// response body, and the parsed ETag (the config-plane version token, present on
// hook/config mutations and reads). Callers branch on StatusCode.
type adminResponse struct {
	StatusCode int
	Body       []byte
	ETag       string // unquoted ETag value, e.g. "42" -> "42"; empty when absent
}

// adminError is the frozen v1 admin error envelope {"error":{"code,message}}.
type adminError struct {
	Error struct {
		Code string `json:"code"`
	} `json:"error"`
}

// do issues an admin request. path is the admin-relative path (e.g. "/keys"),
// body is marshaled to JSON when non-nil, and header is any extra header (used
// for If-Match); pass nil for none.
func (c *adminClient) do(ctx context.Context, method, path string, body any, header map[string]string) (*adminResponse, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+adminPrefix+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set(adminTokenHeader, c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, adminRequestError(err)
	}
	defer resp.Body.Close()

	raw, err := readAdminBody(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	return &adminResponse{
		StatusCode: resp.StatusCode,
		Body:       raw,
		ETag:       strings.Trim(resp.Header.Get("ETag"), `"`),
	}, nil
}

// decode unmarshals the response body into v.
func (r *adminResponse) decode(v any) error {
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decoding response (%d): invalid JSON response", r.StatusCode)
	}
	return nil
}

// errorMessage exposes only recognized error codes, never untrusted response text.
func (r *adminResponse) errorMessage() string {
	// Error bodies can echo config documents and credentials. Only expose stable codes.
	var e adminError
	if json.Unmarshal(r.Body, &e) == nil {
		switch e.Error.Code {
		case "not_found", "unauthorized", "forbidden", "invalid_request", "version_conflict", "conflict", "rate_limited", "internal", "unavailable", "method_not_allowed":
			return e.Error.Code
		}
	}
	return "admin request failed (response body omitted)"
}

const maxAdminBody = 16 << 20

func readAdminBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxAdminBody+1))
	if err != nil {
		return nil, fmt.Errorf("failed to read admin response body")
	}
	if len(raw) > maxAdminBody {
		return nil, fmt.Errorf("admin response exceeds 16 MiB")
	}
	return raw, nil
}

// Transport errors can contain a peer-controlled malformed status line or trailer.
func adminRequestError(err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("admin request canceled")
	}
	var networkError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
		return fmt.Errorf("admin request timed out")
	}
	return fmt.Errorf("admin request failed; check the endpoint, TLS configuration, and network connectivity")
}
