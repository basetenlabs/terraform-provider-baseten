# Contributing

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
