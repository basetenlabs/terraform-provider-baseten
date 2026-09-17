---
page_title: "baseten_api_key Resource - baseten"
subcategory: ""
description: |-
  An API key, for authenticating against the Baseten API.
  Baseten returns a key's value once, when it is created, so api_key is stored in Terraform state. Treat the state as a secret.
  There is no endpoint for changing an existing key, so every attribute replaces the key when it changes. That is what makes rotation work: change rotate_when_changed and the next apply revokes the old key and mints a new one.
  Unlike baseten_secret, this resource has no deletion_protection attribute, because a guard on deletion would also block rotation: Terraform replaces a resource by destroying it and creating it again. Use lifecycle { prevent_destroy = true } to pin one key, accepting that it blocks rotation too.
---

# baseten_api_key (Resource)

An API key, for authenticating against the Baseten API.

Baseten returns a key's value once, when it is created, so `api_key` is stored in Terraform state. Treat the state as a secret.

There is no endpoint for changing an existing key, so every attribute replaces the key when it changes. That is what makes rotation work: change `rotate_when_changed` and the next apply revokes the old key and mints a new one.

Unlike `baseten_secret`, this resource has no `deletion_protection` attribute, because a guard on deletion would also block rotation: Terraform replaces a resource by destroying it and creating it again. Use `lifecycle { prevent_destroy = true }` to pin one key, accepting that it blocks rotation too.

## Example Usage

```terraform
# A key that can invoke every model in its team, for an application or a CI job.
resource "baseten_api_key" "inference" {
  name = "inference"
  type = "WORKSPACE_INVOKE"
}

# Baseten returns the value once, when the key is created, so it is held in
# Terraform state and cannot be read back from the API afterwards.
output "inference_api_key" {
  value     = baseten_api_key.inference.api_key
  sensitive = true
}
```

## Rotating a key

Rotation happens on `terraform apply`, not on a schedule Baseten keeps. Keys have
no server-side expiration, so nothing rotates unless something keeps applying.

```terraform
# Rotation is driven by terraform apply. time_rotating, from the hashicorp/time
# provider, shows a diff once its window has elapsed; any change to
# rotate_when_changed replaces the key. Do not use timestamp() here, which
# changes on every plan.
resource "time_rotating" "inference" {
  rotation_days = 30
}

resource "baseten_api_key" "inference" {
  name = "inference"
  type = "WORKSPACE_INVOKE"

  rotate_when_changed = {
    rotated_at = time_rotating.inference.rfc3339
  }
}
```

Terraform destroys the old key and creates its replacement under the same name
within the one apply, so there are a few seconds in which no valid key exists.
That default ordering is what makes reusing the name possible.

`create_before_destroy` avoids the gap, but a key's name has to be unique among
keys that have not been revoked, and during a create-before-destroy replacement
both exist at once. The scope of that rule depends on `type`:

| `type` | `name` must be unique among |
| --- | --- |
| `WORKSPACE_INVOKE`, `WORKSPACE_EXPORT_METRICS`, `WORKSPACE_MANAGE_ALL` | every non-personal key in the same team, whatever its type |
| `WORKSPACE_MANAGE_API_KEYS` | other keys of that type in the organization |
| `PERSONAL` | your own personal keys |

The cross-type team scope catches people out on a first apply too: two keys named
`inference` in one team collide even with different types.

So to rotate without a gap, either leave `name` unset, since the rule only applies
to keys that have one, or vary it:

```terraform
resource "baseten_api_key" "inference" {
  name = "inference-${time_rotating.inference.rfc3339}"
  type = "WORKSPACE_INVOKE"

  rotate_when_changed = {
    rotated_at = time_rotating.inference.rfc3339
  }

  lifecycle {
    create_before_destroy = true
  }
}
```

## Limiting a key to specific models

```terraform
# Read-only. This model is not managed here, so Terraform resolves its name to
# an ID and never creates, changes, or destroys it.
data "baseten_model" "whisper" {
  name = "whisper-large-v3"
}

resource "baseten_model" "classifier" {
  name = "intent-classifier"

  push = {
    config_dir = "${path.module}/intent-classifier"
  }
}

# Limited to these two models, so the key cannot invoke anything else in the
# team. Referencing the managed model also orders it before the key, because its
# ID is unknown until it is created.
resource "baseten_api_key" "scoped_inference" {
  name = "scoped-inference"
  type = "WORKSPACE_INVOKE"

  model_ids = [
    data.baseten_model.whisper.id,
    baseten_model.classifier.id,
  ]
}
```

`model_ids` takes IDs, not names, because Terraform already resolves names: a
managed model exposes `.id`, and `data.baseten_model` looks one up by name and
fails loudly on an ambiguous one rather than guessing.

Baseten accepts `model_ids` only for `WORKSPACE_INVOKE` and
`WORKSPACE_EXPORT_METRICS`, and rejects it for every other `type`.

## Team-scoped keys

```terraform
# A key belongs to one team. Set team_name, or team_id if you have it, and set
# neither for the organization's default team. Names only have to be unique
# within the team, across every workspace key type.
resource "baseten_api_key" "research_inference" {
  name      = "inference"
  type      = "WORKSPACE_INVOKE"
  team_name = "research"
}
```

`PERSONAL` and `WORKSPACE_MANAGE_API_KEYS` keys cannot be team-scoped.

## Which types you can create

The provider authenticates with an API key of its own, and what that key is
decides which types it may create. A personal key acts as the user it belongs to;
a workspace key acts as a service account.

| `type` | With a personal key | With a workspace key |
| --- | --- | --- |
| `WORKSPACE_INVOKE` | yes, as a team or organization admin | yes, with permission to manage API keys or the team |
| `WORKSPACE_EXPORT_METRICS` | same | same |
| `WORKSPACE_MANAGE_ALL` | same | same |
| `WORKSPACE_MANAGE_API_KEYS` | organization admins only | no |
| `PERSONAL` | yes, owned by that user | no |

A `PERSONAL` key created through Terraform belongs to whoever ran the apply and
disappears with their account, which rarely suits infrastructure as code.

## Managing many keys

Use `for_each` over a map rather than `count` over a list. Removing one element
of a list shifts every later index, so Terraform would replace all the keys after
the one you removed, rotating credentials you did not mean to touch.

```terraform
resource "baseten_api_key" "service" {
  for_each = toset(["billing", "ingest", "search"])

  name = each.key
  type = "WORKSPACE_INVOKE"
}
```

## Importing

A key is imported by its prefix:

```shell
terraform import baseten_api_key.inference abcd1234
```

`api_key` is null afterwards, because Baseten returns a key's value only when it
is created. A configuration that feeds `api_key` into something else cannot adopt
an existing key; it has to create one.

Refreshing fills in `team_name`, so a team-scoped key should be imported against
a configuration that names its team that way. `team_id` cannot be recovered,
because Baseten reports a key's team by name only, and a configuration setting
`team_id` on an imported key replaces it.

<!-- schema generated by tfplugindocs -->
## Schema

### Required

- `type` (String) Scope of the API key. One of `WORKSPACE_INVOKE`, `WORKSPACE_EXPORT_METRICS`, `WORKSPACE_MANAGE_ALL`, `WORKSPACE_MANAGE_API_KEYS`, or `PERSONAL`. Which of them you can create depends on the API key the provider itself authenticates with. Changing this creates a new key and deletes the old one.

### Optional

- `model_ids` (Set of String) IDs of the models the key is limited to, from `baseten_model.example.id` or `data.baseten_model.example.id`. Accepted only for `WORKSPACE_INVOKE` and `WORKSPACE_EXPORT_METRICS`; Baseten rejects it for every other `type`. When unset, the key reaches every model in its scope. Changing this creates a new key and deletes the old one.
- `name` (String) Name of the API key. Must be unique among keys that have not been revoked, within a scope that depends on `type`: the team for workspace keys, whatever their type, the organization for `WORKSPACE_MANAGE_API_KEYS`, and your own keys for `PERSONAL`. Leave it unset to skip the uniqueness rule entirely. Changing this creates a new key and deletes the old one.
- `rotate_when_changed` (Map of String) Arbitrary key/value pairs that replace the API key when they change, so a key can be rotated on an external condition such as a rotating timestamp. Pair it with the `time_rotating` resource rather than `timestamp()`, which changes on every plan.
- `team_id` (String) ID of the team owning the key. Conflicts with `team_name`. When neither is set, the organization's default team is used. Not supported for `PERSONAL` or `WORKSPACE_MANAGE_API_KEYS` keys.
- `team_name` (String) Name of the team owning the key, resolved to an ID. Conflicts with `team_id`. Filled in from Baseten when left unset, including for a key that belongs to the default team. Import a team-scoped key with this rather than `team_id`, which Baseten does not report and so cannot be recovered.

### Read-Only

- `api_key` (String, Sensitive) Value of the API key. Baseten returns it once, when the key is created, so it is stored in Terraform state and cannot be read back afterwards. It is null for a key brought under management with `terraform import`.
- `prefix` (String) Prefix of the API key, which identifies it without revealing it. This is what the Baseten UI and `baseten org api-key list` display, and what identifies the key to `terraform import`.
