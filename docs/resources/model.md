---
page_title: "baseten_model Resource - baseten"
subcategory: ""
description: |-
  A Baseten model, in one of two modes.
  Adopted, without push: the model has to already exist, and Terraform manages only its environment settings. Nothing is ever created or deleted, and destroying the resource forgets the model rather than removing it.
  Managed, with push: Terraform owns the model's code. It creates the model if it does not exist, pushes a new deployment whenever the source changes, and can delete the model on destroy once deletion_protection is off.
  Adding push to a model Terraform already adopted moves it between these modes, which changes what an apply and a destroy can do. The plan warns when that happens.
  environments is exhaustive over what Terraform manages, not over what exists. An environment the configuration never mentions is left alone, so a model can be partly managed here and partly elsewhere.
---

# baseten_model (Resource)

A Baseten model, in one of two modes.

**Adopted**, without `push`: the model has to already exist, and Terraform manages only its environment settings. Nothing is ever created or deleted, and destroying the resource forgets the model rather than removing it.

**Managed**, with `push`: Terraform owns the model's code. It creates the model if it does not exist, pushes a new deployment whenever the source changes, and can delete the model on destroy once `deletion_protection` is off.

Adding `push` to a model Terraform already adopted moves it between these modes, which changes what an apply and a destroy can do. The plan warns when that happens.

`environments` is exhaustive over what Terraform manages, not over what exists. An environment the configuration never mentions is left alone, so a model can be partly managed here and partly elsewhere.

## Example Usage

### Managed, from a model directory

```terraform
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
```

`phi-3-mini/` is an ordinary model directory:

```text
phi-3-mini/
  config.yaml
  model/
    model.py
```

```yaml
# config.yaml. model_name has to match the resource's name.
model_name: Phi 3 Mini
python_version: py311
requirements:
  - transformers==4.41.2
  - torch==2.3.0
  - accelerate==0.30.1
resources:
  instance_type: T4x4x16
```

### Managed, from an inline configuration

Models built from configuration alone need no directory. Inline `config` cannot
carry model code, and comments in it are not preserved.

```terraform
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
```

### Adopted, managing environments only

```terraform
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
```

## What causes a push

Managed mode only. An adopted model has no `push` block, so Terraform never
deploys to it and nothing in this section applies.

A push happens when the source changes, or when a `triggers` entry changes.
Nothing else in `push` deploys anything.

```terraform
push = {
  # Pushes. source_hash is computed from the files this uploads, so editing
  # model/model.py deploys on the next apply.
  config_dir = "${path.module}/phi-3-mini"

  # Pushes. Redeploys identical code, for a base image that moved or a secret
  # that rotated. Keys are yours to choose, so record why.
  triggers = {
    hf_token_rotated = "2026-08-01"
  }

  # None of these push. They apply to the next push, the way a flag applies to
  # the push you are running rather than to the deployment already serving.
  environment            = "staging"
  deployment_name        = "release-2026-08-01"
  labels                 = { team = "search" }
  wait                   = true
  deploy_timeout_minutes = 45
}
```

The plan says which of the two happened and names the files that changed. It
also warns when one of the deferred attributes changed with no push to carry it.

Hashing reads every uploaded file on every plan. On a directory big enough for
that to hurt, set `source_hash` yourself and the directory is not read:

```terraform
push = {
  config_dir  = "${path.module}/phi-3-mini"
  source_hash = var.git_commit_sha
}
```

Any value that changes when the source does will do. This replaces how the
source is identified rather than forcing a push, so removing it later resumes
hashing and pushes once. Use `triggers` to force a push.

## Environments

`environments` is exhaustive over what Terraform manages, not over what exists.

```terraform
environments = {
  # Created if it does not exist. production is the exception: Baseten creates
  # it with the model, and it cannot be created or deleted.
  staging = {
    autoscaling = { min_replica = 0, max_replica = 2 }
  }
}
```

- An environment the configuration never names is left alone, so one created in
  the dashboard survives. Dropping `production` therefore stops managing it
  rather than deleting it, which Baseten does not allow.
- Settings left out of an entry keep their current value. Baseten has no way to
  reset a setting to its default, so deleting a line does nothing and the plan
  warns when a value in state stops being configured.
- Settings are written before the push in the same apply, so a rollout uses the
  settings that apply asked for. The apply that creates the model is inverted,
  since the model has to exist first, so a new model's first deployment always
  rolls out on Baseten's defaults.

## Waiting and timeouts

```terraform
resource "baseten_model" "phi_3_mini" {
  push = {
    config_dir = "${path.module}/phi-3-mini"

    # Block until the deployment is active, failing the apply if it is not.
    wait = true

    # How long Baseten allows the deploy itself to take.
    deploy_timeout_minutes = 45
  }

  # How long Terraform waits. Defaults to 30 minutes.
  timeouts {
    create = "45m"
    update = "45m"
  }
}
```

Without `wait`, an apply returns as soon as the deployment is created, so a
build that fails later surfaces as drift on a later plan instead of as an apply
error. A `timeouts` expiry stops the wait, not the deploy. Baseten keeps going
and the next plan reads the result.

## Destroying

Two flags guard this, both defaulting to true.

```terraform
resource "baseten_model" "phi_3_mini" {
  # Lets a destroy delete the model, and every deployment and environment under
  # it. Does nothing in adopted mode, where a destroy only forgets the model.
  deletion_protection = false

  # Lets an entry be removed from environments, and lets a destroy delete
  # managed environments when the model itself is not being deleted.
  environment_deletion_protection = false
}
```

Removing an environment entry works in the same apply that sets the flag.
Destroying does not, because `terraform destroy` reads the flag out of state, so
apply it first. To stop managing something without deleting it, use
`terraform state rm`.

<!-- schema generated by tfplugindocs -->
## Schema

### Optional

- `deletion_protection` (Boolean) Whether Terraform is prevented from deleting the model. Defaults to true, because deleting a model also deletes every deployment and environment under it and cannot be undone. Only meaningful alongside `push`: an adopted model is never deleted. Setting this to false also allows the destroy to delete environments, regardless of `environment_deletion_protection`, since they go with the model.
- `environment_deletion_protection` (Boolean) Whether Terraform is prevented from deleting environments. Defaults to true, because deleting an environment is traffic-affecting: Baseten scales the deployment serving it down to zero replicas. While true, removing an entry from `environments` fails the plan, and destroying this resource leaves every environment in place. Set it to false in the same apply that removes an entry to allow the deletion.
- `environments` (Attributes Map) Environments to manage, keyed by environment name. An entry that does not exist yet is created, except `production`, which Baseten creates with the model. Settings left out of an entry keep their current values. (see [below for nested schema](#nestedatt--environments))
- `id` (String) Unique identifier of the model. Set this or `name`.
- `name` (String) Name of the model. Set this or `id`. Changing it targets a different model, which replaces this resource, because Baseten cannot rename a model.
- `push` (Attributes) Code to deploy, which puts this resource in managed mode. Leave it out to adopt an existing model without deploying anything. Adding it lets Terraform create the model, push a deployment whenever the source changes, and delete the model on destroy. (see [below for nested schema](#nestedatt--push))
- `team_id` (String) ID of the team owning the model. Conflicts with `team_name`. Set either one to resolve `name` within a single team, since model names are unique only within a team.
- `team_name` (String) Name of the team owning the model, resolved to an ID. Conflicts with `team_id`.
- `timeouts` (Attributes) (see [below for nested schema](#nestedatt--timeouts))

### Read-Only

- `created_at` (String) Time the model was created, in ISO 8601 format.
- `deployment_id` (String) Unique identifier of the deployment Terraform last pushed. Null in adopted mode, and never reflects a deployment created outside Terraform.

<a id="nestedatt--environments"></a>
### Nested Schema for `environments`

Optional:

- `autoscaling` (Attributes) Autoscaling settings for the environment. Attributes left out keep their current values rather than resetting to Baseten's defaults. (see [below for nested schema](#nestedatt--environments--autoscaling))
- `promotion` (Attributes) Promotion settings for the environment. Attributes left out keep their current values rather than resetting to Baseten's defaults. (see [below for nested schema](#nestedatt--environments--promotion))

Read-Only:

- `created_at` (String) Time the environment was created, in ISO 8601 format.
- `instance_type_name` (String) Display name of the instance type serving the environment. Read-only, because Baseten copies the instance type onto a new deployment rather than changing it in place.

<a id="nestedatt--environments--autoscaling"></a>
### Nested Schema for `environments.autoscaling`

Optional:

- `autoscaling_window` (Number) Timeframe of traffic considered for autoscaling decisions, in seconds.
- `concurrency_target` (Number) Number of requests per replica before scaling up.
- `max_replica` (Number) Maximum number of replicas.
- `max_scale_down_rate` (Number) Maximum percentage of replicas that can be removed per autoscaling window, from 1 to 50. For example, 20 means at most 20% of replicas are removed per window. Baseten has no way to clear this once set.
- `min_replica` (Number) Minimum number of replicas.
- `scale_down_delay` (Number) Waiting period before scaling down any active replica, in seconds.
- `target_in_flight_tokens` (Number) Target number of in-flight tokens for autoscaling decisions. Early access only. Baseten has no way to clear this once set.
- `target_utilization_percentage` (Number) Target utilization percentage for scaling up and down.


<a id="nestedatt--environments--promotion"></a>
### Nested Schema for `environments.promotion`

Optional:

- `promotion_cleanup_strategy` (String) The cleanup strategy to use after a promotion completes. One of `KEEP`, `SCALE_TO_ZERO`, or `DEACTIVATE`.
- `ramp_up_duration_seconds` (Number) Duration of the ramp up, in seconds.
- `ramp_up_while_promoting` (Boolean) Whether to ramp up traffic while promoting.
- `redeploy_on_promotion` (Boolean) Whether to deploy on all promotions. Enabling this flag allows model code to safely handle environment-specific logic. When a deployment is promoted, a new deployment is created with a copy of the image.
- `rolling_deploy` (Attributes) Rolling deploy orchestration for promotions to this environment. Setting any of the tuning attributes without `enabled = true` stores the values without turning rolling deploy on. (see [below for nested schema](#nestedatt--environments--promotion--rolling_deploy))

<a id="nestedatt--environments--promotion--rolling_deploy"></a>
### Nested Schema for `environments.promotion.rolling_deploy`

Optional:

- `enabled` (Boolean) Whether the environment should rely on rolling deploy orchestration. Defaults to false at Baseten, so rolling deploy stays off until this is set.
- `max_surge_percent` (Number) The maximum surge percentage for rolling deploys.
- `max_unavailable_percent` (Number) The maximum unavailable percentage for rolling deploys.
- `replica_overhead_percent` (Number) The replica overhead percentage for rolling deploys.
- `stabilization_time_seconds` (Number) The stabilization time in seconds for rolling deploys.
- `strategy` (String) The rolling deploy strategy to use for promotions. Currently `REPLICA`.




<a id="nestedatt--push"></a>
### Nested Schema for `push`

Optional:

- `config` (Dynamic) Model configuration written inline, accepting any field `config.yaml` accepts. Set this or `config_dir`. **Editing it pushes a new deployment**, unless `source_hash` is set. Shorthand for a directory holding only this configuration, so it suits models built from configuration alone and cannot carry model code. `model_name` is filled in from the resource, and comments are not preserved. See the [configuration reference](https://docs.baseten.co/reference/truss-configuration).
- `config_dir` (String) Path to the model directory to push, containing at least a `config.yaml`. Set this or `config`. **Editing any file it uploads pushes a new deployment**, unless `source_hash` is set. A `.truss_ignore` file in the directory decides what is uploaded.
- `deploy_timeout_minutes` (Number) How long Baseten allows the deployment a push creates to take, from 10 to 1440 minutes. Leave unset to use Baseten's own default. This bounds the deploy itself, not how long Terraform waits, which is the resource-level `timeouts` block.

Changing this does not push. It applies to the next push.
- `deployment_name` (String) Name for the deployment a push creates. Names are unique per model and a push never removes the deployment it replaces, so a literal name can be pushed only once. Leave unset to let Baseten assign `deployment-N`.

Changing this does not push, and does not rename the deployment already running. It applies to the next push.
- `environment` (String) Environment the next push deploys into, created if it does not exist.

Changing this does not push, and does not move the deployment already running. It applies to the next push, the same way `--environment` applies to `baseten model push`. To move what is deployed now, promote it outside Terraform.
- `labels` (Map of String) Labels for the deployment a push creates.

Changing this does not push, and does not relabel the deployment already running. It applies to the next push.
- `preserve_env_instance_type` (Boolean) Whether a push into `environment` keeps that environment's current instance type instead of the one in the configuration. Defaults to true at Baseten.

Changing this does not push. It applies to the next push.
- `source_hash` (String) How the provider recognizes that the source changed. Left unset it is computed: from the files `config_dir` uploads, or from the canonical form of an inline `config`. Hashing a directory reads all of it on every plan, so set this to skip that on a large one, supplying any value that changes when the source does, such as a commit SHA.

~> This overrides *how the source is identified*; it is not a way to force a push. Taking it over means the source is no longer read, and removing it later resumes hashing, which pushes once. Use `triggers` to force a push.
- `triggers` (Map of String) Arbitrary values that push a new deployment when any of them changes, even though the source did not. Use it to redeploy identical code, for instance because a base image moved or a secret rotated. Keys are yours to choose and record why: `triggers = { base_image = "v2" }`.

Unlike taking over `source_hash`, this composes with automatic hashing, so bumping a trigger forces one push and leaves the source still tracked.
- `wait` (Boolean) Whether to wait for a pushed deployment to finish deploying, failing the apply if it does not become active. Defaults to false, which returns as soon as the deployment is created, so a later build failure surfaces as drift rather than an apply error. The resource-level `timeouts` block bounds the wait.

Changing this does not push. It applies to the next push.


<a id="nestedatt--timeouts"></a>
### Nested Schema for `timeouts`

Optional:

- `create` (String) A string that can be [parsed as a duration](https://pkg.go.dev/time#ParseDuration) consisting of numbers and unit suffixes, such as "30s" or "2h45m". Valid time units are "s" (seconds), "m" (minutes), "h" (hours).
- `update` (String) A string that can be [parsed as a duration](https://pkg.go.dev/time#ParseDuration) consisting of numbers and unit suffixes, such as "30s" or "2h45m". Valid time units are "s" (seconds), "m" (minutes), "h" (hours).
