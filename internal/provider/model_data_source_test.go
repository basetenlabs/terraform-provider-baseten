package provider

import (
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	fwdatasource "github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestModelDataSourceSchema(t *testing.T) {
	resp := &fwdatasource.SchemaResponse{}
	newModelDataSource().Schema(t.Context(), fwdatasource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema is invalid: %v", diags)
	}
}

func TestModelDataSourceValidateConfig(t *testing.T) {
	tests := []struct {
		name      string
		config    modelDataSourceModel
		wantError bool
	}{
		{
			name:   "NameOnly",
			config: modelDataSourceModel{Name: types.StringValue("whisper")},
		},
		{
			name:   "IDOnly",
			config: modelDataSourceModel{ID: types.StringValue("model-1")},
		},
		{
			name:   "NameWithinTeam",
			config: modelDataSourceModel{Name: types.StringValue("whisper"), TeamName: types.StringValue("ml")},
		},
		{
			name: "BothIdentifiers",
			config: modelDataSourceModel{
				Name: types.StringValue("whisper"),
				ID:   types.StringValue("model-1"),
			},
			wantError: true,
		},
		{
			name:      "NeitherIdentifier",
			config:    modelDataSourceModel{},
			wantError: true,
		},
		{
			name: "BothTeamAttributes",
			config: modelDataSourceModel{
				Name:     types.StringValue("whisper"),
				TeamID:   types.StringValue("team-ml"),
				TeamName: types.StringValue("ml"),
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, _, dataSourceSchema := testModelDataSource(t, nil)

			resp := &fwdatasource.ValidateConfigResponse{}
			d.ValidateConfig(t.Context(), fwdatasource.ValidateConfigRequest{
				Config: testModelDataSourceConfig(t, dataSourceSchema, test.config),
			}, resp)

			if resp.Diagnostics.HasError() != test.wantError {
				t.Errorf("got error %t, want %t: %v", resp.Diagnostics.HasError(), test.wantError, resp.Diagnostics)
			}
		})
	}
}

func TestModelDataSourceReadByName(t *testing.T) {
	productionDeploymentID := "deployment-9"
	d, fake, dataSourceSchema := testModelDataSource(t, map[string]any{
		"GET /v1/models": managementapi.Models{Models: []managementapi.Model{{
			Id:                     "model-1",
			Name:                   "whisper",
			TeamName:               "Default Team",
			CreatedAt:              testTimestamp(t),
			DeploymentsCount:       3,
			InstanceTypeName:       "1x2",
			ProductionDeploymentId: &productionDeploymentID,
		}}},
	})

	got := testModelDataSourceRead(t, d, dataSourceSchema, modelDataSourceModel{
		Name: types.StringValue("whisper"),
	})

	requests := fake.requestsTo("GET", "/v1/models")
	if len(requests) != 1 {
		t.Fatalf("got %d model lookups, want 1: %v", len(requests), fake.requests)
	}
	if query := requests[0].Query.Get("name"); query != "whisper" {
		t.Errorf("got name query %q, want %q", query, "whisper")
	}

	if got.ID.ValueString() != "model-1" {
		t.Errorf("got id %q, want %q", got.ID.ValueString(), "model-1")
	}
	if got.CreatedAt.ValueString() != "2026-08-19T12:00:00Z" {
		t.Errorf("got created_at %q, want the model's timestamp", got.CreatedAt.ValueString())
	}
	if got.DeploymentsCount.ValueInt64() != 3 {
		t.Errorf("got deployments_count %v, want 3", got.DeploymentsCount)
	}
	if got.InstanceTypeName.ValueString() != "1x2" {
		t.Errorf("got instance_type_name %q, want %q", got.InstanceTypeName.ValueString(), "1x2")
	}
	if got.ProductionDeploymentID.ValueString() != productionDeploymentID {
		t.Errorf("got production_deployment_id %v, want %q", got.ProductionDeploymentID, productionDeploymentID)
	}
	// Reported as absent rather than as an empty string, so a configuration can
	// test it with a null check.
	if !got.DevelopmentDeploymentID.IsNull() {
		t.Errorf("got development_deployment_id %v, want null", got.DevelopmentDeploymentID)
	}
	// No team was configured, so the organization's default team was addressed
	// and there is no ID to report.
	if !got.TeamID.IsNull() {
		t.Errorf("got team_id %v, want null for the default team", got.TeamID)
	}
	if got.TeamName.ValueString() != "Default Team" {
		t.Errorf("got team_name %q, want %q", got.TeamName.ValueString(), "Default Team")
	}
}

func TestModelDataSourceReadByID(t *testing.T) {
	d, fake, dataSourceSchema := testModelDataSource(t, map[string]any{
		"GET /v1/models/model-1": managementapi.Model{
			Id: "model-1", Name: "whisper", TeamName: "ml", CreatedAt: testTimestamp(t),
		},
	})

	got := testModelDataSourceRead(t, d, dataSourceSchema, modelDataSourceModel{
		ID: types.StringValue("model-1"),
	})

	if requests := fake.requestsTo("GET", "/v1/models/model-1"); len(requests) != 1 {
		t.Fatalf("got %d model lookups, want 1: %v", len(requests), fake.requests)
	}
	if got.Name.ValueString() != "whisper" {
		t.Errorf("got name %q, want %q", got.Name.ValueString(), "whisper")
	}
	if !got.ProductionDeploymentID.IsNull() {
		t.Errorf("got production_deployment_id %v, want null when nothing is promoted", got.ProductionDeploymentID)
	}
}

// A configured team narrows the lookup, since model names are unique only within
// a team, and the resolved ID is reported back.
func TestModelDataSourceReadWithinTeam(t *testing.T) {
	d, fake, dataSourceSchema := testModelDataSource(t, map[string]any{
		"GET /v1/teams": managementapi.Teams{Teams: []managementapi.Team{{Id: "team-ml", Name: "ml"}}},
		"GET /v1/teams/team-ml/models": managementapi.Models{Models: []managementapi.Model{{
			Id: "model-1", Name: "whisper", TeamName: "ml", CreatedAt: testTimestamp(t),
		}}},
	})

	got := testModelDataSourceRead(t, d, dataSourceSchema, modelDataSourceModel{
		Name:     types.StringValue("whisper"),
		TeamName: types.StringValue("ml"),
	})

	if requests := fake.requestsTo("GET", "/v1/teams/team-ml/models"); len(requests) != 1 {
		t.Fatalf("got %d team-scoped lookups, want 1: %v", len(requests), fake.requests)
	}
	if requests := fake.requestsTo("GET", "/v1/models"); len(requests) != 0 {
		t.Errorf("got %d organization-wide lookups, want none", len(requests))
	}
	if got.TeamID.ValueString() != "team-ml" {
		t.Errorf("got team_id %v, want the resolved team-ml", got.TeamID)
	}
}

// A data source cannot create anything, so a missing model always fails, naming
// the attribute the configuration set.
func TestModelDataSourceReadMissingModelFails(t *testing.T) {
	tests := []struct {
		name       string
		config     modelDataSourceModel
		responses  map[string]any
		wantDetail string
	}{
		{
			name:       "ByName",
			config:     modelDataSourceModel{Name: types.StringValue("gone")},
			responses:  map[string]any{"GET /v1/models": managementapi.Models{}},
			wantDetail: `"gone"`,
		},
		{
			name:       "ByID",
			config:     modelDataSourceModel{ID: types.StringValue("model-gone")},
			responses:  map[string]any{},
			wantDetail: `"model-gone"`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			d, _, dataSourceSchema := testModelDataSource(t, test.responses)

			resp := &fwdatasource.ReadResponse{State: tfsdk.State{Schema: dataSourceSchema}}
			d.Read(t.Context(), fwdatasource.ReadRequest{
				Config: testModelDataSourceConfig(t, dataSourceSchema, test.config),
			}, resp)

			if !resp.Diagnostics.HasError() {
				t.Fatalf("got no error, want one for a model that does not exist")
			}
			if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, test.wantDetail) {
				t.Errorf("got detail %q, want it to contain %s", detail, test.wantDetail)
			}
		})
	}
}

func testModelDataSource(t *testing.T, responses map[string]any) (*modelDataSource, *fakeAPI, schema.Schema) {
	t.Helper()

	d := newModelDataSource()
	schemaResp := &fwdatasource.SchemaResponse{}
	d.Schema(t.Context(), fwdatasource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", schemaResp.Diagnostics)
	}

	fake, managementClient := newFakeAPI(t, responses)
	dataSource := d.(*modelDataSource)
	dataSource.client = managementClient
	return dataSource, fake, schemaResp.Schema
}

// testModelDataSourceConfig renders a model as the configuration Terraform
// supplies. Every attribute the test leaves alone is null, which is what an
// omitted one is in configuration: unlike a plan, configuration is never
// backfilled with a computed value.
func testModelDataSourceConfig(t *testing.T, dataSourceSchema schema.Schema, config modelDataSourceModel) tfsdk.Config {
	t.Helper()

	// tfsdk.Config has no Set, so the raw value is built through a state.
	state := tfsdk.State{Schema: dataSourceSchema}
	if diags := state.Set(t.Context(), &config); diags.HasError() {
		t.Fatalf("building data source config: %v", diags)
	}
	return tfsdk.Config{Schema: dataSourceSchema, Raw: state.Raw}
}

func testModelDataSourceRead(
	t *testing.T,
	d *modelDataSource,
	dataSourceSchema schema.Schema,
	config modelDataSourceModel,
) modelDataSourceModel {
	t.Helper()

	resp := &fwdatasource.ReadResponse{State: tfsdk.State{Schema: dataSourceSchema}}
	d.Read(t.Context(), fwdatasource.ReadRequest{
		Config: testModelDataSourceConfig(t, dataSourceSchema, config),
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	var got modelDataSourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	return got
}
