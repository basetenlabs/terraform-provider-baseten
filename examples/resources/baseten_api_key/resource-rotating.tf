# Rotation is driven by terraform apply. time_rotating, from the hashicorp/time
# provider, shows a diff once its window has elapsed; any change to
# rotate_when_changed replaces the key. Do not use timestamp() here, which
# changes on every plan.
resource "time_rotating" "inference" {
  rotation_days = 30
}

resource "baseten_api_key" "inference" {
  name = "inference"
  type = "WORKSPACE_INVOKE"

  rotate_when_changed = {
    rotated_at = time_rotating.inference.rfc3339
  }
}
