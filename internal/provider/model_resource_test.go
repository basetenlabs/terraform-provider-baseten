package provider

import (
	"path/filepath"
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
		Environments: testModelPlanEnvironments(t, map[string]modelEnvironmentModel{
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
		name        string
		environment string
		protection  bool
		wantError   bool
		wantWarning bool
	}{
		{name: "ProtectedRemovalFails", environment: "staging", protection: true, wantError: true},
		{name: "UnprotectedRemovalPlans", environment: "staging", protection: false},
		// Baseten refuses to delete production either way, so removing it warns
		// rather than erroring. Erroring would send the user to turn off a flag
		// that only leads to a failing apply.
		{name: "ProtectedProductionRemovalWarns", environment: "production", protection: true, wantWarning: true},
		{name: "UnprotectedProductionRemovalWarns", environment: "production", protection: false, wantWarning: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, _, modelSchema := testModelResource(t, nil)

			// Prior state manages the environment; the configuration no longer
			// mentions it.
			state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
				ID:                            types.StringValue("model-1"),
				Name:                          types.StringValue("whisper"),
				EnvironmentDeletionProtection: types.BoolValue(test.protection),
				CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
				Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
					test.environment: {},
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
			if got := resp.Diagnostics.WarningsCount() > 0; got != test.wantWarning {
				t.Fatalf("got warnings %v, want %v: %v", got, test.wantWarning, resp.Diagnostics)
			}
		})
	}
}

// TestModelResourceReconcileSkipsProductionDeletion covers the apply side of the
// same rule: production dropped from the configuration is left alone rather than
// deleted, which Baseten rejects outright.
func TestModelResourceReconcileSkipsProductionDeletion(t *testing.T) {
	production := testAPIEnvironment(t, "production", 1)
	staging := testAPIEnvironment(t, "staging", 0)

	r, fake, _ := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{
			Environments: []managementapi.Environment{production, staging},
		},
		"DELETE /v1/models/model-1/environments/staging": map[string]any{},
	})

	// Both were managed; the configuration now names neither, with protection off.
	prior := testModelEnvironments(t, map[string]modelEnvironmentModel{
		"production": {},
		"staging":    {},
	})
	planned := testModelEnvironments(t, map[string]modelEnvironmentModel{})

	_, diags := r.reconcileEnvironments(t.Context(), "model-1", planned, prior, true)
	if diags.HasError() {
		t.Fatalf("got diagnostics %v, want none", diags)
	}

	if got := fake.requestsTo("DELETE", "/v1/models/model-1/environments/production"); len(got) > 0 {
		t.Errorf("got %d deletes of production, want none", len(got))
	}
	if got := fake.requestsTo("DELETE", "/v1/models/model-1/environments/staging"); len(got) != 1 {
		t.Errorf("got %d deletes of staging, want 1", len(got))
	}
}

// TestModelResourceUpdateKeepsPlannedSettingsOverReadBack covers the rule
// Terraform enforces on every apply: a value the plan settled is the value the
// apply has to return. Baseten can disagree with the plan for two reasons,
// settings that apply asynchronously and settings Baseten moves on its own, and
// recording either would fail the apply with an inconsistent-result error naming
// the provider. The next refresh reports it as drift instead.
func TestModelResourceUpdateKeepsPlannedSettingsOverReadBack(t *testing.T) {
	production := testAPIEnvironment(t, "production", 1)

	// The read taken after the write disagrees with the plan about every kind of
	// attribute: one the configuration set, one it left to the prior value, and a
	// read-only one.
	scaleDownDelay := 900
	readBack := production
	readBack.AutoscalingSettings.MinReplica = 1
	readBack.AutoscalingSettings.MaxReplica = 9
	readBack.AutoscalingSettings.ScaleDownDelay = &scaleDownDelay
	readBack.InstanceType = managementapi.InstanceType{Id: "4x16", Name: "4x16"}

	r, _, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{
			Environments: []managementapi.Environment{production},
		},
		"PATCH /v1/models/model-1/environments/production": managementapi.UpdateAutoscalingSettingsResponse{
			Status: managementapi.UpdateAutoscalingSettingsStatus_ACCEPTED,
		},
		"GET /v1/models/model-1/environments/production": readBack,
	})

	settings := func(minReplica, maxReplica int64) modelEnvironmentModel {
		return modelEnvironmentModel{
			Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
				MinReplica: types.Int64Value(minReplica),
				MaxReplica: types.Int64Value(maxReplica),
			}),
			InstanceTypeName: types.StringValue("1x2"),
		}
	}
	prior := modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": settings(1, 4),
		}),
	}
	// min_replica is configured. max_replica, scale_down_delay, and
	// instance_type_name are not, so Terraform plans the prior values, a null
	// among them, and a null is just as settled as a number.
	planned := prior
	planned.Environments = testModelEnvironments(t, map[string]modelEnvironmentModel{
		"production": settings(3, 4),
	})

	priorValue := testModelValue(t, modelSchema, prior)
	resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: modelSchema, Raw: priorValue}}
	r.Update(t.Context(), fwresource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, planned)},
		State: tfsdk.State{Schema: modelSchema, Raw: priorValue},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	entries, diags := modelEnvironmentEntries(t.Context(), got.Environments)
	if diags.HasError() {
		t.Fatalf("reading environments from state: %v", diags)
	}
	autoscaling := testModelAutoscalingOf(t, entries["production"])
	if autoscaling.MinReplica.ValueInt64() != 3 {
		t.Errorf("got min_replica %v, want the configured 3 despite the read reporting 1",
			autoscaling.MinReplica)
	}
	if autoscaling.MaxReplica.ValueInt64() != 4 {
		t.Errorf("got max_replica %v, want the planned 4 despite the read reporting 9",
			autoscaling.MaxReplica)
	}
	if !autoscaling.ScaleDownDelay.IsNull() {
		t.Errorf("got scale_down_delay %v, want the planned null despite the read reporting 900",
			autoscaling.ScaleDownDelay)
	}
	if entries["production"].InstanceTypeName.ValueString() != "1x2" {
		t.Errorf("got instance_type_name %q, want the planned %q despite the read reporting %q",
			entries["production"].InstanceTypeName.ValueString(), "1x2", "4x16")
	}
}

// TestModelResourceUpdateFailedPushKeepsSourceHash covers a push that fails not
// being recorded as one that happened. The source hash is the only thing that
// decides whether the next plan pushes, and no refresh can recover it, so state
// has to keep the hash of the source that was last pushed. The environment
// writes that did land are recorded, since those already exist at Baseten.
func TestModelResourceUpdateFailedPushKeepsSourceHash(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})
	production := testAPIEnvironment(t, "production", 1)

	// The push routes are absent, so the push fails after the environment writes.
	r, _, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{
			Environments: []managementapi.Environment{production},
		},
		"PATCH /v1/models/model-1/environments/production": managementapi.UpdateAutoscalingSettingsResponse{
			Status: managementapi.UpdateAutoscalingSettingsStatus_ACCEPTED,
		},
		"GET /v1/models/model-1/environments/production": production,
	})

	prior := modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		DeploymentID:                  types.StringValue("deployment-1"),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(dir),
			SourceHash: types.StringValue("hash-pushed"),
		}),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": {
				Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
					MinReplica: types.Int64Value(1),
				}),
			},
		}),
	}
	// The plan carries the edited source's hash and, because it plans to push,
	// an unknown deployment_id, which is what ModifyPlan settles.
	planned := prior
	planned.DeploymentID = types.StringUnknown()
	planned.Push = testModelPushObject(t, modelPushModel{
		ConfigDir:  types.StringValue(dir),
		SourceHash: types.StringValue("hash-edited"),
	})
	planned.Environments = testModelEnvironments(t, map[string]modelEnvironmentModel{
		"production": {
			Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
				MinReplica: types.Int64Value(3),
			}),
		},
	})

	priorValue := testModelValue(t, modelSchema, prior)
	state := tfsdk.State{Schema: modelSchema, Raw: priorValue}
	// The framework seeds the response with prior state, so a provider that
	// returns early records nothing.
	resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: modelSchema, Raw: priorValue}}
	r.Update(t.Context(), fwresource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, planned)},
		State: state,
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("got no error for a push that could not reach Baseten, want one")
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	gotPush, hasPush, diags := modelPushEntry(t.Context(), got.Push)
	if diags.HasError() {
		t.Fatalf("reading push from state: %v", diags)
	}
	if !hasPush {
		t.Fatal("got no push in state, want the prior one")
	}
	if gotPush.SourceHash.ValueString() != "hash-pushed" {
		t.Errorf("got source_hash %q, want the last pushed %q: the next plan would read the source as "+
			"unchanged and never retry the push", gotPush.SourceHash.ValueString(), "hash-pushed")
	}
	// The deployment already serving is untouched, so state still names it.
	if got.DeploymentID.ValueString() != "deployment-1" {
		t.Errorf("got deployment_id %q, want the prior %q", got.DeploymentID.ValueString(), "deployment-1")
	}

	entries, diags := modelEnvironmentEntries(t.Context(), got.Environments)
	if diags.HasError() {
		t.Fatalf("reading environments from state: %v", diags)
	}
	if got := testModelAutoscalingOf(t, entries["production"]).MinReplica.ValueInt64(); got != 3 {
		t.Errorf("got production min_replica %d, want the written 3: the environment write landed", got)
	}
}

// TestModelResourceUpdateDeployedPushRecordsFailedWait is the other half: a push
// Baseten accepted and then failed to bring up did happen, so state records it
// even though the apply errors. Reverting the hash here would have the next apply
// push the same source again, alongside the deployment this one created.
func TestModelResourceUpdateDeployedPushRecordsFailedWait(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})
	model := managementapi.Model{
		Id: "model-1", Name: "whisper", TeamName: "Default Team", CreatedAt: testTimestamp(t),
	}
	// The push reaches Baseten and the deployment it creates never becomes
	// active, which fails the wait without un-pushing anything.
	failed := managementapi.Deployment{
		Id:        "deployment-2",
		ModelId:   "model-1",
		Status:    managementapi.DeploymentStatus_BUILD_FAILED,
		CreatedAt: testTimestamp(t),
	}

	r, _, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{},
		// A prepare that issues no upload target is how a format built from the
		// config alone pushes, which is what makes a push fakeable here.
		"POST /v1/prepare_model_upload": managementapi.PrepareModelUploadResponse{},
		"POST /v1/models/model-1/deployments": managementapi.CreatedModelDeployment{
			Model: model, Deployment: failed,
		},
		"GET /v1/models/model-1/deployments/deployment-2": failed,
	})

	prior := modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		DeploymentID:                  types.StringValue("deployment-1"),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(dir),
			SourceHash: types.StringValue("hash-pushed"),
			Wait:       types.BoolValue(true),
		}),
	}
	planned := prior
	planned.DeploymentID = types.StringUnknown()
	planned.Push = testModelPushObject(t, modelPushModel{
		ConfigDir:  types.StringValue(dir),
		SourceHash: types.StringValue("hash-edited"),
		Wait:       types.BoolValue(true),
	})

	priorValue := testModelValue(t, modelSchema, prior)
	resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: modelSchema, Raw: priorValue}}
	r.Update(t.Context(), fwresource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, planned)},
		State: tfsdk.State{Schema: modelSchema, Raw: priorValue},
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("got no error for a deployment that did not become active, want one")
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	gotPush, _, diags := modelPushEntry(t.Context(), got.Push)
	if diags.HasError() {
		t.Fatalf("reading push from state: %v", diags)
	}
	if gotPush.SourceHash.ValueString() != "hash-edited" {
		t.Errorf("got source_hash %q, want the pushed %q: the source was deployed, so pushing it again "+
			"would deploy a duplicate", gotPush.SourceHash.ValueString(), "hash-edited")
	}
	if got.DeploymentID.ValueString() != "deployment-2" {
		t.Errorf("got deployment_id %q, want the created %q", got.DeploymentID.ValueString(), "deployment-2")
	}
}

// TestModelResourceModifyPlanRemovingPushNullsDeploymentID covers the plan
// promising what the apply does. deployment_id is Computed, so Terraform carries
// the prior value into the plan, and an apply that nulls it without the plan
// saying so fails with an inconsistent result.
func TestModelResourceModifyPlanRemovingPushNullsDeploymentID(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})

	r, _, modelSchema := testModelResource(t, nil)

	state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		DeploymentID:                  types.StringValue("deployment-1"),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(dir),
			SourceHash: types.StringValue("hash-pushed"),
		}),
	})}

	// The configuration dropped push. Everything else, including deployment_id,
	// comes through the plan as prior state had it.
	config := tfsdk.Config{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		Name:                          types.StringValue("whisper"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
	})}
	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		DeploymentID:                  types.StringValue("deployment-1"),
	})}

	resp := &fwresource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(t.Context(), fwresource.ModifyPlanRequest{Config: config, Plan: plan, State: state}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got errors %v, want warnings only", resp.Diagnostics)
	}
	if got := testDiagnosticsText(resp.Diagnostics.Warnings()); !strings.Contains(got, "no longer Terraform-managed") {
		t.Errorf("got warnings %q, want one saying the model is no longer managed", got)
	}

	var planned modelResourceModel
	if diags := resp.Plan.Get(t.Context(), &planned); diags.HasError() {
		t.Fatalf("reading resulting plan: %v", diags)
	}
	if !planned.DeploymentID.IsNull() {
		t.Errorf("got planned deployment_id %v, want null to match what the apply writes", planned.DeploymentID)
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

// TestModelResourceCreateTakesOverExistingModel covers managed mode meeting a
// model that already exists: it is taken over at whatever is deployed, and the
// local hash is recorded as the starting point rather than pushed.
//
// It is also how a create whose push failed recovers. Terraform records nothing
// for a failed create, so the model that push created is untracked, and the next
// apply comes back through here and adopts it instead of pushing it again.
// Recording state on a failed create would be worse: Terraform taints a resource
// whose create errored and replaces it on the next apply, so a create that only
// timed out waiting would come back as a destroy, and with deletion_protection
// off that destroy deletes the model.
func TestModelResourceCreateTakesOverExistingModel(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})

	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models": managementapi.Models{Models: []managementapi.Model{{
			Id: "model-1", Name: "whisper", TeamName: "Default Team", CreatedAt: testTimestamp(t),
		}}},
		"GET /v1/models/model-1/environments": managementapi.Environments{},
	})

	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelPlanValue(t, modelSchema, modelResourceModel{
		Name:                          types.StringValue("whisper"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(dir),
			SourceHash: types.StringValue("hash-local"),
		}),
	})}

	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: modelSchema}}
	r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	if pushes := fake.requestsTo("POST", "/v1/prepare_model_upload"); len(pushes) != 0 {
		t.Errorf("got %d pushes, want none: the model already existed", len(pushes))
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	if got.ID.ValueString() != "model-1" {
		t.Errorf("got id %q, want the existing %q", got.ID.ValueString(), "model-1")
	}
	// Nothing was pushed, so no deployment is Terraform's.
	if !got.DeploymentID.IsNull() {
		t.Errorf("got deployment_id %v, want null", got.DeploymentID)
	}
	gotPush, _, diags := modelPushEntry(t.Context(), got.Push)
	if diags.HasError() {
		t.Fatalf("reading push from state: %v", diags)
	}
	if gotPush.SourceHash.ValueString() != "hash-local" {
		t.Errorf("got source_hash %q, want the local %q recorded as the baseline",
			gotPush.SourceHash.ValueString(), "hash-local")
	}
}

// TestModelResourceCreatePushesNewModel covers the only path that creates a
// model, since Baseten has no way to create one without deploying something.
func TestModelResourceCreatePushesNewModel(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})

	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models":                managementapi.Models{},
		"POST /v1/prepare_model_upload": managementapi.PrepareModelUploadResponse{},
		"POST /v1/models": managementapi.CreatedModelDeployment{
			Model: managementapi.Model{
				Id: "model-1", Name: "whisper", TeamName: "Default Team", CreatedAt: testTimestamp(t),
			},
			Deployment: managementapi.Deployment{
				Id: "deployment-1", ModelId: "model-1", CreatedAt: testTimestamp(t),
				Status: managementapi.DeploymentStatus_BUILDING,
			},
		},
		"GET /v1/models/model-1/environments": managementapi.Environments{},
	})

	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelPlanValue(t, modelSchema, modelResourceModel{
		Name:                          types.StringValue("whisper"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(dir),
			SourceHash: types.StringValue("hash-local"),
		}),
	})}

	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: modelSchema}}
	r.Create(t.Context(), fwresource.CreateRequest{Plan: plan}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	if creates := fake.requestsTo("POST", "/v1/models"); len(creates) != 1 {
		t.Fatalf("got %d model creates, want 1", len(creates))
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	if got.ID.ValueString() != "model-1" {
		t.Errorf("got id %q, want %q", got.ID.ValueString(), "model-1")
	}
	if got.DeploymentID.ValueString() != "deployment-1" {
		t.Errorf("got deployment_id %q, want the pushed %q", got.DeploymentID.ValueString(), "deployment-1")
	}
}

// TestModelResourceUpdateAddingPushAdoptsSource covers adopted becoming managed.
// There is no baseline to compare against, so the local hash becomes the
// baseline and nothing is pushed: what is deployed keeps serving, and the first
// edit after this pushes.
func TestModelResourceUpdateAddingPushAdoptsSource(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})

	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{},
	})

	prior := modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
	}
	planned := prior
	planned.Push = testModelPushObject(t, modelPushModel{
		ConfigDir:  types.StringValue(dir),
		SourceHash: types.StringValue("hash-local"),
	})

	priorValue := testModelValue(t, modelSchema, prior)
	resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: modelSchema, Raw: priorValue}}
	r.Update(t.Context(), fwresource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, planned)},
		State: tfsdk.State{Schema: modelSchema, Raw: priorValue},
	}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	if pushes := fake.requestsTo("POST", "/v1/prepare_model_upload"); len(pushes) != 0 {
		t.Errorf("got %d pushes, want none: adding push takes the source over", len(pushes))
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	gotPush, hasPush, diags := modelPushEntry(t.Context(), got.Push)
	if diags.HasError() {
		t.Fatalf("reading push from state: %v", diags)
	}
	if !hasPush {
		t.Fatal("got no push in state, want the one just added")
	}
	if gotPush.SourceHash.ValueString() != "hash-local" {
		t.Errorf("got source_hash %q, want the local %q as the new baseline",
			gotPush.SourceHash.ValueString(), "hash-local")
	}
	if !got.DeploymentID.IsNull() {
		t.Errorf("got deployment_id %v, want null: nothing was pushed", got.DeploymentID)
	}
}

// TestModelResourceUpdateEnvironmentFailurePushesNothing covers the ordering
// promise. Environment settings are written before a push so a new deployment
// rolls out under the settings the apply asked for, which means a settings
// failure has to stop the push too, and record nothing.
func TestModelResourceUpdateEnvironmentFailurePushesNothing(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})
	production := testAPIEnvironment(t, "production", 1)

	// The PATCH route is absent, so writing the settings fails.
	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{
			Environments: []managementapi.Environment{production},
		},
	})

	prior := modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(dir),
			SourceHash: types.StringValue("hash-pushed"),
		}),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": {
				Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
					MinReplica: types.Int64Value(1),
				}),
			},
		}),
	}
	planned := prior
	planned.DeploymentID = types.StringUnknown()
	planned.Push = testModelPushObject(t, modelPushModel{
		ConfigDir:  types.StringValue(dir),
		SourceHash: types.StringValue("hash-edited"),
	})
	planned.Environments = testModelEnvironments(t, map[string]modelEnvironmentModel{
		"production": {
			Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), &modelAutoscalingModel{
				MinReplica: types.Int64Value(3),
			}),
		},
	})

	priorValue := testModelValue(t, modelSchema, prior)
	resp := &fwresource.UpdateResponse{State: tfsdk.State{Schema: modelSchema, Raw: priorValue}}
	r.Update(t.Context(), fwresource.UpdateRequest{
		Plan:  tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, planned)},
		State: tfsdk.State{Schema: modelSchema, Raw: priorValue},
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("got no error for a settings write that failed, want one")
	}
	if pushes := fake.requestsTo("POST", "/v1/prepare_model_upload"); len(pushes) != 0 {
		t.Errorf("got %d pushes, want none: the settings the deployment would roll out under never landed",
			len(pushes))
	}

	var got modelResourceModel
	if diags := resp.State.Get(t.Context(), &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	gotPush, _, diags := modelPushEntry(t.Context(), got.Push)
	if diags.HasError() {
		t.Fatalf("reading push from state: %v", diags)
	}
	if gotPush.SourceHash.ValueString() != "hash-pushed" {
		t.Errorf("got source_hash %q, want the untouched %q", gotPush.SourceHash.ValueString(), "hash-pushed")
	}
	entries, diags := modelEnvironmentEntries(t.Context(), got.Environments)
	if diags.HasError() {
		t.Fatalf("reading environments from state: %v", diags)
	}
	if got := testModelAutoscalingOf(t, entries["production"]).MinReplica.ValueInt64(); got != 1 {
		t.Errorf("got production min_replica %d, want the untouched 1", got)
	}
}

// TestModelResourceModifyPlanConfiguredSourceHash covers taking the hash over by
// hand, which is how a source Terraform cannot see gets pushed on demand. The
// configured value is used verbatim and the source is never read, proven here by
// a config_dir that does not exist.
func TestModelResourceModifyPlanConfiguredSourceHash(t *testing.T) {
	r, _, modelSchema := testModelResource(t, nil)

	configured := modelResourceModel{
		Name:                          types.StringValue("whisper"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(filepath.Join(t.TempDir(), "does-not-exist")),
			SourceHash: types.StringValue("pinned-2"),
		}),
	}
	configuredValue := testModelValue(t, modelSchema, configured)

	state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
		DeploymentID:                  types.StringValue("deployment-1"),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue(filepath.Join(t.TempDir(), "does-not-exist")),
			SourceHash: types.StringValue("pinned-1"),
		}),
	})}

	resp := &fwresource.ModifyPlanResponse{Plan: tfsdk.Plan{Schema: modelSchema, Raw: configuredValue}}
	r.ModifyPlan(t.Context(), fwresource.ModifyPlanRequest{
		Config: tfsdk.Config{Schema: modelSchema, Raw: configuredValue},
		Plan:   tfsdk.Plan{Schema: modelSchema, Raw: configuredValue},
		State:  state,
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got errors %v, want none: the source must not be read", resp.Diagnostics)
	}
	if got := testDiagnosticsText(resp.Diagnostics.Warnings()); !strings.Contains(got, "will be pushed") {
		t.Errorf("got warnings %q, want one announcing the push", got)
	}

	var planned modelResourceModel
	if diags := resp.Plan.Get(t.Context(), &planned); diags.HasError() {
		t.Fatalf("reading resulting plan: %v", diags)
	}
	plannedPush, _, diags := modelPushEntry(t.Context(), planned.Push)
	if diags.HasError() {
		t.Fatalf("reading push from the plan: %v", diags)
	}
	if plannedPush.SourceHash.ValueString() != "pinned-2" {
		t.Errorf("got planned source_hash %q, want the configured %q verbatim",
			plannedPush.SourceHash.ValueString(), "pinned-2")
	}
	// The push replaces what is deployed, so the plan cannot promise the old one.
	if !planned.DeploymentID.IsUnknown() {
		t.Errorf("got planned deployment_id %v, want unknown", planned.DeploymentID)
	}
}

// TestModelResourceDeleteManaged covers destroy in managed mode, where the model
// is Terraform's to delete and deletion_protection is the only thing standing in
// front of every deployment under it.
func TestModelResourceDeleteManaged(t *testing.T) {
	tests := []struct {
		name       string
		protection bool
		wantDelete int
	}{
		{name: "ProtectedKeepsModel", protection: true},
		{name: "UnprotectedDeletesModel", protection: false, wantDelete: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, fake, modelSchema := testModelResource(t, map[string]any{
				"DELETE /v1/models/model-1": map[string]any{},
			})

			state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
				ID:                            types.StringValue("model-1"),
				Name:                          types.StringValue("whisper"),
				CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
				DeletionProtection:            types.BoolValue(test.protection),
				EnvironmentDeletionProtection: types.BoolValue(true),
				Push: testModelPushObject(t, modelPushModel{
					ConfigDir:  types.StringValue("./whisper"),
					SourceHash: types.StringValue("hash-pushed"),
				}),
			})}

			resp := &fwresource.DeleteResponse{State: state}
			r.Delete(t.Context(), fwresource.DeleteRequest{State: state}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
			}

			if got := fake.requestsTo("DELETE", "/v1/models/model-1"); len(got) != test.wantDelete {
				t.Errorf("got %d model deletes, want %d", len(got), test.wantDelete)
			}
		})
	}
}

// TestModelResourceDeleteAdopted covers destroy in adopted mode, which never
// deletes the model it did not create. Environments are the exception, since
// Terraform did create those, and only where they are unprotected.
func TestModelResourceDeleteAdopted(t *testing.T) {
	r, fake, modelSchema := testModelResource(t, map[string]any{
		"DELETE /v1/models/model-1/environments/staging": map[string]any{},
	})

	state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(false),
		EnvironmentDeletionProtection: types.BoolValue(false),
		Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{
			"production": {},
			"staging":    {},
		}),
	})}

	resp := &fwresource.DeleteResponse{State: state}
	r.Delete(t.Context(), fwresource.DeleteRequest{State: state}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	// deletion_protection is false, and it still must not delete a model this
	// configuration only adopted.
	if got := fake.requestsTo("DELETE", "/v1/models/model-1"); len(got) != 0 {
		t.Errorf("got %d model deletes, want none in adopted mode", len(got))
	}
	if got := fake.requestsTo("DELETE", "/v1/models/model-1/environments/staging"); len(got) != 1 {
		t.Errorf("got %d staging deletes, want 1", len(got))
	}
	if got := fake.requestsTo("DELETE", "/v1/models/model-1/environments/production"); len(got) != 0 {
		t.Errorf("got %d production deletes, want none: Baseten rejects them", len(got))
	}
	if got := testDiagnosticsText(resp.Diagnostics.Warnings()); !strings.Contains(got, "Production") {
		t.Errorf("got warnings %q, want one saying production was left alone", got)
	}
}

// TestModelResourceDeleteToleratesMissingModel covers a destroy racing anything
// that already deleted the model. The end state is the one asked for, so it is
// not an error.
func TestModelResourceDeleteToleratesMissingModel(t *testing.T) {
	r, _, modelSchema := testModelResource(t, nil)

	state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(false),
		EnvironmentDeletionProtection: types.BoolValue(true),
		Push: testModelPushObject(t, modelPushModel{
			ConfigDir:  types.StringValue("./whisper"),
			SourceHash: types.StringValue("hash-pushed"),
		}),
	})}

	resp := &fwresource.DeleteResponse{State: state}
	r.Delete(t.Context(), fwresource.DeleteRequest{State: state}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none for a model that is already gone", resp.Diagnostics)
	}
}

// TestModelResourceReadForgetsDeletedModel covers a refresh finding the model
// gone. Removing it from state has the next plan offer to create it, which is
// the only useful answer.
func TestModelResourceReadForgetsDeletedModel(t *testing.T) {
	r, _, modelSchema := testModelResource(t, nil)

	state := tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		ID:                            types.StringValue("model-1"),
		Name:                          types.StringValue("whisper"),
		CreatedAt:                     types.StringValue("2026-08-19T12:00:00Z"),
		DeletionProtection:            types.BoolValue(true),
		EnvironmentDeletionProtection: types.BoolValue(true),
	})}

	resp := &fwresource.ReadResponse{State: state}
	r.Read(t.Context(), fwresource.ReadRequest{State: state}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("got state %v, want it removed", resp.State.Raw)
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

// testModelPlanEnvironments renders the environments map the way Terraform plans
// one, where every attribute the configuration leaves out arrives unknown rather
// than null. The difference is the whole contract: unknown is Terraform asking
// the apply to fill a value in, and null is a value the plan settled and the
// apply has to return unchanged. Passing nulls instead would let a provider that
// overwrites settled values with whatever Baseten reports pass its tests, and
// then fail a real apply with an inconsistent-result error.
func testModelPlanEnvironments(t *testing.T, entries map[string]modelEnvironmentModel) types.Map {
	t.Helper()

	planned := make(map[string]attr.Value, len(entries))
	for name, element := range testModelEnvironments(t, entries).Elements() {
		planned[name] = testModelPlannedValue(t, element)
	}
	environments, diags := types.MapValue(modelEnvironmentObjectType(), planned)
	if diags.HasError() {
		t.Fatalf("building planned environments map: %v", diags)
	}
	return environments
}

// testModelPlannedValue makes every null in a value unknown, recursively, since
// a create plans an Optional+Computed attribute it has no configuration or prior
// state for as unknown.
func testModelPlannedValue(t *testing.T, value attr.Value) attr.Value {
	t.Helper()

	if object, isObject := value.(types.Object); isObject && !object.IsNull() && !object.IsUnknown() {
		attributes := make(map[string]attr.Value, len(object.Attributes()))
		for name, attribute := range object.Attributes() {
			attributes[name] = testModelPlannedValue(t, attribute)
		}
		planned, diags := types.ObjectValue(object.AttributeTypes(t.Context()), attributes)
		if diags.HasError() {
			t.Fatalf("building planned object: %v", diags)
		}
		return planned
	}
	if !value.IsNull() {
		return value
	}

	valueType := value.Type(t.Context())
	unknown, err := valueType.ValueFromTerraform(t.Context(),
		tftypes.NewValue(valueType.TerraformType(t.Context()), tftypes.UnknownValue))
	if err != nil {
		t.Fatalf("building an unknown %s: %v", valueType, err)
	}
	return unknown
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
