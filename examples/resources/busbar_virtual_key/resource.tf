# Mint a signed key scoped to the smart pool. Budget and rate limits live on
# gateway groups in busbar 1.5 and later, never on individual keys.
resource "busbar_virtual_key" "app" {
  name          = "checkout-service"
  allowed_pools = ["smart"]
}

output "app_key_secret" {
  value     = busbar_virtual_key.app.secret
  sensitive = true
}
