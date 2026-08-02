# Mint a governance virtual key (busbar >= 1.5.0): a signed, expiring token,
# bound to a `groups:` bucket that carries all budget/rate enforcement, scoped
# to the "smart" pool. The signed token is returned only once, at creation, and
# stored in state as a sensitive value.
resource "busbar_virtual_key" "app" {
  name          = "checkout-service"
  group         = "team-checkout" # must exist in the gateway's `groups:` block
  allowed_pools = ["smart"]
  expires_in    = "30d"
  labels        = { service = "checkout" }
}

# The signed bearer token (bbk_...) — hand this to the calling application.
output "app_key_token" {
  value     = busbar_virtual_key.app.token
  sensitive = true
}
