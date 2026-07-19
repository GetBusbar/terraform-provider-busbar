terraform {
  required_providers {
    busbar = {
      source = "GetBusbar/busbar"
    }
  }
}

# Point at the gateway's admin listener. The token is the operator admin token,
# sent as the x-admin-token header. Both may also come from the BUSBAR_ENDPOINT
# and BUSBAR_ADMIN_TOKEN environment variables.
provider "busbar" {
  endpoint = "https://busbar-admin.internal:8081"
  token    = var.busbar_admin_token
}

variable "busbar_admin_token" {
  type      = string
  sensitive = true
}
