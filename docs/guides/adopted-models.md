---
page_title: "Adopted models"
subcategory: "Guides"
description: |-
  Manage an existing model's environments, leaving its source and lifetime
  outside Terraform.
---

# Adopted models

Adopted mode leaves `push` out. The model has to already exist, from
`baseten model push` or the dashboard, and Terraform manages only the
environments the configuration names. Nothing is created, nothing is deployed,
and a destroy forgets the model rather than deleting it.

To have Terraform own the model's source and lifetime instead, see the
[Managed models](managed-models) guide.

## Adopt a model

There is no import step. `baseten_model` resolves the model by `name` or `id`,
and `environments` upserts, so writing the resource and applying adopts whatever
is already there.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

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

Settings live on the environment rather than on any one deployment, so they
survive every later push. Keep pushing with `baseten model push`.

## What Terraform leaves alone

`environments` is exhaustive over what Terraform manages, not over what exists:

- An environment the configuration never names is left alone, so two
  configurations can manage different environments of one model.
- A setting left out keeps its current value. Baseten has no reset-to-default,
  so deleting a line stops managing that setting instead of clearing it, and the
  plan warns when it happens.
- Deployments, replica counts, and promotions are not managed here at all.

## Tear it down

Destroying an adopted model forgets it. The model and its deployments stay,
and `deletion_protection` has nothing to guard.

Environments are deleted only where `environment_deletion_protection = false`,
except production, which Baseten does not allow deleting.
