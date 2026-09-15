package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
)

// accCleanupTimeout bounds the out-of-band teardown, which runs after the test
// body and therefore cannot use the test's own context.
const accCleanupTimeout = 60 * time.Second

// accKeepModelEnvVar leaves a model behind for inspection, for questions the
// test cannot answer, such as how long a deployment actually took to provision.
// It only reaches models this teardown is responsible for: one whose destroy is
// unprotected is deleted by Terraform itself, which no flag here can stop.
const accKeepModelEnvVar = "BASETEN_E2E_KEEP_MODEL"

// accModelConfigYAML is the model this suite pushes. It is deliberately the
// cheapest thing Baseten will build: no GPU, minimal CPU and memory, so a real
// push costs a few minutes rather than a GPU allocation. model_name matches the
// resource's name because the provider rejects a disagreement between them.
const accModelConfigYAML = `model_name: %s
python_version: py313
resources:
  cpu: 50m
  memory: 50Mi
  use_gpu: false
`

const accModelPy = `class Model:
    def load(self):
        pass

    def predict(self, request):
        return {"got request": request}
`

// writeAccModelSource materializes the baked-in model source and returns its
// directory, which the configuration passes as push.config_dir. Baking the
// source into the test binary rather than committing a fixture directory keeps
// the pushed artifact and the test that asserts on it in one place.
func writeAccModelSource(t *testing.T, modelName string) string {
	t.Helper()

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "model"), 0o755); err != nil {
		t.Fatalf("creating model dir: %v", err)
	}
	config := fmt.Appendf(nil, accModelConfigYAML, modelName)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), config, 0o644); err != nil {
		t.Fatalf("writing config.yaml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "model", "model.py"), []byte(accModelPy), 0o644); err != nil {
		t.Fatalf("writing model.py: %v", err)
	}
	return dir
}

// accRandomSuffix keeps names from clashing between concurrent or repeated
// runs, since model and secret names are unique per workspace.
func accRandomSuffix(t *testing.T) string {
	t.Helper()

	var bytes [8]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		t.Fatalf("reading random bytes: %v", err)
	}
	return hex.EncodeToString(bytes[:])
}

// accClient builds a management client from the e2e credentials, for the
// assertions and teardown that have to bypass Terraform. Reading the provider's
// own variables is deliberate: testAccPreCheck has already mapped the e2e ones
// onto them, so the client and the provider under test address one workspace.
func accClient(t *testing.T) *client.ManagementClient {
	t.Helper()

	managementClient, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:  os.Getenv(apiKeyEnvVar),
		BaseURL: os.Getenv(remoteURLEnvVar),
	})
	if err != nil {
		t.Fatalf("creating management client: %v", err)
	}
	return managementClient
}

// accFindModelByName reports the model's ID, or "" when no model has that name.
// Used both to assert on what a destroy did and to find what to clean up.
func accFindModelByName(ctx context.Context, t *testing.T, name string) string {
	t.Helper()

	models, err := accClient(t).API().GetModels(ctx, managementapi.GetV1ModelsParams{Name: &name})
	if err != nil {
		t.Fatalf("listing models: %v", err)
	}
	for _, model := range models.Models {
		if model.Name == name {
			return model.Id
		}
	}
	return ""
}

// accDeleteModel removes the model out of band. Teardown has to reach past
// Terraform because the destroy under test leaves a protected model in place on
// purpose, and because a run that dies mid-apply never reaches a destroy at all.
func accDeleteModel(t *testing.T, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
	defer cancel()

	modelID := accFindModelByName(ctx, t, name)
	if modelID == "" {
		return
	}
	if os.Getenv(accKeepModelEnvVar) != "" {
		t.Logf("%s is set; leaving model %s (%s) in place", accKeepModelEnvVar, name, modelID)
		return
	}
	t.Logf("deleting model %s (%s)", name, modelID)
	if _, err := accClient(t).API().DeleteModels(ctx, modelID); err != nil {
		t.Errorf("deleting model %s: %v", modelID, err)
	}
}

// accSecretExists reports whether the named secret is present, since there is
// no get-by-name endpoint.
func accSecretExists(ctx context.Context, t *testing.T, name string) bool {
	t.Helper()

	secrets, err := accClient(t).API().GetSecrets(ctx)
	if err != nil {
		t.Fatalf("listing secrets: %v", err)
	}
	for _, secret := range secrets.Secrets {
		if secret.Name == name {
			return true
		}
	}
	return false
}

// accDeleteSecret is the net for a run that failed before its own destroy could
// delete the secret. A passing run leaves nothing behind, so this normally finds
// nothing and does nothing.
func accDeleteSecret(t *testing.T, name string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), accCleanupTimeout)
	defer cancel()

	if !accSecretExists(ctx, t, name) {
		return
	}
	t.Logf("deleting secret %s", name)
	if _, err := accClient(t).API().DeleteSecrets(ctx, name); err != nil {
		t.Errorf("deleting secret %s: %v", name, err)
	}
}
