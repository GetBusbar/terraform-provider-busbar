# Read the target gateway's version, compiled-in plugin proof, and topology.
data "busbar_info" "current" {}

output "busbar_version" {
  value = data.busbar_info.current.version
}

output "compiled_auth_modules" {
  value = data.busbar_info.current.auth_modules
}
