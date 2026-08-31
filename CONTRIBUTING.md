# Contributing

## Running a local build

```bash
go build -o "$(go env GOPATH)/bin/terraform-provider-baseten" .
```

Point Terraform at it with a `dev_overrides` block in `~/.terraformrc`:

```hcl
provider_installation {
  dev_overrides {
    "basetenlabs/baseten" = "/home/you/go/bin"
  }
  direct {}
}
```

Then run `terraform plan` directly, without `terraform init`, and drop the `version` constraint from
`required_providers`. Every command warns that overrides are in effect.

## Acceptance tests

`TestAcc*` tests in `*_acc_test.go` apply and destroy against a live workspace. They skip unless `TF_ACC` and both
credentials are set, so `go test ./...` runs them as no-ops.

```bash
TF_ACC=1 \
BASETEN_E2E_TEST_API_KEY=... \
BASETEN_E2E_TEST_REMOTE_URL=... \
    go test ./internal/provider/ -run TestAcc -v -timeout 60m
```

Needs `terraform` on `PATH`, or `TF_ACC_TERRAFORM_PATH`. Both variables are required; without a remote URL the
provider defaults to production.

- Two models, total. Add assertions to existing ones; wait on at most one build.
- Assume nothing exists upstream. Adopted mode adopts what the test pushed.
- Unique names (`"tf-acc-" + accRandomSuffix(t)`); runs overlap.
- Register teardown before the first apply, via the API: a protected destroy keeps the model, and runs die mid-push.
- Leave `deletion_protection` at its default somewhere. It defaults on.
- `BASETEN_E2E_KEEP_MODEL=1` keeps models this teardown owns, for inspecting a deployment afterwards. Models with
  protection off are deleted by Terraform's own destroy regardless.

## Releasing

Push a `v*` tag on `main`. `.github/workflows/release.yml` builds every platform, signs the checksums, and
creates the GitHub release; the Terraform Registry ingests it through a webhook.

```bash
git tag v0.1.0
git push origin v0.1.0
```

Nothing else is version-stamped, so the tag is the only input. There must be no branch sharing its name.

### One-time setup

- `TERRAFORM_PRIVATE_KEY` and `TERRAFORM_PASSPHRASE` secrets, holding an ASCII-armored GPG private key and its
  passphrase. The registry rejects ECC, so the key has to be RSA or DSA.
- The key's ASCII-armored public half, added to the Terraform Registry under the `basetenlabs` namespace, by an
  admin of the GitHub organization. Signatures fail to verify without it.
- The provider published once in the registry, which installs the webhook that picks up later releases.
- An issue filed at [opentofu/registry](https://github.com/opentofu/registry) to list the provider there.
  OpenTofu tracks GitHub releases afterwards, but does not mirror the Terraform Registry.

### Rotating the signing key

Add the new public key in the registry before releasing with it, and leave the old one in place: it is what
verifies the releases already published.
