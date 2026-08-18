# Reads a model without managing it, so another configuration can point at one
# it does not own.
data "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"
}

# This model serves an OpenAI-compatible API, so hand this to an OpenAI client
# as its base URL. A model with its own Model class ends in /predict instead.
output "openai_base_url" {
  value = "https://model-${data.baseten_model.qwen_2_5_3b.id}.api.baseten.co/environments/production/sync/v1"
}
