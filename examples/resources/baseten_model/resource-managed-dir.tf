# Managed mode: Terraform owns the model's code, creating the model if it does
# not exist and pushing whenever the source changes.
resource "baseten_model" "phi_3_mini" {
  name = "Phi 3 Mini"

  push = {
    # A model directory holding config.yaml and model/model.py. Editing any
    # file it uploads pushes a new deployment.
    config_dir = "${path.module}/phi-3-mini"

    # Where the next push lands. Does not move the deployment already running.
    environment = "production"

    # Fail the apply if the deployment does not become active.
    wait = true
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 1
        max_replica      = 5
        scale_down_delay = 900
      }

      # Written before the push in the same apply, so the rollout uses them.
      promotion = {
        rolling_deploy = {
          enabled           = true
          max_surge_percent = 25
        }
      }
    }
  }
}
