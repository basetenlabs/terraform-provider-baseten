---
page_title: "Adopting existing models"
subcategory: "Guides"
description: |-
  Bring models the CLI or dashboard already created under Terraform, in either
  mode.
---

# Adopting existing models

There is no import step. `baseten_model` resolves a model by `name` or `id`
rather than creating one, and `environments` upserts, so writing the resource
and applying adopts whatever is already there.

Pick a mode by how much you want Terraform to own.

## Settings only

Leave `push` out. Terraform manages the environments you name and nothing else,
never creates or deletes the model, and forgets it on destroy.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

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

Keep pushing with the CLI. Anything the configuration does not name is left
alone: an environment built in the dashboard survives, two resources can manage
different environments of one model, and settings you omit keep their current
values instead of resetting.

## Code as well

Add `push`. Terraform starts deploying on source changes, and a destroy can
delete the model once `deletion_protection` is off.

```terraform
resource "baseten_model" "qwen_2_5_3b" {
  name = "Qwen-2.5-3B"

  push = {
    config_dir = "${path.module}/qwen-2.5-3b"
  }

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

## Migrating from the CLI

1. Check out the commit you last pushed.
2. Write the resource with `push`, pointing `config_dir` at the directory you
   push from.
3. `terraform plan`. It should report taking over, not pushing. Anything else
   means the directory does not match what is deployed.
4. `terraform apply`.
5. Move settings you manage elsewhere into `environments`, one at a time.

Promotion stays outside Terraform. `push.environment` chooses where the next
push lands, but moving an existing deployment between environments has no
desired state to hold, so keep using the CLI or the dashboard for it.
