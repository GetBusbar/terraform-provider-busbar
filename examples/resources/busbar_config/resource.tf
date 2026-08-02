# GitOps singleton: apply the whole running config document. Manage AT MOST ONE
# busbar_config per gateway. The document is the JSON form of busbar's 1.5.0
# config syntax, an envelope of { config = {config.yaml deploy block},
# providers = {providers.yaml document} }.
#
# Applies are live-only by default: they revert to disk truth on the next reload
# or restart unless the gateway persists an overlay. Destroying this resource is a
# no-op on the gateway (there is no "unapply"); it only drops Terraform's tracking.
resource "busbar_config" "running" {
  document = jsonencode({
    config = {
      auth = {
        chain = ["keys"]
        admin_auth = [
          { admin-tokens = { token = { env = "BUSBAR_ADMIN_TOKEN" } } }
        ]
      }
      providers = {
        anthropic = { api_key = { env = "ANTHROPIC_API_KEY" } }
      }
      models = {
        "claude-sonnet" = { provider = "anthropic" }
      }
      groups = {
        team-checkout = {}
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
