package provider

import (
	"context"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
)

// Settings attributes are all Optional+Computed, so deleting a line means "leave
// what is there" rather than "reset to default". Baseten has no reset-to-default
// path: every settings field is applied only when non-null, so Optional alone
// would stomp anything configured outside Terraform on the first apply.

// modelEnvironmentModel is one entry of a model's `environments` map. Settings
// objects stay as types.Object rather than Go structs because a newly added
// entry plans them unknown, which a struct pointer cannot represent.
type modelEnvironmentModel struct {
	Autoscaling      types.Object `tfsdk:"autoscaling"`
	Promotion        types.Object `tfsdk:"promotion"`
	CreatedAt        types.String `tfsdk:"created_at"`
	InstanceTypeName types.String `tfsdk:"instance_type_name"`
}

type modelAutoscalingModel struct {
	MinReplica                  types.Int64 `tfsdk:"min_replica"`
	MaxReplica                  types.Int64 `tfsdk:"max_replica"`
	ConcurrencyTarget           types.Int64 `tfsdk:"concurrency_target"`
	AutoscalingWindow           types.Int64 `tfsdk:"autoscaling_window"`
	ScaleDownDelay              types.Int64 `tfsdk:"scale_down_delay"`
	TargetUtilizationPercentage types.Int64 `tfsdk:"target_utilization_percentage"`
	MaxScaleDownRate            types.Int64 `tfsdk:"max_scale_down_rate"`
	TargetInFlightTokens        types.Int64 `tfsdk:"target_in_flight_tokens"`
}

type modelPromotionModel struct {
	RedeployOnPromotion      types.Bool   `tfsdk:"redeploy_on_promotion"`
	PromotionCleanupStrategy types.String `tfsdk:"promotion_cleanup_strategy"`
	RampUpWhilePromoting     types.Bool   `tfsdk:"ramp_up_while_promoting"`
	RampUpDurationSeconds    types.Int64  `tfsdk:"ramp_up_duration_seconds"`
	RollingDeploy            types.Object `tfsdk:"rolling_deploy"`
}

// modelRollingDeployModel collapses the API's `rolling_deploy` boolean and its
// separate `rolling_deploy_config` object into one attribute, so `enabled` is
// the boolean and the rest is the config.
type modelRollingDeployModel struct {
	Enabled                  types.Bool   `tfsdk:"enabled"`
	Strategy                 types.String `tfsdk:"strategy"`
	MaxSurgePercent          types.Int64  `tfsdk:"max_surge_percent"`
	MaxUnavailablePercent    types.Int64  `tfsdk:"max_unavailable_percent"`
	StabilizationTimeSeconds types.Int64  `tfsdk:"stabilization_time_seconds"`
	ReplicaOverheadPercent   types.Int64  `tfsdk:"replica_overhead_percent"`
}

func modelEnvironmentSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"autoscaling": schema.SingleNestedAttribute{
			MarkdownDescription: "Autoscaling settings for the environment. Attributes left out keep their " +
				"current values rather than resetting to Baseten's defaults.",
			Optional:   true,
			Computed:   true,
			Attributes: modelAutoscalingSchemaAttributes(),
		},
		"promotion": schema.SingleNestedAttribute{
			MarkdownDescription: "Promotion settings for the environment. Attributes left out keep their " +
				"current values rather than resetting to Baseten's defaults.",
			Optional:   true,
			Computed:   true,
			Attributes: modelPromotionSchemaAttributes(),
		},
		"created_at": schema.StringAttribute{
			MarkdownDescription: "Time the environment was created, in ISO 8601 format.",
			Computed:            true,
		},
		// Deployment status, replica counts, and in-flight promotions are deliberately
		// absent: they move continuously without anyone asking, so Terraform would report
		// drift it can do nothing about on nearly every plan. Instance type moves too, on
		// any push or promotion that changes it, which is rare enough to stay quiet.
		"instance_type_name": schema.StringAttribute{
			MarkdownDescription: "Display name of the instance type serving the environment. Read-only, " +
				"because Baseten copies the instance type onto a new deployment rather than changing it in place.",
			Computed: true,
		},
	}
}

func modelAutoscalingSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"min_replica": schema.Int64Attribute{
			MarkdownDescription: "Minimum number of replicas.",
			Optional:            true,
			Computed:            true,
		},
		"max_replica": schema.Int64Attribute{
			MarkdownDescription: "Maximum number of replicas.",
			Optional:            true,
			Computed:            true,
		},
		"concurrency_target": schema.Int64Attribute{
			MarkdownDescription: "Number of requests per replica before scaling up.",
			Optional:            true,
			Computed:            true,
		},
		"autoscaling_window": schema.Int64Attribute{
			MarkdownDescription: "Timeframe of traffic considered for autoscaling decisions, in seconds.",
			Optional:            true,
			Computed:            true,
		},
		"scale_down_delay": schema.Int64Attribute{
			MarkdownDescription: "Waiting period before scaling down any active replica, in seconds.",
			Optional:            true,
			Computed:            true,
		},
		"target_utilization_percentage": schema.Int64Attribute{
			MarkdownDescription: "Target utilization percentage for scaling up and down.",
			Optional:            true,
			Computed:            true,
		},
		"max_scale_down_rate": schema.Int64Attribute{
			MarkdownDescription: "Maximum percentage of replicas that can be removed per autoscaling window, " +
				"from 1 to 50. For example, 20 means at most 20% of replicas are removed per window. Baseten " +
				"has no way to clear this once set.",
			Optional: true,
			Computed: true,
		},
		"target_in_flight_tokens": schema.Int64Attribute{
			MarkdownDescription: "Target number of in-flight tokens for autoscaling decisions. Early access " +
				"only. Baseten has no way to clear this once set.",
			Optional: true,
			Computed: true,
		},
	}
}

func modelPromotionSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"redeploy_on_promotion": schema.BoolAttribute{
			MarkdownDescription: "Whether to deploy on all promotions. Enabling this flag allows model code " +
				"to safely handle environment-specific logic. When a deployment is promoted, a new deployment " +
				"is created with a copy of the image.",
			Optional: true,
			Computed: true,
		},
		"promotion_cleanup_strategy": schema.StringAttribute{
			MarkdownDescription: "The cleanup strategy to use after a promotion completes. One of `KEEP`, " +
				"`SCALE_TO_ZERO`, or `DEACTIVATE`.",
			Optional: true,
			Computed: true,
		},
		"ramp_up_while_promoting": schema.BoolAttribute{
			MarkdownDescription: "Whether to ramp up traffic while promoting.",
			Optional:            true,
			Computed:            true,
		},
		"ramp_up_duration_seconds": schema.Int64Attribute{
			MarkdownDescription: "Duration of the ramp up, in seconds.",
			Optional:            true,
			Computed:            true,
		},
		"rolling_deploy": schema.SingleNestedAttribute{
			MarkdownDescription: "Rolling deploy orchestration for promotions to this environment. Setting " +
				"any of the tuning attributes without `enabled = true` stores the values without turning " +
				"rolling deploy on.",
			Optional:   true,
			Computed:   true,
			Attributes: modelRollingDeploySchemaAttributes(),
		},
	}
}

func modelRollingDeploySchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"enabled": schema.BoolAttribute{
			MarkdownDescription: "Whether the environment should rely on rolling deploy orchestration. " +
				"Defaults to false at Baseten, so rolling deploy stays off until this is set.",
			Optional: true,
			Computed: true,
		},
		"strategy": schema.StringAttribute{
			MarkdownDescription: "The rolling deploy strategy to use for promotions. Currently `REPLICA`.",
			Optional:            true,
			Computed:            true,
		},
		"max_surge_percent": schema.Int64Attribute{
			MarkdownDescription: "The maximum surge percentage for rolling deploys.",
			Optional:            true,
			Computed:            true,
		},
		"max_unavailable_percent": schema.Int64Attribute{
			MarkdownDescription: "The maximum unavailable percentage for rolling deploys.",
			Optional:            true,
			Computed:            true,
		},
		"stabilization_time_seconds": schema.Int64Attribute{
			MarkdownDescription: "The stabilization time in seconds for rolling deploys.",
			Optional:            true,
			Computed:            true,
		},
		"replica_overhead_percent": schema.Int64Attribute{
			MarkdownDescription: "The replica overhead percentage for rolling deploys.",
			Optional:            true,
			Computed:            true,
		},
	}
}

// attrTypesOf derives the object attribute types from the schema attributes
// themselves, so the two cannot drift.
func attrTypesOf(attributes map[string]schema.Attribute) map[string]attr.Type {
	attrTypes := make(map[string]attr.Type, len(attributes))
	for name, attribute := range attributes {
		attrTypes[name] = attribute.GetType()
	}
	return attrTypes
}

func modelEnvironmentObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: attrTypesOf(modelEnvironmentSchemaAttributes())}
}

// modelObjectAsOptions treats a null or unknown object as one whose every
// attribute is null, which is what both the request builders and the merge want:
// nothing to send, nothing to prefer.
var modelObjectAsOptions = basetypes.ObjectAsOptions{
	UnhandledNullAsEmpty:    true,
	UnhandledUnknownAsEmpty: true,
}

// modelEnvironmentFromAPI builds an environment entry from a read, leaving the
// settings exactly as Baseten reports them.
func modelEnvironmentFromAPI(ctx context.Context, environment *managementapi.Environment) (modelEnvironmentModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	var entry modelEnvironmentModel

	autoscaling, autoscalingDiags := types.ObjectValueFrom(ctx, attrTypesOf(modelAutoscalingSchemaAttributes()), &modelAutoscalingModel{
		MinReplica:                  types.Int64Value(int64(environment.AutoscalingSettings.MinReplica)),
		MaxReplica:                  types.Int64Value(int64(environment.AutoscalingSettings.MaxReplica)),
		ConcurrencyTarget:           types.Int64Value(int64(environment.AutoscalingSettings.ConcurrencyTarget)),
		AutoscalingWindow:           modelInt64PointerValue(environment.AutoscalingSettings.AutoscalingWindow),
		ScaleDownDelay:              modelInt64PointerValue(environment.AutoscalingSettings.ScaleDownDelay),
		TargetUtilizationPercentage: modelInt64PointerValue(environment.AutoscalingSettings.TargetUtilizationPercentage),
		MaxScaleDownRate:            modelInt64PointerValue(environment.AutoscalingSettings.MaxScaleDownRate),
		TargetInFlightTokens:        modelInt64PointerValue(environment.AutoscalingSettings.TargetInFlightTokens),
	})
	diags.Append(autoscalingDiags...)

	rollingDeployAttrTypes := attrTypesOf(modelRollingDeploySchemaAttributes())
	rollingDeploySettings := modelRollingDeployModel{Enabled: modelBoolPointerValue(environment.PromotionSettings.RollingDeploy)}
	if config := environment.PromotionSettings.RollingDeployConfig; config != nil {
		rollingDeploySettings.MaxSurgePercent = modelInt64PointerValue(config.MaxSurgePercent)
		rollingDeploySettings.MaxUnavailablePercent = modelInt64PointerValue(config.MaxUnavailablePercent)
		rollingDeploySettings.StabilizationTimeSeconds = modelInt64PointerValue(config.StabilizationTimeSeconds)
		rollingDeploySettings.ReplicaOverheadPercent = modelInt64PointerValue(config.ReplicaOverheadPercent)
		if config.RollingDeployStrategy != nil {
			rollingDeploySettings.Strategy = types.StringValue(string(*config.RollingDeployStrategy))
		}
	}
	rollingDeploy, rollingDeployDiags := types.ObjectValueFrom(ctx, rollingDeployAttrTypes, &rollingDeploySettings)
	diags.Append(rollingDeployDiags...)

	promotionSettings := modelPromotionModel{
		RedeployOnPromotion:   modelBoolPointerValue(environment.PromotionSettings.RedeployOnPromotion),
		RampUpWhilePromoting:  modelBoolPointerValue(environment.PromotionSettings.RampUpWhilePromoting),
		RampUpDurationSeconds: modelInt64PointerValue(environment.PromotionSettings.RampUpDurationSeconds),
		RollingDeploy:         rollingDeploy,
	}
	if strategy := environment.PromotionSettings.PromotionCleanupStrategy; strategy != nil {
		promotionSettings.PromotionCleanupStrategy = types.StringValue(string(*strategy))
	}
	promotion, promotionDiags := types.ObjectValueFrom(ctx, attrTypesOf(modelPromotionSchemaAttributes()), &promotionSettings)
	diags.Append(promotionDiags...)

	entry = modelEnvironmentModel{
		Autoscaling:      autoscaling,
		Promotion:        promotion,
		CreatedAt:        types.StringValue(environment.CreatedAt.Format(time.RFC3339)),
		InstanceTypeName: types.StringValue(environment.InstanceType.Name),
	}
	return entry, diags
}

// modelAutoscalingUpdate builds the autoscaling payload, returning nil when the
// configuration asks for nothing. A null attribute is omitted rather than sent
// as null, since Baseten applies fields only when non-null.
func modelAutoscalingUpdate(ctx context.Context, object types.Object) (*managementapi.UpdateAutoscalingSettings, diag.Diagnostics) {
	var settings modelAutoscalingModel
	diags := object.As(ctx, &settings, modelObjectAsOptions)
	if diags.HasError() {
		return nil, diags
	}

	update := managementapi.UpdateAutoscalingSettings{
		MinReplica:                  modelIntPointer(settings.MinReplica),
		MaxReplica:                  modelIntPointer(settings.MaxReplica),
		ConcurrencyTarget:           modelIntPointer(settings.ConcurrencyTarget),
		AutoscalingWindow:           modelIntPointer(settings.AutoscalingWindow),
		ScaleDownDelay:              modelIntPointer(settings.ScaleDownDelay),
		TargetUtilizationPercentage: modelIntPointer(settings.TargetUtilizationPercentage),
		MaxScaleDownRate:            modelIntPointer(settings.MaxScaleDownRate),
		TargetInFlightTokens:        modelIntPointer(settings.TargetInFlightTokens),
	}
	if update == (managementapi.UpdateAutoscalingSettings{}) {
		return nil, diags
	}
	return &update, diags
}

// modelPromotionUpdate builds the promotion payload, splitting the provider's
// single rolling_deploy attribute back into the API's boolean and config.
func modelPromotionUpdate(ctx context.Context, object types.Object) (*managementapi.UpdatePromotionSettings, diag.Diagnostics) {
	var settings modelPromotionModel
	diags := object.As(ctx, &settings, modelObjectAsOptions)
	if diags.HasError() {
		return nil, diags
	}

	update := managementapi.UpdatePromotionSettings{
		RedeployOnPromotion:   modelBoolPointer(settings.RedeployOnPromotion),
		RampUpWhilePromoting:  modelBoolPointer(settings.RampUpWhilePromoting),
		RampUpDurationSeconds: modelIntPointer(settings.RampUpDurationSeconds),
	}
	if modelIsSet(settings.PromotionCleanupStrategy) {
		strategy := managementapi.PromotionCleanupStrategy(settings.PromotionCleanupStrategy.ValueString())
		update.PromotionCleanupStrategy = &strategy
	}

	var rollingDeploy modelRollingDeployModel
	rollingDeployDiags := settings.RollingDeploy.As(ctx, &rollingDeploy, modelObjectAsOptions)
	diags.Append(rollingDeployDiags...)
	if diags.HasError() {
		return nil, diags
	}

	update.RollingDeploy = modelBoolPointer(rollingDeploy.Enabled)
	rollingDeployConfig := managementapi.UpdateRollingDeployConfig{
		MaxSurgePercent:          modelIntPointer(rollingDeploy.MaxSurgePercent),
		MaxUnavailablePercent:    modelIntPointer(rollingDeploy.MaxUnavailablePercent),
		StabilizationTimeSeconds: modelIntPointer(rollingDeploy.StabilizationTimeSeconds),
		ReplicaOverheadPercent:   modelIntPointer(rollingDeploy.ReplicaOverheadPercent),
	}
	if modelIsSet(rollingDeploy.Strategy) {
		strategy := managementapi.RollingDeployStrategy(rollingDeploy.Strategy.ValueString())
		rollingDeployConfig.RollingDeployStrategy = &strategy
	}
	if rollingDeployConfig != (managementapi.UpdateRollingDeployConfig{}) {
		update.RollingDeployConfig = &rollingDeployConfig
	}

	if update.RedeployOnPromotion == nil && update.RampUpWhilePromoting == nil &&
		update.RampUpDurationSeconds == nil && update.PromotionCleanupStrategy == nil &&
		update.RollingDeploy == nil && update.RollingDeployConfig == nil {
		return nil, diags
	}
	return &update, diags
}

// modelMergeSettings prefers configured values over what was just read back.
// Environment settings apply asynchronously, so a read taken right after a write
// can still report the old value, and returning it would fail the apply with an
// inconsistent-result error. Anything the configuration did not set comes from
// the read.
func modelMergeSettings(ctx context.Context, planned, fetched types.Object) (types.Object, diag.Diagnostics) {
	var diags diag.Diagnostics
	if planned.IsNull() || planned.IsUnknown() || fetched.IsNull() || fetched.IsUnknown() {
		return fetched, diags
	}

	plannedAttributes := planned.Attributes()
	merged := make(map[string]attr.Value, len(plannedAttributes))
	for name, fetchedValue := range fetched.Attributes() {
		plannedValue, ok := plannedAttributes[name]
		if !ok {
			merged[name] = fetchedValue
			continue
		}
		plannedObject, plannedIsObject := plannedValue.(types.Object)
		fetchedObject, fetchedIsObject := fetchedValue.(types.Object)
		if plannedIsObject && fetchedIsObject {
			mergedObject, mergeDiags := modelMergeSettings(ctx, plannedObject, fetchedObject)
			diags.Append(mergeDiags...)
			merged[name] = mergedObject
			continue
		}
		if plannedValue.IsNull() || plannedValue.IsUnknown() {
			merged[name] = fetchedValue
			continue
		}
		merged[name] = plannedValue
	}

	mergedObject, objectDiags := types.ObjectValue(fetched.AttributeTypes(ctx), merged)
	diags.Append(objectDiags...)
	return mergedObject, diags
}

func modelIsSet(value attr.Value) bool {
	return !value.IsNull() && !value.IsUnknown()
}

func modelIntPointer(value types.Int64) *int {
	if !modelIsSet(value) {
		return nil
	}
	converted := int(value.ValueInt64())
	return &converted
}

func modelBoolPointer(value types.Bool) *bool {
	if !modelIsSet(value) {
		return nil
	}
	converted := value.ValueBool()
	return &converted
}

func modelInt64PointerValue(value *int) types.Int64 {
	if value == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*value))
}

func modelBoolPointerValue(value *bool) types.Bool {
	if value == nil {
		return types.BoolNull()
	}
	return types.BoolValue(*value)
}
