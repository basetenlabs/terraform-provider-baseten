---
page_title: "Getting started"
description: |-
  Configure the Baseten provider and pick between adopted and managed models.
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

`baseten_model` has two modes, and the difference is whether Terraform owns the
model's source and lifetime.

## Adopted models

Without `push`, the model has to already exist, from `baseten model push` or the
dashboard. Terraform manages only the environments the configuration names:
nothing is created, nothing is deployed, and a destroy forgets the model.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  environments = {
    production = {
      autoscaling = {
        min_replica = 1
        max_replica = 5
      }
    }
  }
}
```

See the [Adopted models](adopted-models) guide. To have Terraform own the
model's source and lifetime instead, see the [Managed models](managed-models)
guide.

## Managed models

With `push`, Terraform owns the model's source and lifetime. It creates the model
if it does not exist, pushes a new deployment whenever the source changes, and
can delete the model on destroy once `deletion_protection` is off.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
    config_dir = "${path.module}/qwen-2.5-3b"
    wait       = true
  }
}
```

See the [Managed models](managed-models) guide. To manage settings on a model
that already exists and leave its source and lifetime alone, see the
[Adopted models](adopted-models) guide.

Both modes manage `environments` the same way, and a model can move between
modes later.

## Secrets

Secrets belong to the workspace, so they work the same in either mode.

```terraform
resource "baseten_secret" "hf_access_token" {
  name             = "hf_access_token"
  value_wo         = var.hf_access_token
  value_wo_version = 1
}
```

`value_wo` is write-only, so the value reaches neither state nor plan files.
Increment `value_wo_version` to rotate. Rotating deploys nothing.

## Next

- [`baseten_model`](../resources/model) and [`baseten_secret`](../resources/secret)
  for the full attribute reference.
