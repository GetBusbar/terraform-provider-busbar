# terraform-provider-busbar

Terraform provider for the [busbar](https://github.com/GetBusbar) LLM gateway, driving
its admin API.

## Slice 1 (this cut)

The first reviewable slice deliberately keeps scope small — it proves the plumbing,
not the feature set:

- A generated Go API client (`internal/apiclient`, from busbar's OpenAPI 3.1 contract).
- Provider authentication: the operator token as the `x-admin-token` header, plus
  optional mTLS / custom CA / insecure-skip-verify for the admin plane.
- One read-only data source, `busbar_info` → `GET /api/v1/admin/info`.

No `config`/`hooks`/`keys` resources yet — those land in later slices.

## Provider configuration

```hcl
provider "busbar" {
  endpoint = "https://busbar-admin.internal:8081" # required; admin listener URL
  token    = var.busbar_admin_token               # required; sent as x-admin-token

  # mTLS, when the admin plane requires client certs:
  # client_cert_pem = file("client.crt")
  # client_key_pem  = file("client.key")
  # ca_cert_pem     = file("admin-ca.crt") # trust a private admin server cert
  # insecure        = false                # skip TLS verify (dev only)
}
```

`endpoint` and `token` fall back to the `BUSBAR_ENDPOINT` and `BUSBAR_ADMIN_TOKEN`
environment variables, so `provider "busbar" {}` with those set is valid.

## The `busbar_info` data source

```hcl
data "busbar_info" "this" {}

output "gateway_version" {
  value = data.busbar_info.this.version
}
```

Surfaced attributes (all computed): `version`, `uptime_seconds`, `started_at`,
`config_persistence`, `config_version`, `auth_modules`, `hook_plugins`,
`weighted_floor`, `pools`, `models`, `providers`.

## Building

```sh
make generate   # regenerate the client from internal/apiclient/openapi.json
make build      # -> ./terraform-provider-busbar
make fmt vet test
```

The API client is generated with [oapi-codegen](https://github.com/oapi-codegen/oapi-codegen)
(pinned in `tools.go`); `internal/apiclient/client.gen.go` is generated — do not hand-edit it.

## Local development (dev overrides)

Point Terraform at the locally built binary instead of the registry:

```sh
make build
cat > dev.tfrc <<EOF
provider_installation {
  dev_overrides {
    "GetBusbar/busbar" = "$(pwd)"
  }
  direct {}
}
EOF
export TF_CLI_CONFIG_FILE="$(pwd)/dev.tfrc"
```

Then, in a directory with a `.tf` using the provider:

```sh
terraform providers schema -json   # inspect the loaded schema (no init needed)
terraform plan                     # reads busbar_info against the live gateway
```

With dev overrides, skip `terraform init` — Terraform prints a warning that the
provider is dev-overridden, which is expected.

## Acceptance tests

Gated on `TF_ACC`; require a reachable gateway:

```sh
BUSBAR_ENDPOINT=http://localhost:8081 \
BUSBAR_ADMIN_TOKEN=… \
make testacc
```
