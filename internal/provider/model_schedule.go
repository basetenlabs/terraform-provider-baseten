package provider

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Names give Terraform a stable map key while the API assigns schedule IDs.
// A configured map owns the whole collection; a null/unknown map does not.
type modelScheduleModel struct {
	ID          types.String `tfsdk:"id"`
	Cadence     types.String `tfsdk:"cadence"`
	Enabled     types.Bool   `tfsdk:"enabled"`
	Weekdays    types.Set    `tfsdk:"weekdays"`
	StartHour   types.Int64  `tfsdk:"start_hour"`
	EndHour     types.Int64  `tfsdk:"end_hour"`
	StartMinute types.Int64  `tfsdk:"start_minute"`
	EndMinute   types.Int64  `tfsdk:"end_minute"`
	StartAt     types.String `tfsdk:"start_at"`
	EndAt       types.String `tfsdk:"end_at"`
	Autoscaling types.Object `tfsdk:"autoscaling"`
}

func modelScheduleSchemaAttributes() map[string]schema.Attribute {
	// Schedule overrides are complete replacements: null means inherit the
	// environment value, unlike the environment's partial autoscaling mutation.
	autoscaling := modelAutoscalingSchemaAttributes()
	for name, attribute := range autoscaling {
		setting := attribute.(schema.Int64Attribute)
		setting.Computed = false
		setting.MarkdownDescription = "Schedule override. Omit to inherit the environment value."
		if name == "min_replica" || name == "max_replica" {
			setting.Optional = false
			setting.Required = true
			setting.MarkdownDescription = "Number of replicas during this schedule."
		}
		autoscaling[name] = setting
	}
	return map[string]schema.Attribute{
		"id":           schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseNonNullStateForUnknown()}, MarkdownDescription: "Stable schedule ID assigned by Baseten."},
		"cadence":      schema.StringAttribute{Required: true, MarkdownDescription: "`DAILY`, `HOURLY`, or `ONE_TIME`."},
		"enabled":      schema.BoolAttribute{Required: true, MarkdownDescription: "Whether this schedule is enabled."},
		"weekdays":     schema.SetAttribute{Optional: true, ElementType: types.StringType, MarkdownDescription: "Weekdays for recurring schedules, such as `MONDAY` and `FRIDAY`. Omit for `ONE_TIME`."},
		"start_hour":   schema.Int64Attribute{Optional: true, MarkdownDescription: "Starting hour (0–23). Required for DAILY; omit both hours for unrestricted HOURLY schedules."},
		"end_hour":     schema.Int64Attribute{Optional: true, MarkdownDescription: "Ending hour (0–23). Required for DAILY; omit both hours for unrestricted HOURLY schedules."},
		"start_minute": schema.Int64Attribute{Optional: true, MarkdownDescription: "Starting minute (0–59), required for recurring schedules."},
		"end_minute":   schema.Int64Attribute{Optional: true, MarkdownDescription: "Ending minute (0–59), required for recurring schedules."},
		"start_at":     schema.StringAttribute{Optional: true, MarkdownDescription: "Inclusive start of a ONE_TIME schedule, in RFC 3339 format. New schedules must start in the future."},
		"end_at":       schema.StringAttribute{Optional: true, MarkdownDescription: "Exclusive end of a ONE_TIME schedule, in RFC 3339 format."},
		"autoscaling":  schema.SingleNestedAttribute{Required: true, Attributes: autoscaling, MarkdownDescription: "Autoscaling overrides during this schedule. Omitted optional overrides inherit the environment value."},
	}
}

func modelScheduleObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: attrTypesOf(modelScheduleSchemaAttributes())}
}

func modelSchedulesFromAPI(ctx context.Context, collection *managementapi.EnvironmentAutoscalingSchedules) (types.Map, types.String, diag.Diagnostics) {
	var diags diag.Diagnostics
	entries := map[string]modelScheduleModel{}
	timezone := types.StringValue("")
	if collection != nil {
		if collection.Timezone != nil {
			timezone = types.StringValue(*collection.Timezone)
		}
		for _, item := range collection.Schedules {
			value, err := item.ValueByDiscriminator()
			if err != nil {
				diags.AddError("Unable to read autoscaling schedule", err.Error())
				continue
			}
			entry := modelScheduleModel{Weekdays: types.SetNull(types.StringType)}
			var name string
			var settings managementapi.AutoscalingScheduleSettings
			switch schedule := value.(type) {
			case managementapi.AutoscalingSchedule:
				name, settings = schedule.Name, schedule.AutoscalingSettings
				entry.ID, entry.Cadence = types.StringValue(schedule.Id), types.StringValue(string(schedule.Cadence))
				entry.Enabled = types.BoolValue(schedule.Enabled)
				entry.StartHour, entry.EndHour = modelInt64PointerValue(schedule.StartHour), modelInt64PointerValue(schedule.EndHour)
				entry.StartMinute, entry.EndMinute = types.Int64Value(int64(schedule.StartMinute)), types.Int64Value(int64(schedule.EndMinute))
				var weekdayDiags diag.Diagnostics
				entry.Weekdays, weekdayDiags = types.SetValueFrom(ctx, types.StringType, schedule.Weekdays)
				diags.Append(weekdayDiags...)
			case managementapi.OneTimeAutoscalingSchedule:
				name, settings = schedule.Name, schedule.AutoscalingSettings
				entry.ID, entry.Cadence = types.StringValue(schedule.Id), types.StringValue("ONE_TIME")
				entry.Enabled = types.BoolValue(schedule.Enabled)
				entry.StartAt, entry.EndAt = types.StringValue(schedule.StartAt.Format(time.RFC3339Nano)), types.StringValue(schedule.EndAt.Format(time.RFC3339Nano))
			}
			if _, duplicate := entries[name]; duplicate {
				diags.AddError("Duplicate autoscaling schedule name", fmt.Sprintf("Multiple schedules are named %q. Give schedules unique names before managing them with Terraform.", name))
				continue
			}
			autoscaling := modelAutoscalingModel{
				MinReplica: types.Int64Value(int64(settings.MinReplica)), MaxReplica: types.Int64Value(int64(settings.MaxReplica)),
				ConcurrencyTarget: modelInt64PointerValue(settings.ConcurrencyTarget), AutoscalingWindow: modelInt64PointerValue(settings.AutoscalingWindow),
				ScaleDownDelay: modelInt64PointerValue(settings.ScaleDownDelay), TargetUtilizationPercentage: modelInt64PointerValue(settings.TargetUtilizationPercentage),
				MaxScaleDownRate: modelInt64PointerValue(settings.MaxScaleDownRate), TargetInFlightTokens: modelInt64PointerValue(settings.TargetInFlightTokens),
			}
			var objectDiags diag.Diagnostics
			entry.Autoscaling, objectDiags = types.ObjectValueFrom(ctx, attrTypesOf(modelAutoscalingSchemaAttributes()), autoscaling)
			diags.Append(objectDiags...)
			entries[name] = entry
		}
	}
	result, mapDiags := types.MapValueFrom(ctx, modelScheduleObjectType(), entries)
	diags.Append(mapDiags...)
	return result, timezone, diags
}

func modelBackpressureUpdate(policy types.String) *managementapi.UpdateRequestBackpressureSettings {
	if !modelIsSet(policy) {
		return nil
	}
	var value *managementapi.RequestBackpressurePolicy
	if policy.ValueString() != "" {
		converted := managementapi.RequestBackpressurePolicy(policy.ValueString())
		value = &converted
	}
	return &managementapi.UpdateRequestBackpressureSettings{Policy: managementapi.NewOptional(value)}
}

// modelScheduleUpdate translates a full desired collection into the API's
// partial mutation. Matching on name also adopts schedules created outside
// Terraform, and reuses their IDs instead of creating a duplicate on each apply.
func modelScheduleUpdate(ctx context.Context, planned types.Map, timezone types.String, current *managementapi.EnvironmentAutoscalingSchedules) (*managementapi.UpdateAutoscalingScheduleSettings, diag.Diagnostics) {
	var diags diag.Diagnostics
	update := managementapi.UpdateAutoscalingScheduleSettings{}
	if modelIsSet(timezone) && (timezone.ValueString() != "" || (current != nil && current.Timezone != nil)) {
		var value *string
		if timezone.ValueString() != "" {
			converted := timezone.ValueString()
			value = &converted
		}
		update.Timezone = managementapi.NewOptional(value)
	}
	if modelIsSet(planned) {
		var entries map[string]modelScheduleModel
		diags.Append(planned.ElementsAs(ctx, &entries, false)...)
		currentMap, _, currentDiags := modelSchedulesFromAPI(ctx, current)
		diags.Append(currentDiags...)
		var existing map[string]modelScheduleModel
		diags.Append(currentMap.ElementsAs(ctx, &existing, false)...)
		if diags.HasError() {
			return nil, diags
		}
		upserts := make([]managementapi.UpdateAutoscalingScheduleSettings_Schedules_Item, 0, len(entries))
		for _, name := range modelSortedNames(entries) {
			entry := entries[name]
			var settings modelAutoscalingModel
			diags.Append(entry.Autoscaling.As(ctx, &settings, modelObjectAsOptions)...)
			if diags.HasError() {
				return nil, diags
			}
			overrides := managementapi.AutoscalingScheduleSettingsRequest{
				MinReplica: int(settings.MinReplica.ValueInt64()), MaxReplica: int(settings.MaxReplica.ValueInt64()),
				ConcurrencyTarget: modelIntPointer(settings.ConcurrencyTarget), AutoscalingWindow: modelIntPointer(settings.AutoscalingWindow),
				ScaleDownDelay: modelIntPointer(settings.ScaleDownDelay), TargetUtilizationPercentage: modelIntPointer(settings.TargetUtilizationPercentage),
				MaxScaleDownRate: modelIntPointer(settings.MaxScaleDownRate), TargetInFlightTokens: modelIntPointer(settings.TargetInFlightTokens),
			}
			var id *string
			if previous, found := existing[name]; found {
				value := previous.ID.ValueString()
				id = &value
			}
			var item managementapi.UpdateAutoscalingScheduleSettings_Schedules_Item
			var err error
			if entry.Cadence.ValueString() == "ONE_TIME" {
				start, startErr := time.Parse(time.RFC3339Nano, entry.StartAt.ValueString())
				end, endErr := time.Parse(time.RFC3339Nano, entry.EndAt.ValueString())
				if startErr != nil || endErr != nil {
					diags.AddError("Invalid one-time schedule", fmt.Sprintf("Schedule %q requires RFC 3339 start_at and end_at timestamps.", name))
					continue
				}
				err = item.FromOneTimeAutoscalingScheduleUpsert(managementapi.OneTimeAutoscalingScheduleUpsert{
					Id: id, Name: name, Enabled: entry.Enabled.ValueBool(), StartAt: start, EndAt: end, AutoscalingSettings: overrides,
				})
			} else {
				var weekdays []managementapi.AutoscalingScheduleWeekday
				diags.Append(entry.Weekdays.ElementsAs(ctx, &weekdays, false)...)
				sort.Slice(weekdays, func(i, j int) bool { return weekdays[i] < weekdays[j] })
				err = item.FromAutoscalingScheduleUpsert(managementapi.AutoscalingScheduleUpsert{
					Id: id, Name: name, Cadence: managementapi.AutoscalingScheduleUpsertCadence(entry.Cadence.ValueString()),
					Enabled: entry.Enabled.ValueBool(), Weekdays: weekdays, StartHour: modelIntPointer(entry.StartHour), EndHour: modelIntPointer(entry.EndHour),
					StartMinute: int(entry.StartMinute.ValueInt64()), EndMinute: int(entry.EndMinute.ValueInt64()), AutoscalingSettings: overrides,
				})
			}
			if err != nil {
				diags.AddError("Unable to encode autoscaling schedule", err.Error())
			}
			upserts = append(upserts, item)
		}
		deleted := make([]string, 0)
		for _, name := range modelSortedNames(existing) {
			if _, kept := entries[name]; !kept {
				deleted = append(deleted, existing[name].ID.ValueString())
			}
		}
		update.Schedules, update.DeleteSchedules = &upserts, &deleted
	}
	if !update.Timezone.IsSet() && update.Schedules == nil {
		return nil, diags
	}
	return &update, diags
}

// Schedule names are map keys, so merging preserves planned settings while
// filling newly generated IDs from the mutation response. A missing name is an
// error: storing an unknown ID would leave Terraform with an invalid state.
func modelMergeSchedules(ctx context.Context, planned, fetched types.Map) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics
	if planned.IsUnknown() {
		return fetched, diags
	}
	if planned.IsNull() {
		return planned, diags
	}
	merged := make(map[string]modelScheduleModel)
	for name, value := range planned.Elements() {
		fetchedValue, found := fetched.Elements()[name]
		if !found {
			diags.AddError("Autoscaling schedule missing after update", fmt.Sprintf("Baseten did not return schedule %q. Refresh state before applying again.", name))
			continue
		}
		object, objectDiags := modelMergeSettings(ctx, value.(types.Object), fetchedValue.(types.Object))
		diags.Append(objectDiags...)
		var entry modelScheduleModel
		diags.Append(object.As(ctx, &entry, modelObjectAsOptions)...)
		merged[name] = entry
	}
	result, mapDiags := types.MapValueFrom(ctx, modelScheduleObjectType(), merged)
	diags.Append(mapDiags...)
	return result, diags
}

// The API may normalize timestamp offsets or fractional seconds. Keep the
// state's spelling of the same instant, while still reporting real changes.
func modelPreserveScheduleTimes(ctx context.Context, fetched, prior types.Map) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics
	if !modelIsSet(prior) {
		return fetched, diags
	}
	var entries, previous map[string]modelScheduleModel
	diags.Append(fetched.ElementsAs(ctx, &entries, false)...)
	diags.Append(prior.ElementsAs(ctx, &previous, false)...)
	if diags.HasError() {
		return fetched, diags
	}
	for name, entry := range entries {
		old, found := previous[name]
		if !found || !entry.ID.Equal(old.ID) {
			continue
		}
		preserve := func(current *types.String, prior types.String) {
			if !modelIsSet(*current) || !modelIsSet(prior) {
				return
			}
			currentTime, currentErr := time.Parse(time.RFC3339Nano, current.ValueString())
			priorTime, priorErr := time.Parse(time.RFC3339Nano, prior.ValueString())
			if currentErr == nil && priorErr == nil && currentTime.Equal(priorTime) {
				*current = prior
			}
		}
		preserve(&entry.StartAt, old.StartAt)
		preserve(&entry.EndAt, old.EndAt)
		entries[name] = entry
	}
	result, mapDiags := types.MapValueFrom(ctx, modelScheduleObjectType(), entries)
	diags.Append(mapDiags...)
	return result, diags
}

func modelValidateScheduleSettings(ctx context.Context, entry modelEnvironmentModel, diags *diag.Diagnostics) {
	if modelIsSet(entry.RequestBackpressurePolicy) {
		policy := entry.RequestBackpressurePolicy.ValueString()
		if policy != "" && !managementapi.RequestBackpressurePolicy(policy).Valid() {
			diags.AddError("Invalid request backpressure policy", "Use QUEUE_ON_FULL, REJECT_ON_FULL, or an empty string to clear the policy.")
		}
	}
	if !modelIsSet(entry.AutoscalingSchedules) {
		return
	}
	for name, value := range entry.AutoscalingSchedules.Elements() {
		invalid := func(message string) {
			diags.AddError("Invalid autoscaling schedule", fmt.Sprintf("Schedule %q: %s", name, message))
		}
		if value.IsUnknown() {
			continue
		}
		if value.IsNull() {
			invalid("the schedule must not be null.")
			continue
		}
		var schedule modelScheduleModel
		diags.Append(value.(types.Object).As(ctx, &schedule, modelObjectAsOptions)...)
		if name == "" {
			invalid("the name must not be empty.")
		}
		if modelIsSet(schedule.Autoscaling) {
			var settings modelAutoscalingModel
			diags.Append(schedule.Autoscaling.As(ctx, &settings, modelObjectAsOptions)...)
			if modelIsSet(settings.MinReplica) && modelIsSet(settings.MaxReplica) &&
				(settings.MinReplica.ValueInt64() < 0 || settings.MaxReplica.ValueInt64() < settings.MinReplica.ValueInt64()) {
				invalid("replica bounds must satisfy 0 <= min_replica <= max_replica.")
			}
		}
		if !modelIsSet(schedule.Cadence) {
			continue
		}
		cadence := schedule.Cadence.ValueString()
		if cadence != "DAILY" && cadence != "HOURLY" && cadence != "ONE_TIME" {
			invalid("cadence must be DAILY, HOURLY, or ONE_TIME.")
			continue
		}
		if cadence == "ONE_TIME" {
			if !schedule.StartAt.IsUnknown() && !schedule.EndAt.IsUnknown() {
				start, startErr := time.Parse(time.RFC3339Nano, schedule.StartAt.ValueString())
				end, endErr := time.Parse(time.RFC3339Nano, schedule.EndAt.ValueString())
				if startErr != nil || endErr != nil || !end.After(start) {
					invalid("start_at and end_at must be RFC 3339 timestamps with end_at after start_at.")
				}
			}
			if modelIsSet(schedule.Weekdays) || modelIsSet(schedule.StartHour) || modelIsSet(schedule.EndHour) || modelIsSet(schedule.StartMinute) || modelIsSet(schedule.EndMinute) {
				invalid("ONE_TIME schedules cannot have weekdays or recurring hour/minute fields.")
			}
		} else {
			if modelIsSet(schedule.StartAt) || modelIsSet(schedule.EndAt) {
				invalid("recurring schedules cannot have start_at or end_at.")
			}
			if !schedule.Weekdays.IsUnknown() {
				weekdays := schedule.Weekdays.Elements()
				if len(weekdays) == 0 {
					invalid("recurring schedules require weekdays.")
				}
				for _, weekday := range weekdays {
					if weekday.IsUnknown() {
						continue
					}
					if weekday.IsNull() || !managementapi.AutoscalingScheduleWeekday(weekday.(types.String).ValueString()).Valid() {
						invalid("weekdays must be uppercase weekday names.")
					}
				}
			}
			for _, minute := range []types.Int64{schedule.StartMinute, schedule.EndMinute} {
				if minute.IsNull() || (modelIsSet(minute) && (minute.ValueInt64() < 0 || minute.ValueInt64() > 59)) {
					invalid("recurring schedules require start_minute and end_minute between 0 and 59.")
				}
			}
			if cadence == "DAILY" && (schedule.StartHour.IsNull() || schedule.EndHour.IsNull()) {
				invalid("DAILY schedules require start_hour and end_hour.")
			}
			if cadence == "HOURLY" && !schedule.StartHour.IsUnknown() && !schedule.EndHour.IsUnknown() && schedule.StartHour.IsNull() != schedule.EndHour.IsNull() {
				invalid("HOURLY schedules require both hours or neither hour.")
			}
			for _, hour := range []types.Int64{schedule.StartHour, schedule.EndHour} {
				if modelIsSet(hour) && (hour.ValueInt64() < 0 || hour.ValueInt64() > 23) {
					invalid("hours must be between 0 and 23.")
				}
			}
		}
	}
}
