package main

// Regenerate the API client from the vendored OpenAPI schema. The schema is a
// copy of busbar's published contract (internal/apiclient/openapi.json); refresh
// it from a new release, then `make generate`.
//
//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen -config internal/apiclient/config.yaml internal/apiclient/openapi.json
