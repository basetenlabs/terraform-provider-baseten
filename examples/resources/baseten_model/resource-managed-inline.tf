# Managed mode with no directory. vLLM serves pinned weights, so the whole
# artifact is its configuration and there is no model code to upload.
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
    # Everything config.yaml accepts. model_name comes from name above, and
    # editing any of this pushes a new deployment.
    config = {
      model_metadata = {
        tags = ["openai-compatible"]
      }
      base_image = {
        image = "vllm/vllm-openai:v0.27.1"
      }
      docker_server = {
        start_command      = "vllm serve /models/qwen --served-model-name Qwen/Qwen2.5-3B-Instruct --host 0.0.0.0 --port 8000"
        readiness_endpoint = "/health"
        liveness_endpoint  = "/health"
        predict_endpoint   = "/v1/chat/completions"
        server_port        = 8000
      }
      weights = [{
        source         = "hf://Qwen/Qwen2.5-3B-Instruct@aa8e72537993ba99e69dfaafa59ed015b17504d1"
        mount_location = "/models/qwen"
      }]
      resources = {
        instance_type = "L4:4x16"
      }
      runtime = {
        predict_concurrency = 256
      }
    }
  }
}
