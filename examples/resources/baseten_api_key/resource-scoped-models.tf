# Read-only. This model is not managed here, so Terraform resolves its name to
# an ID and never creates, changes, or destroys it.
data "baseten_model" "whisper" {
  name = "whisper-large-v3"
}

resource "baseten_model" "classifier" {
  name = "intent-classifier"

  push = {
    config_dir = "${path.module}/intent-classifier"
  }
}

# Limited to these two models, so the key cannot invoke anything else in the
# team. Referencing the managed model also orders it before the key, because its
# ID is unknown until it is created.
resource "baseten_api_key" "scoped_inference" {
  name = "scoped-inference"
  type = "WORKSPACE_INVOKE"

  model_ids = [
    data.baseten_model.whisper.id,
    baseten_model.classifier.id,
  ]
}
