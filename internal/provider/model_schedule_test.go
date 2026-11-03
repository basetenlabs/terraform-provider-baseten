package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestModelScheduleUpdatePreservesIDsAndDeletesRemovedSchedules(t *testing.T) {
	current := testAPISchedules(t, `{"timezone":"UTC","schedules":[
		{"id":"keep-id","name":"business","cadence":"DAILY","enabled":true,"weekdays":["MONDAY"],"start_hour":9,"end_hour":17,"start_minute":0,"end_minute":0,"autoscaling_settings":{"min_replica":1,"max_replica":4}},
		{"id":"remove-id","name":"old","cadence":"DAILY","enabled":true,"weekdays":["MONDAY"],"start_hour":9,"end_hour":17,"start_minute":0,"end_minute":0,"autoscaling_settings":{"min_replica":1,"max_replica":4}}
	]}`)
	plan := testScheduleMap(t, map[string]modelScheduleModel{
		"business": testSchedule(t, "DAILY"),
		"burst":    testSchedule(t, "HOURLY"),
		"event":    testSchedule(t, "ONE_TIME"),
	})
	update, diags := modelScheduleUpdate(t.Context(), plan, types.StringUnknown(), current)
	if diags.HasError() {
		t.Fatal(diags)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Timezone        *string  `json:"timezone"`
		DeleteSchedules []string `json:"delete_schedules"`
		Schedules       []struct {
			ID          *string        `json:"id"`
			Name        string         `json:"name"`
			Cadence     string         `json:"cadence"`
			Enabled     bool           `json:"enabled"`
			StartHour   *int           `json:"start_hour"`
			StartAt     string         `json:"start_at"`
			Autoscaling map[string]any `json:"autoscaling_settings"`
		} `json:"schedules"`
	}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"timezone"`) {
		t.Fatalf("omitted timezone was mutated: %s", encoded)
	}
	if len(payload.DeleteSchedules) != 1 || payload.DeleteSchedules[0] != "remove-id" {
		t.Fatalf("wrong deletes: %s", encoded)
	}
	if len(payload.Schedules) != 3 {
		t.Fatalf("wrong upserts: %s", encoded)
	}
	if payload.Schedules[1].ID == nil || *payload.Schedules[1].ID != "keep-id" {
		t.Fatalf("existing ID was not reused: %s", encoded)
	}
	for _, schedule := range payload.Schedules {
		if schedule.Name != "business" && schedule.ID != nil {
			t.Fatalf("new schedule has ID: %s", encoded)
		}
		if schedule.Enabled {
			t.Fatalf("explicit false was lost: %s", encoded)
		}
		if value, present := schedule.Autoscaling["concurrency_target"]; !present || value != nil {
			t.Fatalf("inheritance must encode explicit null: %s", encoded)
		}
		if schedule.Autoscaling["min_replica"] != float64(2) {
			t.Fatalf("wrong replica override: %s", encoded)
		}
		if schedule.Cadence == "HOURLY" && schedule.StartHour != nil {
			t.Fatalf("unrestricted hourly schedule has hours: %s", encoded)
		}
		if schedule.Cadence == "ONE_TIME" && schedule.StartAt != "2030-01-01T09:00:00Z" {
			t.Fatalf("wrong one-time start: %s", encoded)
		}
	}
}

func TestModelScheduleUpdateOmissionAndClearing(t *testing.T) {
	current := testAPISchedules(t, `{"timezone":"UTC","schedules":[{"id":"old-id","name":"old","cadence":"ONE_TIME","enabled":false,"start_at":"2030-01-01T09:00:00Z","end_at":"2030-01-01T10:00:00Z","autoscaling_settings":{"min_replica":0,"max_replica":1}}]}`)
	for _, planned := range []types.Map{types.MapNull(modelScheduleObjectType()), types.MapUnknown(modelScheduleObjectType())} {
		update, diags := modelScheduleUpdate(t.Context(), planned, types.StringUnknown(), current)
		if diags.HasError() || update != nil {
			t.Fatalf("omitted schedules mutated: %v, %v", update, diags)
		}
	}
	update, diags := modelScheduleUpdate(t.Context(), testScheduleMap(t, map[string]modelScheduleModel{}), types.StringValue(""), current)
	if diags.HasError() {
		t.Fatal(diags)
	}
	encoded, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"delete_schedules":["old-id"],"schedules":[],"timezone":null}` {
		t.Fatalf("unexpected clear payload: %s", encoded)
	}
	update, diags = modelScheduleUpdate(t.Context(), types.MapUnknown(modelScheduleObjectType()), types.StringValue(""), nil)
	if diags.HasError() || update != nil {
		t.Fatalf("empty read-back timezone caused an unrelated mutation: %v, %v", update, diags)
	}
}

func TestModelBackpressureUpdate(t *testing.T) {
	for _, policy := range []types.String{types.StringNull(), types.StringUnknown()} {
		if update := modelBackpressureUpdate(policy); update != nil {
			t.Fatalf("omitted policy mutated: %v", update)
		}
	}
	for _, test := range []struct{ policy, want string }{
		{"QUEUE_ON_FULL", `{"policy":"QUEUE_ON_FULL"}`},
		{"REJECT_ON_FULL", `{"policy":"REJECT_ON_FULL"}`},
		{"", `{"policy":null}`},
	} {
		encoded, err := json.Marshal(modelBackpressureUpdate(types.StringValue(test.policy)))
		if err != nil {
			t.Fatal(err)
		}
		if string(encoded) != test.want {
			t.Fatalf("policy %q: got %s, want %s", test.policy, encoded, test.want)
		}
	}
}

// Exercise the real SDK and Create through HTTP. The serving read deliberately
// lags the mutation response, so the newly assigned ID must come from PATCH.
func TestModelResourceCreateWithSchedules(t *testing.T) {
	created := testAPIEnvironment(t, "staging", 0)
	updated := created
	updated.AutoscalingSchedules = testAPISchedules(t, `{"timezone":"UTC","schedules":[{"id":"assigned-id","name":"business","cadence":"DAILY","enabled":false,"weekdays":["MONDAY"],"start_hour":9,"end_hour":17,"start_minute":0,"end_minute":0,"autoscaling_settings":{"min_replica":2,"max_replica":4}}]}`)
	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models":                                managementapi.Models{Models: []managementapi.Model{{Id: "model-1", Name: "whisper", TeamName: "Default Team", CreatedAt: testTimestamp(t)}}},
		"GET /v1/models/model-1/environments":           managementapi.Environments{},
		"POST /v1/models/model-1/environments":          created,
		"PATCH /v1/models/model-1/environments/staging": managementapi.UpdateEnvironmentResponse{Environment: updated, Status: managementapi.UpdateAutoscalingSettingsStatus_ACCEPTED},
		"GET /v1/models/model-1/environments/staging":   created,
	})
	plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelPlanValue(t, modelSchema, modelResourceModel{
		Name: types.StringValue("whisper"), DeletionProtection: types.BoolValue(true), EnvironmentDeletionProtection: types.BoolValue(true),
		Environments: testModelPlanEnvironments(t, map[string]modelEnvironmentModel{"staging": {
			RequestBackpressurePolicy: types.StringValue("REJECT_ON_FULL"), AutoscalingScheduleTimezone: types.StringValue("UTC"),
			AutoscalingSchedules: testScheduleMap(t, map[string]modelScheduleModel{"business": testSchedule(t, "DAILY")}),
		}}),
	})}
	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: modelSchema}}
	r.Create(t.Context(), fwresource.CreateRequest{Plan: plan, Config: tfsdk.Config{Schema: modelSchema, Raw: plan.Raw}}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	posts := fake.requestsTo("POST", "/v1/models/model-1/environments")
	patches := fake.requestsTo("PATCH", "/v1/models/model-1/environments/staging")
	if len(posts) != 1 || len(patches) != 1 {
		t.Fatalf("unexpected requests: %v", fake.requests)
	}
	if !strings.Contains(posts[0].Body, `"request_backpressure_settings":{"policy":"REJECT_ON_FULL"}`) {
		t.Fatalf("create omitted policy: %s", posts[0].Body)
	}
	if !strings.Contains(patches[0].Body, `"autoscaling_schedule_settings"`) || strings.Contains(patches[0].Body, `"request_backpressure_settings"`) {
		t.Fatalf("wrong follow-up patch: %s", patches[0].Body)
	}
	var state modelResourceModel
	if diags := resp.State.Get(t.Context(), &state); diags.HasError() {
		t.Fatal(diags)
	}
	entries, diags := modelEnvironmentEntries(t.Context(), state.Environments)
	if diags.HasError() {
		t.Fatal(diags)
	}
	var schedules map[string]modelScheduleModel
	if diags := entries["staging"].AutoscalingSchedules.ElementsAs(t.Context(), &schedules, false); diags.HasError() {
		t.Fatal(diags)
	}
	if schedules["business"].ID.ValueString() != "assigned-id" {
		t.Fatalf("missing generated ID: %v", schedules)
	}
	if entries["staging"].RequestBackpressurePolicy.ValueString() != "REJECT_ON_FULL" {
		t.Fatal("stale read overwrote configured policy")
	}
}

func TestModelSchedulesReadAndMerge(t *testing.T) {
	collection := testAPISchedules(t, `{"timezone":"UTC","schedules":[{"id":"hourly-id","name":"burst","cadence":"HOURLY","enabled":false,"weekdays":["MONDAY"],"start_minute":0,"end_minute":0,"autoscaling_settings":{"min_replica":1,"max_replica":3}},{"id":"once-id","name":"event","cadence":"ONE_TIME","enabled":false,"start_at":"2030-01-01T09:00:00Z","end_at":"2030-01-01T10:00:00Z","autoscaling_settings":{"min_replica":1,"max_replica":3}}]}`)
	fetched, timezone, diags := modelSchedulesFromAPI(t.Context(), collection)
	if diags.HasError() || timezone.ValueString() != "UTC" {
		t.Fatalf("read: %v, %v", timezone, diags)
	}
	planned := testScheduleMap(t, map[string]modelScheduleModel{"burst": testSchedule(t, "HOURLY"), "event": testSchedule(t, "ONE_TIME")})
	merged, diags := modelMergeSchedules(t.Context(), planned, fetched)
	if diags.HasError() {
		t.Fatal(diags)
	}
	var entries map[string]modelScheduleModel
	if diags := merged.ElementsAs(t.Context(), &entries, false); diags.HasError() {
		t.Fatal(diags)
	}
	if entries["burst"].ID.ValueString() != "hourly-id" || entries["event"].ID.ValueString() != "once-id" {
		t.Fatalf("wrong IDs: %v", entries)
	}
	var settings modelAutoscalingModel
	if diags := entries["burst"].Autoscaling.As(t.Context(), &settings, modelObjectAsOptions); diags.HasError() {
		t.Fatal(diags)
	}
	if settings.MinReplica.ValueInt64() != 2 || !settings.ConcurrencyTarget.IsNull() {
		t.Fatalf("read overwrote planned overrides: %v", settings)
	}
	update, diags := modelScheduleUpdate(t.Context(), merged, timezone, collection)
	if diags.HasError() {
		t.Fatal(diags)
	}
	for _, item := range *update.Schedules {
		encoded, err := json.Marshal(item)
		if err != nil || !strings.Contains(string(encoded), `"id":`) {
			t.Fatalf("second apply would duplicate schedule: %s, %v", encoded, err)
		}
	}
	_, diags = modelMergeSchedules(t.Context(), planned, testScheduleMap(t, map[string]modelScheduleModel{}))
	if !diags.HasError() {
		t.Fatal("missing mutation result must not create an unknown ID in state")
	}
}

func TestModelScheduleValidation(t *testing.T) {
	for _, cadence := range []string{"DAILY", "HOURLY", "ONE_TIME"} {
		var diags diag.Diagnostics
		modelValidateScheduleSettings(t.Context(), modelEnvironmentModel{AutoscalingSchedules: testScheduleMap(t, map[string]modelScheduleModel{"valid": testSchedule(t, cadence)})}, &diags)
		if diags.HasError() {
			t.Fatalf("valid %s schedule: %v", cadence, diags)
		}
	}
	for _, test := range []struct {
		name   string
		change func(*modelScheduleModel)
	}{
		{"bad cadence", func(s *modelScheduleModel) { s.Cadence = types.StringValue("YEARLY") }},
		{"missing daily hour", func(s *modelScheduleModel) { s.StartHour = types.Int64Null() }},
		{"bad minute", func(s *modelScheduleModel) { s.EndMinute = types.Int64Value(60) }},
		{"bad weekday", func(s *modelScheduleModel) {
			s.Weekdays, _ = types.SetValueFrom(t.Context(), types.StringType, []string{"monday"})
		}},
		{"mixed time fields", func(s *modelScheduleModel) { s.StartAt = types.StringValue("2030-01-01T09:00:00Z") }},
	} {
		t.Run(test.name, func(t *testing.T) {
			schedule := testSchedule(t, "DAILY")
			test.change(&schedule)
			var diags diag.Diagnostics
			modelValidateScheduleSettings(t.Context(), modelEnvironmentModel{AutoscalingSchedules: testScheduleMap(t, map[string]modelScheduleModel{"invalid": schedule})}, &diags)
			if !diags.HasError() {
				t.Fatal("invalid schedule accepted")
			}
		})
	}
	var diags diag.Diagnostics
	modelValidateScheduleSettings(t.Context(), modelEnvironmentModel{RequestBackpressurePolicy: types.StringValue("invalid"), AutoscalingSchedules: types.MapUnknown(modelScheduleObjectType())}, &diags)
	if !diags.HasError() {
		t.Fatal("invalid backpressure policy accepted")
	}
}

func TestModelScheduleValidationPartialUnknowns(t *testing.T) {
	for _, test := range []struct {
		name      string
		change    func(*modelScheduleModel)
		wantError bool
	}{
		{"unknown weekday element", func(s *modelScheduleModel) {
			s.Weekdays, _ = types.SetValue(types.StringType, []attr.Value{types.StringValue("MONDAY"), types.StringUnknown()})
		}, false},
		{"invalid known weekday alongside unknown", func(s *modelScheduleModel) {
			s.Weekdays, _ = types.SetValue(types.StringType, []attr.Value{types.StringValue("monday"), types.StringUnknown()})
		}, true},
		{"empty weekdays", func(s *modelScheduleModel) { s.Weekdays, _ = types.SetValue(types.StringType, nil) }, true},
		{"null weekday element", func(s *modelScheduleModel) {
			s.Weekdays, _ = types.SetValue(types.StringType, []attr.Value{types.StringNull()})
		}, true},
		{"unknown start hour", func(s *modelScheduleModel) { s.StartHour = types.Int64Unknown(); s.EndHour = types.Int64Null() }, false},
		{"unknown end hour", func(s *modelScheduleModel) { s.StartHour = types.Int64Null(); s.EndHour = types.Int64Unknown() }, false},
		{"resolved absent hours", func(s *modelScheduleModel) { s.StartHour = types.Int64Null(); s.EndHour = types.Int64Null() }, false},
		{"resolved unpaired hours", func(s *modelScheduleModel) { s.StartHour = types.Int64Value(9); s.EndHour = types.Int64Null() }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			schedule := testSchedule(t, "HOURLY")
			test.change(&schedule)
			var diags diag.Diagnostics
			modelValidateScheduleSettings(t.Context(), modelEnvironmentModel{AutoscalingSchedules: testScheduleMap(t, map[string]modelScheduleModel{"burst": schedule})}, &diags)
			if diags.HasError() != test.wantError {
				t.Fatalf("diagnostics = %v, want error %t", diags, test.wantError)
			}
		})
	}
}

// A saved plan must not make omitted settings authoritative when another
// client changes the environment before apply.
func TestModelOmittedSchedulesPreserveExternalChanges(t *testing.T) {
	old := testAPIEnvironment(t, "production", 1)
	old.AutoscalingSchedules = testAPISchedules(t, `{"timezone":"UTC","schedules":[{"id":"old-id","name":"business","cadence":"HOURLY","enabled":false,"weekdays":["MONDAY"],"start_minute":0,"end_minute":0,"autoscaling_settings":{"min_replica":1,"max_replica":3}}]}`)
	oldPolicy := managementapi.RequestBackpressurePolicy_REJECT_ON_FULL
	old.RequestBackpressureSettings.Policy = &oldPolicy
	live := old
	// The old schedule was removed and replaced outside Terraform after plan.
	live.AutoscalingSchedules = testAPISchedules(t, `{"timezone":"Europe/London","schedules":[{"id":"external-id","name":"external","cadence":"HOURLY","enabled":true,"weekdays":["FRIDAY"],"start_minute":5,"end_minute":10,"autoscaling_settings":{"min_replica":2,"max_replica":4}}]}`)
	livePolicy := managementapi.RequestBackpressurePolicy_QUEUE_ON_FULL
	live.RequestBackpressureSettings.Policy = &livePolicy
	r, fake, modelSchema := testModelResource(t, map[string]any{
		"GET /v1/models/model-1/environments": managementapi.Environments{Environments: []managementapi.Environment{live}},
	})
	entry, diags := modelEnvironmentFromAPI(t.Context(), &old)
	if diags.HasError() {
		t.Fatal(diags)
	}
	prior := modelResourceModel{ID: types.StringValue("model-1"), Name: types.StringValue("whisper"), EnvironmentDeletionProtection: types.BoolValue(true), Environments: testModelEnvironments(t, map[string]modelEnvironmentModel{"production": entry})}
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-op", true: "update"}[changed], func(t *testing.T) {
			planned := prior
			planned.EnvironmentDeletionProtection = types.BoolValue(!changed)
			config := planned
			config.Environments = testModelEnvironments(t, map[string]modelEnvironmentModel{"production": {}})
			plan := tfsdk.Plan{Schema: modelSchema, Raw: testModelValue(t, modelSchema, planned)}
			resp := &fwresource.ModifyPlanResponse{Plan: plan}
			r.ModifyPlan(t.Context(), fwresource.ModifyPlanRequest{Plan: plan, Config: tfsdk.Config{Schema: modelSchema, Raw: testModelValue(t, modelSchema, config)}, State: tfsdk.State{Schema: modelSchema, Raw: testModelValue(t, modelSchema, prior)}}, resp)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			if !changed {
				if !resp.Plan.Raw.Equal(plan.Raw) {
					t.Fatal("no-op plan changed")
				}
				return
			}
			if diags := resp.Plan.Get(t.Context(), &planned); diags.HasError() {
				t.Fatal(diags)
			}
			entries, diags := modelEnvironmentEntries(t.Context(), planned.Environments)
			if diags.HasError() {
				t.Fatal(diags)
			}
			got := entries["production"]
			if !got.AutoscalingSchedules.IsUnknown() || !got.AutoscalingScheduleTimezone.IsUnknown() || !got.RequestBackpressurePolicy.IsUnknown() {
				t.Fatal("omitted settings were frozen in the update plan")
			}
			// Keep unrelated legacy settings out of this reconciliation request.
			got.Autoscaling = types.ObjectNull(attrTypesOf(modelAutoscalingSchemaAttributes()))
			got.Promotion = types.ObjectNull(attrTypesOf(modelPromotionSchemaAttributes()))
			result, diags := r.reconcileEnvironments(t.Context(), "model-1", testModelEnvironments(t, map[string]modelEnvironmentModel{"production": got}), config.Environments, prior.Environments, false)
			if diags.HasError() {
				t.Fatal(diags)
			}
			entries, diags = modelEnvironmentEntries(t.Context(), result)
			if diags.HasError() {
				t.Fatal(diags)
			}
			got = entries["production"]
			if len(got.AutoscalingSchedules.Elements()) != 1 || got.AutoscalingSchedules.Elements()["external"] == nil || got.AutoscalingScheduleTimezone.ValueString() != "Europe/London" || got.RequestBackpressurePolicy.ValueString() != "QUEUE_ON_FULL" {
				t.Fatalf("external changes lost: %+v", got)
			}
			if len(fake.requestsTo("PATCH", "/v1/models/model-1/environments/production")) != 0 {
				t.Fatal("omitted settings caused a mutation")
			}
		})
	}
}

func TestModelScheduleTimestampRefresh(t *testing.T) {
	current := testSchedule(t, "ONE_TIME")
	current.ID = types.StringValue("once-id")
	previous := current
	previous.StartAt = types.StringValue("2030-01-01T10:00:00+01:00")
	previous.EndAt = types.StringValue("2030-01-01T11:00:00+01:00")
	result, diags := modelPreserveScheduleTimes(t.Context(),
		testScheduleMap(t, map[string]modelScheduleModel{"event": current}),
		testScheduleMap(t, map[string]modelScheduleModel{"event": previous}))
	if diags.HasError() {
		t.Fatal(diags)
	}
	var entries map[string]modelScheduleModel
	if diags := result.ElementsAs(t.Context(), &entries, false); diags.HasError() {
		t.Fatal(diags)
	}
	if !entries["event"].StartAt.Equal(previous.StartAt) || !entries["event"].EndAt.Equal(previous.EndAt) {
		t.Fatal("equivalent timestamp formatting would cause a perpetual diff")
	}
	current.StartAt = types.StringValue("2030-01-02T09:00:00Z")
	result, diags = modelPreserveScheduleTimes(t.Context(),
		testScheduleMap(t, map[string]modelScheduleModel{"event": current}),
		testScheduleMap(t, map[string]modelScheduleModel{"event": previous}))
	if diags.HasError() {
		t.Fatal(diags)
	}
	if diags := result.ElementsAs(t.Context(), &entries, false); diags.HasError() {
		t.Fatal(diags)
	}
	if !entries["event"].StartAt.Equal(current.StartAt) {
		t.Fatal("a real timestamp change was hidden")
	}
}

func testAPISchedules(t *testing.T, body string) *managementapi.EnvironmentAutoscalingSchedules {
	t.Helper()
	var collection managementapi.EnvironmentAutoscalingSchedules
	if err := json.Unmarshal([]byte(body), &collection); err != nil {
		t.Fatal(err)
	}
	return &collection
}

func testScheduleMap(t *testing.T, entries map[string]modelScheduleModel) types.Map {
	t.Helper()
	value, diags := types.MapValueFrom(t.Context(), modelScheduleObjectType(), entries)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return value
}

func testSchedule(t *testing.T, cadence string) modelScheduleModel {
	t.Helper()
	schedule := modelScheduleModel{ID: types.StringUnknown(), Cadence: types.StringValue(cadence), Enabled: types.BoolValue(false), Weekdays: types.SetNull(types.StringType),
		Autoscaling: testModelObject(t, modelAutoscalingSchemaAttributes(), modelAutoscalingModel{MinReplica: types.Int64Value(2), MaxReplica: types.Int64Value(4)}),
	}
	if cadence == "ONE_TIME" {
		schedule.StartAt = types.StringValue("2030-01-01T09:00:00Z")
		schedule.EndAt = types.StringValue("2030-01-01T10:00:00Z")
	} else {
		schedule.Weekdays, _ = types.SetValueFrom(t.Context(), types.StringType, []string{"MONDAY"})
		schedule.StartMinute = types.Int64Value(0)
		schedule.EndMinute = types.Int64Value(0)
		if cadence == "DAILY" {
			schedule.StartHour = types.Int64Value(9)
			schedule.EndHour = types.Int64Value(17)
		}
	}
	return schedule
}
