# Adopted mode: no push block, so the model has to already exist and Terraform
# manages only its settings. Destroying this forgets the model, never deletes it.
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  # Only these are managed. An environment created in the dashboard and never
  # listed here is left alone.
  environments = {
    production = {
      autoscaling = {
        min_replica = 2
        max_replica = 20
      }
    }

    # Created on the first apply if it does not exist yet.
    staging = {
      autoscaling = {
        min_replica = 0
        max_replica = 2
      }
    }
  }
}
