# A key that can invoke every model in its team, for an application or a CI job.
resource "baseten_api_key" "inference" {
  name = "inference"
  type = "WORKSPACE_INVOKE"
}

# Baseten returns the value once, when the key is created, so it is held in
# Terraform state and cannot be read back from the API afterwards.
output "inference_api_key" {
  value     = baseten_api_key.inference.api_key
  sensitive = true
}
