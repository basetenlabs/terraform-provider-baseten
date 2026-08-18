variable "hf_access_token" {
  type      = string
  sensitive = true
}

# A workspace secret, referenced by name from a model's config.yaml.
resource "baseten_secret" "hf_access_token" {
  name = "hf_access_token"

  # Write-only, so it is never stored in state or plan files.
  value_wo = var.hf_access_token

  # Increment to push the current value_wo. Editing value_wo alone shows no
  # diff, since a write-only value is never stored to compare against.
  value_wo_version = 1
}
