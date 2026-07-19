# Mint a governance virtual key with a daily budget and a request-rate cap,
# scoped to the "smart" pool. The plaintext secret is returned only once, at
# creation, and stored in state as a sensitive value.
resource "busbar_virtual_key" "app" {
  name             = "checkout-service"
  budget_period    = "daily"
  max_budget_cents = 5000 # $50/day
  rpm_limit        = 60
  tpm_limit        = 200000
  allowed_pools    = ["smart"]
}

# The bearer secret (sk-bb-...) — hand this to the calling application.
output "app_key_secret" {
  value     = busbar_virtual_key.app.secret
  sensitive = true
}
