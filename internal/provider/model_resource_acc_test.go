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

// TestAccModelManaged drives one model through the whole managed lifecycle.
// It is one test rather than several because a push is the expensive part: this
// pays for exactly two, one waited create and one forced redeploy, and hangs
// every other assertion off the model they produce. Environment writes are
// cheap, so the steps in between cover them generously.
//
// The implicit assertion in every step matters as much as the explicit ones:
// the harness re-plans after each apply and fails on any diff, which is the
// only thing that can prove the Optional+Computed settings model does not
// permadiff.
func TestAccModelManaged(t *testing.T) {
	testAccPreCheck(t)

	modelName := "tf-acc-" + accRandomSuffix(t)
	sourceDir := writeAccModelSource(t, modelName)

	// Registered before the first apply, so a run that dies mid-push still
	// tears down, and because the destroy this test ends on deliberately leaves
	// the model in place.
	t.Cleanup(func() { accDeleteModel(t, modelName) })

	// deployment_id is compared between steps rather than against a literal,
	// because what is under test is which changes push and which do not.
	deploymentKept := statecheck.CompareValue(compare.ValuesSame())
	deploymentReplaced := statecheck.CompareValue(compare.ValuesDiffer())

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// deletion_protection is left at its default, so the harness's destroy
		// must delete nothing. Asserting that is the point, and it is why
		// teardown goes out of band.
		CheckDestroy: func(*terraform.State) error {
			ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
			defer cancel()
			if accFindModelByName(ctx, t, modelName) == "" {
				return fmt.Errorf("model %q was deleted by destroy, want it left in place while "+
					"deletion_protection is true", modelName)
			}
			return nil
		},
		Steps: []resource.TestStep{
			// Create the model by pushing it, and read it back through the data
			// source in the same configuration, which is also how anyone
			// referencing a model they just deployed would write it.
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name = %[1]q

  push = {
    config_dir  = %[2]q
    environment = "production"
    wait        = true
  }

  environments = {
    production = {
      autoscaling = {
        min_replica = 0
        max_replica = 1
      }
    }
  }
}

data "baseten_model" "test" {
  name = baseten_model.test.name
}
`, modelName, sourceDir),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("id"), knownvalue.NotNull()),
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("deployment_id"), knownvalue.NotNull()),
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("push").AtMapKey("source_hash"), knownvalue.NotNull()),
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("environments").AtMapKey("production").
							AtMapKey("autoscaling").AtMapKey("min_replica"),
						knownvalue.Int64Exact(0)),
					// The push waited, so what Terraform deployed is what
					// production serves, and the data source has to agree.
					statecheck.CompareValuePairs(
						"baseten_model.test", tfjsonpath.New("deployment_id"),
						"data.baseten_model.test", tfjsonpath.New("production_deployment_id"),
						compare.ValuesSame()),
					deploymentKept.AddStateValue("baseten_model.test", tfjsonpath.New("deployment_id")),
				},
			},

			// Retune production and create staging. Settings must never deploy
			// anything, which the deployment_id comparison pins.
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name = %[1]q

  push = {
    config_dir  = %[2]q
    environment = "production"
    wait        = true
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 0
        max_replica      = 2
        scale_down_delay = 900
      }
    }
    staging = {
      autoscaling = {
        min_replica = 0
        max_replica = 1
      }
    }
  }
}
`, modelName, sourceDir),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("environments").AtMapKey("production").
							AtMapKey("autoscaling").AtMapKey("max_replica"),
						knownvalue.Int64Exact(2)),
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("environments").AtMapKey("staging").
							AtMapKey("autoscaling").AtMapKey("max_replica"),
						knownvalue.Int64Exact(1)),
					deploymentKept.AddStateValue("baseten_model.test", tfjsonpath.New("deployment_id")),
				},
			},

			// Dropping staging while it is protected has to fail the plan,
			// rather than silently leaving the environment unmanaged.
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name = %[1]q

  push = {
    config_dir  = %[2]q
    environment = "production"
    wait        = true
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 0
        max_replica      = 2
        scale_down_delay = 900
      }
    }
  }
}
`, modelName, sourceDir),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`Environment is protected from deletion`),
			},

			// The same removal succeeds once protection is off in the same
			// apply, which is the one-apply escape hatch the design promises.
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name                            = %[1]q
  environment_deletion_protection = false

  push = {
    config_dir  = %[2]q
    environment = "production"
    wait        = true
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 0
        max_replica      = 2
        scale_down_delay = 900
      }
    }
  }
}
`, modelName, sourceDir),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("environments"),
						knownvalue.MapSizeExact(1)),
					deploymentKept.AddStateValue("baseten_model.test", tfjsonpath.New("deployment_id")),
				},
			},

			// Two things at once, both cheap. labels are create-only at Baseten,
			// so they apply to the next push and must not cause one now, which
			// is the design's central claim about what deploys. And a second
			// resource adopts the same model without a push, managing the
			// staging the managed resource stopped mentioning: that is the mode
			// most users run, and it is only reachable here because the push
			// above created something to adopt.
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name                            = %[1]q
  environment_deletion_protection = false

  push = {
    config_dir  = %[2]q
    environment = "production"
    wait        = true

    labels = {
      owner = "terraform-acceptance"
    }
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 0
        max_replica      = 2
        scale_down_delay = 900
      }
    }
  }
}

resource "baseten_model" "adopted" {
  name = baseten_model.test.name

  # Nothing to protect: an adopted model is forgotten on destroy rather than
  # deleted, which the CheckDestroy assertion relies on.
  deletion_protection             = false
  environment_deletion_protection = false

  environments = {
    staging = {
      autoscaling = {
        min_replica = 0
        max_replica = 1
      }
    }
  }
}
`, modelName, sourceDir),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("push").AtMapKey("labels").AtMapKey("owner"),
						knownvalue.StringExact("terraform-acceptance")),
					deploymentKept.AddStateValue("baseten_model.test", tfjsonpath.New("deployment_id")),
					deploymentReplaced.AddStateValue("baseten_model.test", tfjsonpath.New("deployment_id")),

					// Adopted mode resolves the same model and deploys nothing.
					statecheck.CompareValuePairs(
						"baseten_model.test", tfjsonpath.New("id"),
						"baseten_model.adopted", tfjsonpath.New("id"),
						compare.ValuesSame()),
					statecheck.ExpectKnownValue("baseten_model.adopted",
						tfjsonpath.New("push"), knownvalue.Null()),
					statecheck.ExpectKnownValue("baseten_model.adopted",
						tfjsonpath.New("deployment_id"), knownvalue.Null()),
					// Each resource manages only what it names, so the two do
					// not fight over the model's other environments.
					statecheck.ExpectKnownValue("baseten_model.adopted",
						tfjsonpath.New("environments"), knownvalue.MapSizeExact(1)),
					statecheck.ExpectKnownValue("baseten_model.test",
						tfjsonpath.New("environments"), knownvalue.MapSizeExact(1)),
				},
			},

			// The second and last push: triggers redeploy source that has not
			// changed. wait goes back to its default of false, so this costs the
			// time to start a build rather than to finish one, and environment
			// protection defaults back on so the closing destroy is a no-op.
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name = %[1]q

  push = {
    config_dir  = %[2]q
    environment = "production"

    labels = {
      owner = "terraform-acceptance"
    }

    triggers = {
      reason = "acceptance test forced redeploy"
    }
  }

  environments = {
    production = {
      autoscaling = {
        min_replica      = 0
        max_replica      = 2
        scale_down_delay = 900
      }
    }
  }
}
`, modelName, sourceDir),
				ConfigStateChecks: []statecheck.StateCheck{
					deploymentReplaced.AddStateValue("baseten_model.test", tfjsonpath.New("deployment_id")),
				},
			},
		},
	})
}

// TestAccModelAdoptedNotFound is the cheap half of adopted mode: it never
// creates anything, so it costs one API call. Adopting a model that does exist
// is covered by the managed test's first step, which takes the same path once
// the push has created the model.
func TestAccModelAdoptedNotFound(t *testing.T) {
	testAccPreCheck(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "test" {
  name = %q
}
`, "tf-acc-absent-"+accRandomSuffix(t)),
				ExpectError: regexp.MustCompile(`Model not found`),
			},
		},
	})
}
