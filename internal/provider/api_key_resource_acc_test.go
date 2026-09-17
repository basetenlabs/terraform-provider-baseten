package provider

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/compare"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// accAPIKeyConfig is a personal key, deliberately. Every workspace key also
// creates a service account user that revoking the key does not remove, so a
// workspace type here would leave one behind in the shared e2e workspace on
// every run. A personal key is owned by the caller directly and leaves nothing.
// The cost is that team_id and model_ids cannot be exercised, since Baseten
// rejects both for personal keys; the unit tests cover how they are sent.
const accAPIKeyConfig = `
resource "baseten_api_key" "test" {
  name = %q
  type = "PERSONAL"

  rotate_when_changed = {
    rotated_at = %q
  }
}
`

// TestAccAPIKey exists mainly for one assertion that nothing else can make: that
// a key's value really does carry its prefix ahead of the first dot. The
// provider derives the prefix that way because creating a key returns the value
// alone, and the format is not something the API promises, so only a live key
// can confirm it. The rest of the lifecycle is cheap to assert alongside it.
func TestAccAPIKey(t *testing.T) {
	testAccPreCheck(t)

	keyName := "tf-acc-" + accRandomSuffix(t)
	t.Cleanup(func() { accDeleteAPIKeysByName(t, keyName) })

	// Rotation has to replace the key, which is the opposite of the secret
	// resource, whose ID survives a rotation of its value.
	prefixReplaced := statecheck.CompareValue(compare.ValuesDiffer())

	// Captured from the first apply, to assert the rotation revoked it.
	var rotatedPrefix string

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
			defer cancel()
			if prefixes := accAPIKeyPrefixesByName(ctx, t, keyName); len(prefixes) > 0 {
				return fmt.Errorf("API key %q survived destroy as %v, want it revoked", keyName, prefixes)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(accAPIKeyConfig, keyName, "first"),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_api_key.test",
						tfjsonpath.New("api_key"), knownvalue.NotNull()),
					statecheck.ExpectKnownValue("baseten_api_key.test",
						tfjsonpath.New("prefix"), knownvalue.NotNull()),
					// Unset means the organization's default team.
					statecheck.ExpectKnownValue("baseten_api_key.test",
						tfjsonpath.New("team_id"), knownvalue.Null()),
					prefixReplaced.AddStateValue("baseten_api_key.test", tfjsonpath.New("prefix")),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					accCheckAPIKeyValueCarriesPrefix("baseten_api_key.test"),
					func(state *terraform.State) error {
						rotatedPrefix = accAPIKeyStateAttr(state, "baseten_api_key.test", "prefix")
						return nil
					},
				),
			},

			// Rotation: the trigger is the only thing that changed, and it has to
			// have produced a different key, with the first one revoked.
			{
				Config: fmt.Sprintf(accAPIKeyConfig, keyName, "second"),
				ConfigStateChecks: []statecheck.StateCheck{
					prefixReplaced.AddStateValue("baseten_api_key.test", tfjsonpath.New("prefix")),
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					accCheckAPIKeyValueCarriesPrefix("baseten_api_key.test"),
					func(state *terraform.State) error {
						ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
						defer cancel()
						if accAPIKeyExists(ctx, t, rotatedPrefix) {
							return fmt.Errorf("API key %q is still live after rotation, want it revoked",
								rotatedPrefix)
						}
						return nil
					},
				),
			},

			// Import addresses a key by prefix, and cannot recover the value,
			// because Baseten returns it only when the key is created.
			{
				ResourceName: "baseten_api_key.test",
				ImportState:  true,
				ImportStateIdFunc: func(state *terraform.State) (string, error) {
					return accAPIKeyStateAttr(state, "baseten_api_key.test", "prefix"), nil
				},
				ImportStateVerify:                    true,
				ImportStateVerifyIdentifierAttribute: "prefix",
				// Neither is readable back: the value is returned once, and the
				// rotation trigger only ever exists in the configuration.
				ImportStateVerifyIgnore: []string{"api_key", "rotate_when_changed"},
			},
		},
	})
}

// accAPIKeyPrefixesByName reports the prefixes of every key with this name that
// has not been revoked. Keys are addressed by prefix, which is not known until
// one is created, so teardown and assertions find them by name instead.
func accAPIKeyPrefixesByName(ctx context.Context, t *testing.T, name string) []string {
	t.Helper()

	keys, err := accClient(t).API().GetApiKeys(ctx)
	if err != nil {
		t.Fatalf("listing API keys: %v", err)
	}
	var prefixes []string
	for _, key := range keys.Keys {
		if key.Name != nil && *key.Name == name {
			prefixes = append(prefixes, key.Prefix)
		}
	}
	return prefixes
}

// accAPIKeyExists reports whether the key is still live. The listing holds only
// keys that have not been revoked, so a revoked key reads as absent.
func accAPIKeyExists(ctx context.Context, t *testing.T, prefix string) bool {
	t.Helper()

	keys, err := accClient(t).API().GetApiKeys(ctx)
	if err != nil {
		t.Fatalf("listing API keys: %v", err)
	}
	for _, key := range keys.Keys {
		if key.Prefix == prefix {
			return true
		}
	}
	return false
}

// accDeleteAPIKeysByName is the net for a run that failed before its own destroy
// could revoke the key. Rotation replaces a key by destroying it before creating
// its successor, so only one should ever hold the name, but every match is
// revoked in case a run died between the two.
func accDeleteAPIKeysByName(t *testing.T, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
	defer cancel()

	for _, prefix := range accAPIKeyPrefixesByName(ctx, t, name) {
		t.Logf("deleting API key %s (%s)", name, prefix)
		if _, err := accClient(t).API().DeleteApiKeys(ctx, prefix); err != nil {
			t.Errorf("deleting API key %s: %v", prefix, err)
		}
	}
}

// accCheckAPIKeyValueCarriesPrefix is the assertion this suite is for. The
// failure it guards is silent: a value the prefix cannot be cut out of would
// leave Terraform unable to read or revoke the key it just created.
func accCheckAPIKeyValueCarriesPrefix(resourceName string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		prefix := accAPIKeyStateAttr(state, resourceName, "prefix")
		if prefix == "" {
			return fmt.Errorf("%s has no prefix in state", resourceName)
		}
		// The value itself stays out of the failure message.
		if !strings.HasPrefix(accAPIKeyStateAttr(state, resourceName, "api_key"), prefix+".") {
			return fmt.Errorf("%s has an api_key that does not begin with %q, so Baseten's key format "+
				"has changed and the provider can no longer derive a prefix from it", resourceName, prefix+".")
		}
		return nil
	}
}

func accAPIKeyStateAttr(state *terraform.State, resourceName, attribute string) string {
	resourceState, ok := state.RootModule().Resources[resourceName]
	if !ok {
		return ""
	}
	return resourceState.Primary.Attributes[attribute]
}
