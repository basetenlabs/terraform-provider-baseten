---
page_title: "Getting started"
subcategory: "Guides"
description: |-
  Deploy a model to Baseten with Terraform and tune the environment serving it.
---

# Getting started

## Configure the provider

```terraform
terraform {
  required_providers {
    baseten = {
      source  = "basetenlabs/baseten"
      version = "~> 0.1"
    }
  }
}

provider "baseten" {}
```

Export a [Baseten API key](https://docs.baseten.co/organization/api-keys):

```sh
export BASETEN_API_KEY="..."
```

## Deploy a model

Most models are configuration alone. This one has vLLM serve pinned Qwen
weights, so `push.config` carries everything and there is nothing to upload.

On the first apply, Terraform creates the model and pushes it, because Baseten
has no model without a deployment.

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

A model that is more than a `config.yaml`, whether that is `model/model.py`,
data files, or vendored packages, takes `config_dir` instead and points at the
directory. Everything below applies either way.

## Deploy a change

Bump the vLLM image and plan. The provider hashes the configuration, so the plan
reports a push:

```text
Warning: A new deployment will be pushed

Applying this pushes a new deployment.
```

Editing `labels` or `deployment_name` instead reports no push, because those
apply to the next one. See
[What causes a push](../resources/model#what-causes-a-push).

## Tune the environment

`environments` manages settings on the environment rather than on any one
deployment, so they survive every later push.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
    # ...
    environment = "production"
    wait        = true
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 1
        max_replica      = 5
        scale_down_delay = 900
      }
    }

    # Created by this apply.
    staging = {
      autoscaling = {
        min_replica = 0
        max_replica = 2
      }
    }
  }
}
```

Settings are written before the push in the same apply, so a rollout uses them.

## Add a secret

Gated weights need a Hugging Face token. Terraform cannot see the reference,
which lives in the model's configuration, so declare the ordering yourself.

```terraform
resource "baseten_secret" "hf_access_token" {
  name             = "hf_access_token"
  value_wo         = var.hf_access_token
  value_wo_version = 1
}

resource "baseten_model" "llama_3_2_1b" {
  name = "Llama 3.2 1B"

  push = {
    config = {
      # ...
      weights = [{
        source         = "hf://meta-llama/Llama-3.2-1B-Instruct@main"
        mount_location = "/models/llama"
        auth = {
          auth_method      = "CUSTOM_SECRET"
          auth_secret_name = "hf_access_token"
        }
      }]
    }
  }

  depends_on = [baseten_secret.hf_access_token]
}
```

Adding the `auth` block edits the configuration, so that apply pushes a new
deployment. Rotating the secret's value later does not, since nothing about the
model changed. Add a `push.triggers` entry to redeploy on a rotation.

## Tear it down

Both protection flags default to true and are read out of state, so turn off
what you mean to delete and apply before destroying.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  deletion_protection = false
  # ...
}
```

## Next

- [Adopting existing models](adopting-existing-models), for models the CLI or
  dashboard already created.
- [`baseten_model`](../resources/model) for the full attribute reference.
