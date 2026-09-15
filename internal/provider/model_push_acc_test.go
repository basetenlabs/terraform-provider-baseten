package provider

import (
	"context"
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// TestAccModelInlineConfig is the second and last model this suite creates. It
// exists because inline config is a wholly separate push path from config_dir:
// the provider synthesizes a directory, injects model_name from the resource,
// and hashes canonical JSON instead of walking files.
//
// The configuration is a server that needs no model code, which is what inline
// config is for, and the cheapest such thing: a stock Python image serving HTTP
// on CPU. The push is not waited, so this costs the time to accept a build
// rather than to finish one.
//
// It also omits push.environment, which is the common case: a deployment that
// belongs to no environment. Nothing here manages environments either, so this
// covers a model Terraform deploys to but does not organize.
//
// The most valuable assertion is again the implicit one. Storing a dynamic
// value verbatim is the requirement that makes inline config work at all, since
// reconstructing it through Go maps turns an HCL tuple into a list and fails the
// apply, so an empty plan afterwards is what proves the round trip.
func TestAccModelInlineConfig(t *testing.T) {
	testAccPreCheck(t)

	modelName := "tf-acc-inline-" + accRandomSuffix(t)
	t.Cleanup(func() { accDeleteModel(t, modelName) })

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		// deletion_protection is off here, so unlike the config_dir test this
		// asserts the other half of the contract: an unprotected destroy really
		// does delete the model.
		CheckDestroy: func(*terraform.State) error {
			ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
			defer cancel()
			if modelID := accFindModelByName(ctx, t, modelName); modelID != "" {
				return fmt.Errorf("model %q (%s) survived destroy, want it deleted once "+
					"deletion_protection is false", modelName, modelID)
			}
			return nil
		},
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "baseten_model" "inline" {
  name                = %[1]q
  deletion_protection = false

  push = {
    # No model_name: the provider fills it in from the resource, and one that
    # disagrees is rejected.
    config = {
      base_image = {
        image = "python:3.12-slim"
      }
      docker_server = {
        start_command      = "python -m http.server 8000"
        readiness_endpoint = "/"
        liveness_endpoint  = "/"
        predict_endpoint   = "/"
        server_port        = 8000
      }
      resources = {
        cpu     = "1"
        memory  = "512Mi"
        use_gpu = false
      }
    }
  }
}
`, modelName),
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("baseten_model.inline",
						tfjsonpath.New("deployment_id"), knownvalue.NotNull()),
					statecheck.ExpectKnownValue("baseten_model.inline",
						tfjsonpath.New("push").AtMapKey("source_hash"), knownvalue.NotNull()),
					// Held exactly as written, including the number's type,
					// which is where reconstructing it would go wrong.
					statecheck.ExpectKnownValue("baseten_model.inline",
						tfjsonpath.New("push").AtMapKey("config").
							AtMapKey("docker_server").AtMapKey("server_port"),
						knownvalue.Int64Exact(8000)),
					statecheck.ExpectKnownValue("baseten_model.inline",
						tfjsonpath.New("push").AtMapKey("config").
							AtMapKey("base_image").AtMapKey("image"),
						knownvalue.StringExact("python:3.12-slim")),
				},
			},
		},
	})
}
