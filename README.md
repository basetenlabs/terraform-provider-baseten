# Terraform Provider for Baseten

[![CI](https://github.com/basetenlabs/terraform-provider-baseten/actions/workflows/ci.yml/badge.svg)](https://github.com/basetenlabs/terraform-provider-baseten/actions/workflows/ci.yml)
[![License](https://img.shields.io/github/license/basetenlabs/terraform-provider-baseten)](LICENSE)

Terraform and OpenTofu provider for [Baseten](https://baseten.co). Manages the environments of models that
already exist, manages workspace secrets, and can own a model's source and deploy it.

[Provider documentation](docs/), including a [getting started guide](docs/guides/getting-started.md),
[adopted models](docs/guides/adopted-models.md), and [managed models](docs/guides/managed-models.md).

⚠️ Resources and attributes may change between releases until this provider reaches 1.0.

## Installation

Not yet published to the Terraform Registry, so build it and point Terraform at your build.

1. Build the provider:

       go build -o "$(go env GOPATH)/bin/terraform-provider-baseten" .

2. Add a `dev_overrides` block to `~/.terraformrc`, naming the directory holding that binary:

   ```hcl
   provider_installation {
     dev_overrides {
       "basetenlabs/baseten" = "/home/you/go/bin"
     }
     direct {}
   }
   ```

3. Declare the provider, without a version constraint. Terraform resolves a bare `baseten` to
   `hashicorp/baseten`, so the source address has to be spelled out for the override to match:

   ```hcl
   terraform {
     required_providers {
       baseten = {
         source = "basetenlabs/baseten"
       }
     }
   }
   ```

4. Run `terraform plan` directly. Do not run `terraform init`, which fails trying to fetch a provider the
   registry does not have yet. Every command warns that development overrides are in effect.

Once the provider is published, delete the `dev_overrides` block, add a version constraint, and `terraform init`
normally.

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
