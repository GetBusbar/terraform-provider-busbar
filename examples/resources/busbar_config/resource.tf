# GitOps singleton: apply the whole running config document. Manage AT MOST ONE
# busbar_config per gateway. The document is the JSON payload busbar boots from,
# an envelope of { config = {DeployCfg}, providers = {name = ProviderDef} }.
#
# Full config applies are live-only: they revert to disk plus overlay on reload
# or restart. Persist the source document in config.yaml. Destroying this resource is a
# no-op on the gateway (there is no "unapply"); it only drops Terraform's tracking.
resource "busbar_config" "running" {
  document = jsonencode({
    config = {
      models = {
        "claude-sonnet" = { provider = "anthropic", max_concurrent = 8, max_requests = -1 }
      }
      providers = {
        anthropic = { api_key = { env = "ANTHROPIC_API_KEY" } }
      }
      auth = {
        chain = ["keys"]
        admin_auth = [{ "admin-tokens" = { token = { env = "BUSBAR_ADMIN_TOKEN" } } }]
      }
    }
    providers = {
      anthropic = { protocol = "anthropic", base_url = "https://api.anthropic.com" }
    }
  })
}

output "config_version" {
  value = busbar_config.running.config_version
}
