# Register a blocking "gate" hook reached over a webhook: it may inspect
# (prompt = "ro") and rerank candidates before the request is dispatched.
resource "busbar_hook" "ranker" {
  name       = "quality-ranker"
  kind       = "gate"
  webhook    = "https://ranker.internal.example/rank"
  prompt     = "ro"
  timeout_ms = 100
  priority   = 10
  on_error   = "weighted" # fall back to the weighted floor if the hook errors
  settings   = jsonencode({ min_score = 0.6 })
}

# A fire-and-forget "tap" hook over a unix socket for async usage telemetry.
resource "busbar_hook" "usage_tap" {
  name    = "usage-telemetry"
  kind    = "tap"
  socket  = "/run/busbar/usage.sock"
  at      = "completion"
  global  = true
}
