# Terraform Provider for Baseten

[![CI](https://github.com/basetenlabs/terraform-provider-baseten/actions/workflows/ci.yml/badge.svg)](https://github.com/basetenlabs/terraform-provider-baseten/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/basetenlabs/terraform-provider-baseten)](LICENSE)

Terraform and OpenTofu provider for [Baseten](https://baseten.co). Manages the environments of models that
already exist, manages workspace secrets, and can own a model's source and deploy it.

[Provider documentation](https://registry.terraform.io/providers/basetenlabs/baseten/latest/docs), including a
[getting started guide](https://registry.terraform.io/providers/basetenlabs/baseten/latest/docs/guides/getting-started),
[adopted models](https://registry.terraform.io/providers/basetenlabs/baseten/latest/docs/guides/adopted-models),
and [managed models](https://registry.terraform.io/providers/basetenlabs/baseten/latest/docs/guides/managed-models).

⚠️ Resources and attributes may change between releases until this provider reaches 1.0.

## Installation

Terraform resolves a bare `baseten` to `hashicorp/baseten`, so spell the source address out:

```hcl
terraform {
  required_providers {
    baseten = {
      source  = "basetenlabs/baseten"
      version = "~> 0.1"
    }
  }
}
```

Then `terraform init`. OpenTofu uses the same address, from
[its own registry](https://search.opentofu.org/provider/basetenlabs/baseten).

## Usage

Authenticate with `BASETEN_API_KEY` in the environment, or set `api_key` on the provider.

`baseten_model` has two modes, and the difference is whether Terraform owns the model's source and lifetime.

### Adopted models

Without `push`, the model has to already exist and Terraform manages only the environments the configuration
names. Nothing is created, nothing is deployed, and a destroy forgets the model. Settings live on the
environment rather than on any one deployment, so they survive later pushes, and settings left out keep their
current values.

```hcl
resource "baseten_model" "phi_3_mini" {
  name = "Phi 3 Mini"

  environments = {
    production = {
      autoscaling = {
        min_replica = 2
        max_replica = 20
      }
    }
  }
}
```

See the [adopted models guide](docs/guides/adopted-models.md).

### Managed models

With `push`, Terraform owns the model's source and lifetime. It creates the model on the first apply and pushes
a new deployment whenever the source changes.

```hcl
resource "baseten_model" "phi_3_mini" {
  name = "Phi 3 Mini"

  push = {
    config_dir = "${path.module}/phi-3-mini"
    wait       = true
  }
}
```

A model that is configuration alone can skip the directory and write `push.config` inline instead. See the
[managed models guide](docs/guides/managed-models.md).

### Manage secrets

```hcl
resource "baseten_secret" "hf_access_token" {
  name             = "hf_access_token"
  value_wo         = var.hf_access_token
  value_wo_version = 1
}
```

`value_wo` is write-only, so it never reaches state. Increment `value_wo_version` to push a new value.

## Building

    go build ./

See [CONTRIBUTING.md](CONTRIBUTING.md) for contribution guidelines.
