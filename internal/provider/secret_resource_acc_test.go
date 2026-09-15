package provider

import (
	"context"
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// TestAccSecret costs nothing upstream, so it carries the assertions the model
// tests cannot afford: rotation through the write-only value, and both halves of
// the deletion-protection contract. A protected secret refuses to be destroyed,
// which is stricter than the model's silent no-op, so the flip-then-destroy
// sequence the error message prescribes is exercised here rather than described.
func TestAccSecret(t *testing.T) {
	testAccPreCheck(t)

	secretName := "tf-acc-" + accRandomSuffix(t)
	t.Cleanup(func() { accDeleteSecret(t, secretName) })

	// The value is write-only, so drift is undetectable by design and the ID is
	// stable across rotation. Comparing the ID is all that can be asserted, and
	// it is worth asserting: a rotation that replaced the secret would be wrong.
	idKept := statecheck.CompareValue(compare.ValuesSame())

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
			defer cancel()
			if accSecretExists(ctx, t, secretName) {
				return fmt.Errorf("secret %q survived destroy, want it deleted once "+
					"deletion_protection is false", secretName)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "baseten_secret" "test" {
  name             = %q
  value_wo         = "acceptance-value-1"
  value_wo_version = 1
}
`, secretName),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_secret.test",
						tfjsonpath.New("id"), knownvalue.NotNull()),
					// Defaulted on, and never written to state as a value.
					statecheck.ExpectKnownValue("baseten_secret.test",
						tfjsonpath.New("deletion_protection"), knownvalue.Bool(true)),
					statecheck.ExpectKnownValue("baseten_secret.test",
						tfjsonpath.New("value_wo"), knownvalue.Null()),
					idKept.AddStateValue("baseten_secret.test", tfjsonpath.New("id")),
				},
			},

			// Rotation: a new value only takes effect because the counter moved.
			{
				Config: fmt.Sprintf(`
resource "baseten_secret" "test" {
  name             = %q
  value_wo         = "acceptance-value-2"
  value_wo_version = 2
}
`, secretName),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_secret.test",
						tfjsonpath.New("value_wo_version"), knownvalue.Int64Exact(2)),
					idKept.AddStateValue("baseten_secret.test", tfjsonpath.New("id")),
				},
			},

			// Destroying while protected fails, and says what to do about it.
			// The config is repeated because a destroy step destroys a config.
			{
				Config: fmt.Sprintf(`
resource "baseten_secret" "test" {
  name             = %q
  value_wo         = "acceptance-value-2"
  value_wo_version = 2
}
`, secretName),
				Destroy:     true,
				ExpectError: regexp.MustCompile(`Secret is protected from deletion`),
			},

			// Doing what it said, in its own apply, as the message requires.
			{
				Config: fmt.Sprintf(`
resource "baseten_secret" "test" {
  name                = %q
  value_wo            = "acceptance-value-2"
  value_wo_version    = 2
  deletion_protection = false
}
`, secretName),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_secret.test",
						tfjsonpath.New("deletion_protection"), knownvalue.Bool(false)),
					// Flipping the flag must not have rewritten the credential,
					// which is why Update returns early on an unchanged version.
					idKept.AddStateValue("baseten_secret.test", tfjsonpath.New("id")),
				},
			},
		},
	})
}
