package provider

import (
	"os"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
)

const (
	e2eAPIKeyEnvVar    = "BASETEN_E2E_TEST_API_KEY"
	e2eRemoteURLEnvVar = "BASETEN_E2E_TEST_REMOTE_URL"
)

// testAccProtoV6ProviderFactories serves the provider in-process for
// acceptance tests, so no built binary is involved.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"baseten": providerserver.NewProtocol6WithError(New("test")()),
}

// testAccPreCheck skips unless e2e credentials are present, matching the CLI,
// where forks have no secrets. It maps the e2e variables onto the ones the
// provider itself reads.
//
// Both are required. Without an explicit remote URL the provider would fall
// back to the production API, and these tests create and delete real secrets.
func testAccPreCheck(t *testing.T) {
	t.Helper()

	apiKey := os.Getenv(e2eAPIKeyEnvVar)
	remoteURL := os.Getenv(e2eRemoteURLEnvVar)
	if apiKey == "" || remoteURL == "" {
		t.Skipf("%s and %s must both be set", e2eAPIKeyEnvVar, e2eRemoteURLEnvVar)
	}

	t.Setenv(apiKeyEnvVar, apiKey)
	t.Setenv(remoteURLEnvVar, remoteURL)
}

func TestProviderSchema(t *testing.T) {
	resp := &provider.SchemaResponse{}
	New("test")().Schema(t.Context(), provider.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema is invalid: %v", diags)
	}
}
