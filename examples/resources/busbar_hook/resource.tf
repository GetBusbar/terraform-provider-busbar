# Register a blocking "gate" hook backed by a signed `kind: hook` plugin from
# the gateway's plugin catalog: it may inspect (prompt = "ro") and rerank
# candidates before the request is dispatched.
resource "busbar_hook" "ranker" {
  name       = "quality-ranker"
  kind       = "gate"
  plugin     = "ranking" # a compiled-in or signed hook plugin's name
  prompt     = "ro"
  timeout_ms = 100
  priority   = 10
  on_error   = "weighted" # fall back to the weighted floor if the hook errors
  settings   = jsonencode({ min_score = 0.6 })
}

# A fire-and-forget "tap" hook for async usage telemetry.
resource "busbar_hook" "usage_tap" {
  name   = "usage-telemetry"
  kind   = "tap"
  plugin = "usage-telemetry"
  at     = "completion"
  global = true
}
