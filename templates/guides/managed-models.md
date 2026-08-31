---
page_title: "Managed models"
subcategory: "Guides"
description: |-
  Have Terraform own a model's source and lifetime, deploying when it changes.
---

# Managed models

Managed mode adds `push`. Terraform owns the model's source and lifetime: it
creates the model if it does not exist, pushes a new deployment whenever the
source changes, and can delete the model on destroy once `deletion_protection`
is off.

To manage an existing model's environments and leave its source and lifetime
alone, see the [Adopted models](adopted-models) guide.

## Create a model

Terraform creates a model by pushing one, because Baseten has no model without a
deployment. Point `push` at the source with either `config_dir` or `config`.

### From a model directory

`config_dir` uploads the directory as it stands, so it carries anything the model
needs: `config.yaml`, model code, data files, vendored packages. A
`.truss_ignore` file decides what is left out.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
    config_dir  = "${path.module}/qwen-2.5-3b"
    environment = "production"
    wait        = true
  }
}
```

```text
qwen-2.5-3b/
  config.yaml
  model/
    model.py
```

`model_name` in `config.yaml` has to match the resource's `name`.

### From an inline configuration

`config` takes the same fields as `config.yaml` and needs no directory, for a
model that is configuration alone. It cannot carry model code, and comments in it
are not preserved.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
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

    environment = "production"
    wait        = true
  }
}
```

`wait = true` holds the apply until the deployment is active. Without it the
apply returns as soon as the deployment is created.

## What deploys

A push happens when the source changes, or when a `push.triggers` entry changes.
Everything else in `push` is an argument the next push will use, so editing
`labels` or `deployment_name` deploys nothing. The plan says which it is and
names the files that changed. See
[What causes a push](../resources/model#what-causes-a-push).

Environment settings are written before the push in the same apply, so a rollout
uses them.

Promotion stays outside Terraform. `push.environment` chooses where the next
push lands, but moving an existing deployment between environments has no
desired state to hold, so keep using the CLI or the dashboard for it.

## Tear it down

Both protection flags default to true and are read out of state, so turn off
what you mean to delete and apply before destroying.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  deletion_protection = false
  # ...
}
```

Deleting the model cascades to every deployment and environment under it.

## Take over a model that already exists

Adding `push` to a model Baseten already has, whether it was adopted by Terraform
or never managed at all, takes over its source without deploying.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
    config_dir = "${path.module}/qwen-2.5-3b"
  }
}
```

The first apply pushes nothing. It records the hash of the local source as the
baseline and leaves the running deployment alone. The next edit pushes. The plan
says so:

```text
Warning: Taking over the model's source without pushing

Terraform recorded the hash of the local source as its starting point and will
not push.
```

~> **Confirm the source matches what is deployed.** Baseten does not report what
a deployment was built from, so the provider trusts the local directory.
Adopting from a stale checkout records an in-sync state while the model serves
older code, and it corrects itself on the next edit but silently.

### Migrate from the CLI

1. Check out the commit you last pushed.
1. Write the resource with `push`, pointing `config_dir` at the directory you
   run `baseten model push` from.
1. `terraform plan`. It should report taking over, not pushing. Anything else
   means the directory does not match what is deployed.
1. `terraform apply`.
1. Move settings you manage elsewhere into `environments`, one at a time.
