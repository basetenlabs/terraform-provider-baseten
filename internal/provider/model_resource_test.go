package provider

import (
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestModelResourceSchema(t *testing.T) {
	resp := &fwresource.SchemaResponse{}
	newModelResource().Schema(t.Context(), fwresource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema is invalid: %v", diags)
	}
}

func TestModelResourceValidateConfig(t *testing.T) {
	tests := []struct {
		name      string
		config    modelResourceModel
		wantError bool
	}{
		{
			name:   "NameOnly",
			config: modelResourceModel{Name: types.StringValue("whisper")},
		},
		{
			name:   "IDOnly",
			config: modelResourceModel{ID: types.StringValue("model-1")},
		},
		{
			name:      "NameAndIDConflict",
			config:    modelResourceModel{Name: types.StringValue("whisper"), ID: types.StringValue("model-1")},
			wantError: true,
		},
		{
			name:      "NeitherNameNorID",
			config:    modelResourceModel{},
			wantError: true,
		},
		{
			name: "TeamIDAndTeamNameConflict",
			config: modelResourceModel{
				Name:     types.StringValue("whisper"),
				TeamID:   types.StringValue("team-ml"),
				TeamName: types.StringValue("ml"),
			},
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, _, modelSchema := testModelResource(t, nil)

			config := tfsdk.Config{Schema: modelSchema, Raw: testModelValue(t, modelSchema, test.config)}

			resp := &fwresource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), fwresource.ValidateConfigRequest{Config: config}, resp)

			if test.wantError && !resp.Diagnostics.HasError() {
				t.Fatal("got no error, want one")
			}
			if !test.wantError && resp.Diagnostics.HasError() {
				t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
			}
		})
	}
}

// TestModelResourceCreateUpsertsEnvironments covers the whole reconcile: an
// environment that exists is patched, one that does not is created, and a
// configured value survives a read that has not caught up yet.
func TestModelResourceCreateUpsertsEnvironments(t *testing.T) {
	production := testAPIEnvironment(t, "production", 1)
	staging := testAPIEnvironment(t, "staging", 0)

	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models": managementapi.Models{Models: []managementapi.Model{{
			Id: "model-1", Name: "whisper", TeamName: "Default Team", CreatedAt: testTimestamp(t),
		}}},
		"GET /v1/models/model-1/environments": managementapi.Environments{
			Environments: []managementapi.Environment{production},
		},
		"PATCH /v1/models/model-1/environments/production": managementapi.UpdateAutoscalingSettingsResponse{
			Status:  managementapi.UpdateAutoscalingSettingsStatus_ACCEPTED,
			Message: "Your request to update environment settings is complete",
		},
		// Settings apply asynchronously, so the read back still reports the old
		// min_replica of 1. State must carry the configured 3 regardless.
		"GET /v1/models/model-1/environments/production": production,
		"POST /v1/models/model-1/environments":           staging,
	})

	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelPlanValue(t, modelSchema, modelResourceModel{
		Name:                          types.StringValue("whisper"),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": {
				Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
					MinReplica: types.Int64Value(3),
				}),
			},
			"staging": {
				Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
					MaxReplica: types.Int64Value(2),
				}),
			},
		}),
	})}

	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: modelSchema}}
	r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	if patches := fake.requestsTo("PATCH", "/v1/models/model-1/environments/production"); len(patches) != 1 {
		t.Errorf("got %d PATCHes to production, want 1", len(patches))
	} else if !strings.Contains(patches[0].Body, `"min_replica":3`) {
		t.Errorf("got production PATCH body %q, want it to carry min_replica 3", patches[0].Body)
	}
	posts := fake.requestsTo("POST", "/v1/models/model-1/environments")
	if len(posts) != 1 {
		t.Fatalf("got %d POSTs to environments, want 1", len(posts))
	}
	if !strings.Contains(posts[0].Body, `"name":"staging"`) || !strings.Contains(posts[0].Body, `"max_replica":2`) {
		t.Errorf("got staging POST body %q, want it to create staging with max_replica 2", posts[0].Body)
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	if got.ID.ValueString() != "model-1" {
		t.Errorf("got id %q, want %q", got.ID.ValueString(), "model-1")
	}
	if !got.TeamID.IsNull() {
		t.Errorf("got team_id %v, want null for the default team", got.TeamID)
	}

	entries, diags := modelEnvironmentEntries(t.Context(), got.Environments)
	if diags.HasError() {
		t.Fatalf("reading environments from state: %v", diags)
	}
	productionAutoscaling := testModelAutoscalingOf(t, entries["production"])
	if productionAutoscaling.MinReplica.ValueInt64() != 3 {
		t.Errorf("got production min_replica %v, want the configured 3", productionAutoscaling.MinReplica)
	}
	// Not configured, so it comes from the read.
	if productionAutoscaling.MaxReplica.ValueInt64() != 4 {
		t.Errorf("got production max_replica %v, want the read-back 4", productionAutoscaling.MaxReplica)
	}
	stagingAutoscaling := testModelAutoscalingOf(t, entries["staging"])
	if stagingAutoscaling.MaxReplica.ValueInt64() != 2 {
		t.Errorf("got staging max_replica %v, want the configured 2", stagingAutoscaling.MaxReplica)
	}
	if entries["staging"].InstanceTypeName.ValueString() != "1x2" {
		t.Errorf("got staging instance_type_name %q, want %q", entries["staging"].InstanceTypeName.ValueString(), "1x2")
	}
}

// A configured team_name has to narrow the lookup, since model names are unique
// only within a team. On a create team_id is unknown, so mistaking unknown for a
// set value would search the whole organization and adopt another team's model.
func TestModelResourceCreateResolvesTeamName(t *testing.T) {
	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/teams": managementapi.Teams{Teams: []managementapi.Team{
			{Id: "team-ml", Name: "ml"},
		}},
		"GET /v1/teams/team-ml/models": managementapi.Models{Models: []managementapi.Model{{
			Id: "model-1", Name: "whisper", TeamName: "ml", CreatedAt: testTimestamp(t),
		}}},
		"GET /v1/models/model-1/environments": managementapi.Environments{},
	})

	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelPlanValue(t, modelSchema, modelResourceModel{
		Name:                          types.StringValue("whisper"),
		TeamName:                      types.StringValue("ml"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
	})}

	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: modelSchema}}
	r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	if requests := fake.requestsTo("GET", "/v1/teams/team-ml/models"); len(requests) != 1 {
		t.Errorf("got %d requests to the team's models, want 1: %v", len(requests), fake.requests)
	}
	if requests := fake.requestsTo("GET", "/v1/models"); len(requests) != 0 {
		t.Errorf("got %d organization-wide model lookups, want none", len(requests))
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	if got.TeamID.ValueString() != "team-ml" {
		t.Errorf("got team_id %v, want the resolved team-ml", got.TeamID)
	}
}

// Adopted mode cannot create a model, so a missing one fails the apply. The
// message has to name what the user configured; id is unknown here, not set.
func TestModelResourceCreateMissingModelNamesIt(t *testing.T) {
	r, _, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models": managementapi.Models{},
	})

	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelPlanValue(t, modelSchema, modelResourceModel{
		Name:                          types.StringValue("whisper"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
	})}

	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: modelSchema}}
	r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("got no error, want one for a model that does not exist")
	}
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, `"whisper"`) {
		t.Errorf("got detail %q, want it to name the configured model", detail)
	}
}

func TestModelResourceModifyPlanEnvironmentDeletionProtection(t *testing.T) {
	tests := []struct {
		name       string
		protection bool
		wantError  bool
	}{
		{name: "ProtectedRemovalFails", protection: true, wantError: true},
		{name: "UnprotectedRemovalPlans", protection: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, _, modelSchema := testModelResource(t, nil)

			// Prior state manages staging; the configuration no longer mentions it.
			state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
				ID:                            types.StringValue("model-1"),
				Name:                          types.StringValue("whisper"),
				EnvironmentDeletionProtection: types.BoolValue(test.protection),
				CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
				Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
					"staging": {},
				}),
			})}

			plannedValue := testModelValue(t, modelSchema, modelResourceModel{
				ID:                            types.StringValue("model-1"),
				Name:                          types.StringValue("whisper"),
				EnvironmentDeletionProtection: types.BoolValue(test.protection),
				CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
			})
			plan := tfsdk.Plan{Schema: modelSchema, Raw: plannedValue}
			config := tfsdk.Config{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
				Name: types.StringValue("whisper"),
			})}

			resp := &fwresource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(t.Context(), fwresource.ModifyPlanRequest{
				Config: config,
				Plan:   plan,
				State:  state,
			}, resp)

			if test.wantError {
				if !resp.Diagnostics.HasError() {
					t.Fatal("got no error for removing a protected environment, want one")
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
			}
		})
	}
}

func TestModelResourceModifyPlanWarnings(t *testing.T) {
	r, _, modelSchema := testModelResource(t, nil)

	// State holds a scale_down_delay the configuration dropped, and the
	// configuration tunes rolling deploy without enabling it.
	state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		EnvironmentDeletionProtection: types.BoolValue(true),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": {
				Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
					MinReplica:     types.Int64Value(1),
					ScaleDownDelay: types.Int64Value(900),
				}),
			},
		}),
	})}

	configured := modelResourceModel{
		Name:                          types.StringValue("whisper"),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": {
				Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
					MinReplica: types.Int64Value(1),
				}),
				Promotion: testModelObject(t, modelPromotionSchemaAttributes(), &modelPromotionModel{
					RollingDeploy: testModelObject(t, modelRollingDeploySchemaAttributes(), &modelRollingDeployModel{
						MaxSurgePercent: types.Int64Value(50),
					}),
				}),
			},
		}),
	}
	configuredValue := testModelValue(t, modelSchema, configured)
	config := tfsdk.Config{Schema: modelSchema, Raw: configuredValue}
	plan := tfsdk.Plan{Schema: modelSchema, Raw: configuredValue}

	resp := &fwresource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(t.Context(), fwresource.ModifyPlanRequest{Config: config, Plan: plan, State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got errors %v, want warnings only", resp.Diagnostics)
	}
	summaries := make([]string, 0, resp.Diagnostics.WarningsCount())
	for _, warning := range resp.Diagnostics.Warnings() {
		summaries = append(summaries, warning.Summary()+": "+warning.Detail())
	}
	joined := strings.Join(summaries, "\n")
	if !strings.Contains(joined, "autoscaling.scale_down_delay") {
		t.Errorf("got warnings %q, want one naming the dropped scale_down_delay", joined)
	}
	if !strings.Contains(joined, "Rolling deploy is not enabled") {
		t.Errorf("got warnings %q, want one about rolling deploy being off", joined)
	}
}

func testModelResource(t *testing.T, responses map[string]any) (*modelResource, *fakeAPI, schema.Schema) {
	t.Helper()

	r := newModelResource()
	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(t.Context(), fwresource.SchemaRequest{}, schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", schemaResp.Diagnostics)
	}

	fake, managementClient := newFakeAPI(t, responses)
	modelResource := r.(*modelResource)
	modelResource.client = managementClient
	return modelResource, fake, schemaResp.Schema
}

// testModelValue renders a resource model as the raw value a config, plan, or
// state carries. tfsdk.Config has no Set, so one raw value serves all three.
func testModelValue(t *testing.T, modelSchema schema.Schema, model modelResourceModel) tftypes.Value {
	t.Helper()

	// Zero-value collections and nested objects carry no type information, so
	// they cannot convert. Default them to typed nulls, which is what a
	// configuration omitting them produces.
	if model.Environments.ElementType(t.Context()) == nil {
		model.Environments = types.MapNull(modelEnvironmentObjectType())
	}
	if len(model.Push.AttributeTypes(t.Context())) == 0 {
		model.Push = types.ObjectNull(modelPushObjectType().AttrTypes)
	}
	if len(model.Timeouts.AttributeTypes(t.Context())) == 0 {
		model.Timeouts = timeouts.Value{
			Object: types.ObjectNull(map[string]attr.Type{
				"create": types.StringType,
				"update": types.StringType,
			}),
		}
	}
	state := tfsdk.State{Schema: modelSchema}
	if diags := state.Set(t.Context(), &model); diags.HasError() {
		t.Fatalf("building resource value: %v", diags)
	}
	return state.Raw
}

// testModelPlanValue renders a resource model as the plan Terraform produces for
// a create, where every Optional+Computed attribute the configuration left out
// arrives unknown rather than null. Passing null instead would let a provider
// that mistakes unknown for a set value pass its tests and then address the
// wrong team, or report an empty ID, against a real Terraform.
func testModelPlanValue(t *testing.T, modelSchema schema.Schema, model modelResourceModel) tftypes.Value {
	t.Helper()

	for _, attribute := range []*types.String{
		&model.ID, &model.Name, &model.TeamID, &model.TeamName,
		&model.CreatedAt, &model.DeploymentID,
	} {
		if attribute.IsNull() {
			*attribute = types.StringUnknown()
		}
	}
	return testModelValue(t, modelSchema, model)
}

func testModelEnvironments(t *testing.T, entries map[string]modelEnvironmentModel) types.Map {
	t.Helper()

	// A zero-value nested object has no attribute types, so it cannot convert.
	for name, entry := range entries {
		if len(entry.Autoscaling.AttributeTypes(t.Context())) == 0 {
			entry.Autoscaling = types.ObjectNull(attrTypesOf(modelAutoscalingSchemaAttributes()))
		}
		if len(entry.Promotion.AttributeTypes(t.Context())) == 0 {
			entry.Promotion = types.ObjectNull(attrTypesOf(modelPromotionSchemaAttributes()))
		}
		entries[name] = entry
	}

	environments, diags := types.MapValueFrom(t.Context(), modelEnvironmentObjectType(), entries)
	if diags.HasError() {
		t.Fatalf("building environments map: %v", diags)
	}
	return environments
}

func testModelObject(t *testing.T, attributes map[string]schema.Attribute, value any) types.Object {
	t.Helper()

	object, diags := types.ObjectValueFrom(t.Context(), attrTypesOf(attributes), value)
	if diags.HasError() {
		t.Fatalf("building object: %v", diags)
	}
	return object
}

func testModelAutoscalingOf(t *testing.T, entry modelEnvironmentModel) modelAutoscalingModel {
	t.Helper()

	var autoscaling modelAutoscalingModel
	if diags := entry.Autoscaling.As(t.Context(), &autoscaling, modelObjectAsOptions); diags.HasError() {
		t.Fatalf("reading autoscaling: %v", diags)
	}
	return autoscaling
}

// testAPIEnvironment is an environment as the API reports it. max_replica is
// always 4, so a test can tell a read-back value from a configured one.
func testAPIEnvironment(t *testing.T, name string, minReplica int) managementapi.Environment {
	t.Helper()

	return managementapi.Environment{
		Name:      name,
		ModelId:   "model-1",
		CreatedAt: testTimestamp(t),
		AutoscalingSettings: managementapi.AutoscalingSettings{
			MinReplica:        minReplica,
			MaxReplica:        4,
			ConcurrencyTarget: 1,
		},
		PromotionSettings: managementapi.PromotionSettings{},
		InstanceType:      managementapi.InstanceType{Id: "1x2", Name: "1x2"},
	}
}
