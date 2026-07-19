# GitOps singleton: apply the whole running config document. Manage AT MOST ONE
# busbar_config per gateway. The document is the JSON payload busbar boots from,
# an envelope of { config = {DeployCfg}, providers = {name = ProviderDef} }.
#
# Applies are live-only by default: they revert to disk truth on the next reload
# or restart unless the gateway persists an overlay. Destroying this resource is a
# no-op on the gateway (there is no "unapply"); it only drops Terraform's tracking.
resource "busbar_config" "running" {
  document = jsonencode({
    config = {
      auth = null
      models = {
        "claude-sonnet" = { provider = "anthropic", max_concurrent = 8, max_requests = -1 }
      }
      providers = {
        anthropic = { api_key_env = "ANTHROPIC_API_KEY" }
      }
      governance = {
        enabled     = true
        db_path     = "/var/lib/busbar/governance.db"
        admin_token = var.busbar_admin_token
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
