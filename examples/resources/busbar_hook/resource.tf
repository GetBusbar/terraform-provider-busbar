# The module must already be installed as a signed hook plugin on the gateway.
resource "busbar_hook" "ranker" {
  name       = "quality-ranker"
  kind       = "gate"
  module     = "my-installed-ranker"
  prompt     = "ro"
  timeout_ms = 100
  priority   = 10
  on_error   = "weighted"
  settings   = jsonencode({ min_score = 0.6 })
}
