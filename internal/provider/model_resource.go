package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// modelProductionEnvironmentName is the environment Baseten creates and manages
// itself. It cannot be created or deleted through the API.
const modelProductionEnvironmentName = "production"

// modelDefaultTimeout is how long an apply waits, which is deliberately much
// shorter than how long Baseten allows a deploy to take. Timing out here does
// not cancel anything: the deployment keeps going and settles on its own, so the
// next plan sees the result. That makes firing early cheap, and raising it a
// matter of setting the timeouts block.
//
// It is finite because an unbounded wait is only safe interactively. Terraform
// has no operation timeout of its own and blocks on the provider indefinitely;
// the first interrupt cancels this context, but a CI job timeout instead kills
// Terraform mid-apply, which can lose the state write for a model that was
// already created and leave the state lock held. Only push.wait blocks long
// enough for any of this to matter.
const modelDefaultTimeout = 30 * time.Minute

// The optional interfaces are asserted because the framework discovers them by
// type assertion: a method whose signature drifts still compiles, and the
// behavior silently stops running.
var (
	_ resource.Resource                   = &modelResource{}
	_ resource.ResourceWithConfigure      = &modelResource{}
	_ resource.ResourceWithValidateConfig = &modelResource{}
	_ resource.ResourceWithModifyPlan     = &modelResource{}
)

// modelResource manages a model's environments. The model itself is adopted:
// creating one requires pushing an artifact, which this resource does not do yet.
type modelResource struct {
	client *client.ManagementClient
}

type modelResourceModel struct {
	ID                            types.String   `tfsdk:"id"`
	Name                          types.String   `tfsdk:"name"`
	TeamID                        types.String   `tfsdk:"team_id"`
	TeamName                      types.String   `tfsdk:"team_name"`
	DeletionProtection            types.Bool     `tfsdk:"deletion_protection"`
	EnvironmentDeletionProtection types.Bool     `tfsdk:"environment_deletion_protection"`
	Push                          types.Object   `tfsdk:"push"`
	Environments                  types.Map      `tfsdk:"environments"`
	DeploymentID                  types.String   `tfsdk:"deployment_id"`
	CreatedAt                     types.String   `tfsdk:"created_at"`
	Timeouts                      timeouts.Value `tfsdk:"timeouts"`
}

func (m modelResourceModel) identity() modelIdentity {
	return modelIdentity{ID: m.ID, Name: m.Name, TeamID: m.TeamID, TeamName: m.TeamName}
}

// modelPushEntry unpacks the push attribute, treating null and unknown as
// absent, which is the adopted mode.
func modelPushEntry(ctx context.Context, push types.Object) (modelPushModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	if !modelIsSet(push) {
		return modelPushModel{}, false, diags
	}
	var entry modelPushModel
	diags.Append(push.As(ctx, &entry, modelObjectAsOptions)...)
	return entry, true, diags
}

func newModelResource() resource.Resource {
	return &modelResource{}
}

func (r *modelResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model"
}

func (r *modelResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A Baseten model, in one of two modes.\n\n" +
			"**Adopted**, without `push`: the model has to already exist, and Terraform manages only its " +
			"environment settings. Nothing is ever created or deleted, and destroying the resource forgets " +
			"the model rather than removing it.\n\n" +
			"**Managed**, with `push`: Terraform owns the model's source and lifetime. It creates the model if it does not " +
			"exist, pushes a new deployment whenever the source changes, and can delete the model on destroy " +
			"once `deletion_protection` is off.\n\n" +
			"Adding `push` to a model Terraform already adopted moves it between these modes, which changes " +
			"what an apply and a destroy can do. The plan warns when that happens.\n\n" +
			"`environments` is exhaustive over what Terraform manages, not over what exists. An environment " +
			"the configuration never mentions is left alone, so a model can be partly managed here and partly " +
			"elsewhere.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the model. Set this or `id`. Changing it targets a different " +
					"model, which replaces this resource, because Baseten cannot rename a model.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"id": schema.StringAttribute{
				MarkdownDescription: "Unique identifier of the model. Set this or `name`.",
				Optional:            true,
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"team_id": schema.StringAttribute{
				MarkdownDescription: "ID of the team owning the model. Conflicts with `team_name`. Set either " +
					"one to resolve `name` within a single team, since model names are unique only within a team.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"team_name": schema.StringAttribute{
				MarkdownDescription: "Name of the team owning the model, resolved to an ID. Conflicts with " +
					"`team_id`.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"deletion_protection": schema.BoolAttribute{
				MarkdownDescription: "Whether Terraform is prevented from deleting the model. Defaults to " +
					"true, because deleting a model also deletes every deployment and environment under it and " +
					"cannot be undone. Only meaningful alongside `push`: an adopted model is never deleted. " +
					"Setting this to false also allows the destroy to delete environments, regardless of " +
					"`environment_deletion_protection`, since they go with the model.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"environment_deletion_protection": schema.BoolAttribute{
				MarkdownDescription: "Whether Terraform is prevented from deleting environments. Defaults to " +
					"true, because deleting an environment is traffic-affecting: Baseten scales the deployment " +
					"serving it down to zero replicas. While true, removing an entry from `environments` fails " +
					"the plan, and destroying this resource leaves every environment in place. Set it to false " +
					"in the same apply that removes an entry to allow the deletion.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "Time the model was created, in ISO 8601 format.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"environments": schema.MapNestedAttribute{
				MarkdownDescription: "Environments to manage, keyed by environment name. An entry that does " +
					"not exist yet is created, except `production`, which Baseten creates with the model. " +
					"Settings left out of an entry keep their current values.",
				Optional:     true,
				NestedObject: schema.NestedAttributeObject{Attributes: modelEnvironmentSchemaAttributes()},
			},
			"push": schema.SingleNestedAttribute{
				MarkdownDescription: "Code to deploy, which puts this resource in managed mode. Leave it out " +
					"to adopt an existing model without deploying anything. Adding it lets Terraform create the " +
					"model, push a deployment whenever the source changes, and delete the model on destroy.",
				Optional:   true,
				Attributes: modelPushSchemaAttributes(),
			},
			"deployment_id": schema.StringAttribute{
				MarkdownDescription: "Unique identifier of the deployment Terraform last pushed. Null in " +
					"adopted mode, and never reflects a deployment created outside Terraform.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"timeouts": timeouts.Attributes(ctx, timeouts.Opts{Create: true, Update: true}),
		},
	}
}

func (r *modelResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config modelResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	validateModelIdentity(config.identity(), &resp.Diagnostics)

	push, hasPush, diags := modelPushEntry(ctx, config.Push)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !hasPush {
		if config.DeletionProtection.Equal(types.BoolValue(false)) {
			resp.Diagnostics.AddAttributeWarning(
				path.Root("deletion_protection"),
				"Deletion protection does nothing without push",
				"This resource adopts a model rather than creating it, so destroying it forgets the model "+
					"instead of deleting it and there is nothing for deletion_protection to guard. Use "+
					"environment_deletion_protection to allow environments to be deleted.",
			)
		}
		return
	}

	switch {
	case modelIsSet(push.ConfigDir) && modelIsSet(push.Config):
		resp.Diagnostics.AddError(
			"Conflicting model configuration",
			"Set push.config_dir or push.config, not both.",
		)
	case push.ConfigDir.IsNull() && push.Config.IsNull():
		resp.Diagnostics.AddError(
			"Missing model configuration",
			"Set push.config_dir or push.config so the provider knows what to deploy.",
		)
	}
}

// ModifyPlan reports the problems that are only visible with the configuration
// and prior state side by side, so they surface during plan rather than apply.
func (r *modelResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}

	var config modelResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	configEnvironments, diags := modelEnvironmentEntries(ctx, config.Environments)
	resp.Diagnostics.Append(diags...)

	var stateEnvironments map[string]modelEnvironmentModel
	if !req.State.Raw.IsNull() {
		var state modelResourceModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		stateEnvironments, diags = modelEnvironmentEntries(ctx, state.Environments)
		resp.Diagnostics.Append(diags...)

		var plan modelResourceModel
		resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
		if resp.Diagnostics.HasError() {
			return
		}
		for _, name := range modelSortedNames(stateEnvironments) {
			if _, kept := configEnvironments[name]; kept {
				continue
			}
			// Baseten refuses to delete production, so removing it only stops
			// Terraform managing it. Reporting it as a protected deletion would
			// send the user to turn off a flag that changes nothing.
			if name == modelProductionEnvironmentName {
				resp.Diagnostics.AddAttributeWarning(
					path.Root("environments"),
					"Production environment cannot be deleted",
					"Removing production from environments stops Terraform managing its settings. Baseten does "+
						"not allow deleting the production environment, so it stays as it is.",
				)
				continue
			}
			if plan.EnvironmentDeletionProtection.ValueBool() {
				resp.Diagnostics.AddAttributeError(
					path.Root("environments"),
					"Environment is protected from deletion",
					fmt.Sprintf("Removing %q from environments would delete it, which scales the deployment "+
						"serving it down to zero replicas. Set environment_deletion_protection = false to allow "+
						"this, or run terraform state rm to stop managing the environment without deleting it.", name),
				)
			}
		}
	}
	if resp.Diagnostics.HasError() {
		return
	}

	for _, name := range modelSortedNames(configEnvironments) {
		configEntry := configEnvironments[name]
		stateEntry, hasState := stateEnvironments[name]
		if hasState {
			modelWarnDroppedSettings(ctx, name, "autoscaling", stateEntry.Autoscaling, configEntry.Autoscaling, &resp.Diagnostics)
			modelWarnDroppedSettings(ctx, name, "promotion", stateEntry.Promotion, configEntry.Promotion, &resp.Diagnostics)
		}
		modelWarnRollingDeployDisabled(ctx, name, configEntry, stateEntry, hasState, &resp.Diagnostics)
	}

	r.modifyPushPlan(ctx, req, resp)
}

// modifyPushPlan settles the source hash so the plan shows whether a push is
// coming, says why, and announces a move between adopted and managed mode, which
// changes what an apply and a destroy are able to do.
func (r *modelResource) modifyPushPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	var config modelResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	configPush, hasPush, diags := modelPushEntry(ctx, config.Push)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	var state modelResourceModel
	var statePush modelPushModel
	hadPush := false
	if !req.State.Raw.IsNull() {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		statePush, hadPush, diags = modelPushEntry(ctx, state.Push)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	switch {
	case hasPush && !hadPush && !req.State.Raw.IsNull():
		resp.Diagnostics.AddAttributeWarning(
			path.Root("push"),
			"This model is becoming Terraform-managed",
			"Adding push moves the model from adopted to managed: from now on Terraform pushes a new "+
				"deployment whenever the source changes, and a destroy can delete the model once "+
				"deletion_protection is false. Nothing is pushed by this change alone, because the source on "+
				"disk is taken as the starting point.",
		)
	case !hasPush && hadPush:
		resp.Diagnostics.AddAttributeWarning(
			path.Root("push"),
			"This model is no longer Terraform-managed",
			"Removing push moves the model back to adopted. Terraform stops tracking the source and forgets "+
				"the recorded hash. Nothing is pushed and nothing is deleted, and the deployment already "+
				"running is left alone.",
		)
		// deployment_id is Computed, so Terraform carries the prior value into the
		// plan, but adopted mode reports no deployment and the apply nulls it. The
		// plan has to say so, or the apply returns a value the plan did not promise.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("deployment_id"), types.StringNull())...)
	}
	if !hasPush {
		return
	}

	// Settle the hash the plan will compare against. A configured source_hash is
	// taken verbatim, which is the point of setting it: the source is not read.
	// Otherwise walk the source, unless something it depends on is still unknown.
	plannedPush := configPush
	changeSummary := ""
	var source *modelPushSource
	switch {
	case modelIsSet(configPush.SourceHash):
	case configPush.ConfigDir.IsUnknown() || configPush.Config.IsUnknown():
		// source_hash is set unknown explicitly. It is Optional+Computed, so
		// Terraform has already backfilled the prior value into the plan, and
		// leaving that would read as "source unchanged" and skip the push.
		plannedPush.SourceHash = types.StringUnknown()
	default:
		// Hashing uses the source as written, before model_name is filled in, so
		// it never depends on resolving the model.
		resolved, sourceDiags := modelPushResolveSource(ctx, configPush, "")
		resp.Diagnostics.Append(sourceDiags...)
		if resp.Diagnostics.HasError() {
			return
		}
		source = resolved
		defer source.Close()

		hashes, err := source.Hashes(ctx)
		if err != nil {
			resp.Diagnostics.AddAttributeError(path.Root("push"), "Unable to hash the model source", err.Error())
			return
		}
		plannedPush.SourceHash = types.StringValue(hashes.OverallHash())

		if prior, found := modelPushReadHashes(ctx, req.Private); found {
			changeSummary = hashes.ChangeSummary(prior)
		}
		modelPushWriteHashes(ctx, resp.Private, hashes)
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, modelPushPath("source_hash"), plannedPush.SourceHash)...)

	// Report what the apply will do with it.
	switch {
	// Creating the resource. Whether this pushes depends on whether the model
	// already exists, which only the apply can resolve: an absent model is
	// created by pushing, and an existing one is taken over as it stands.
	// deployment_id is unknown for a create either way.
	case req.State.Raw.IsNull():
		resp.Diagnostics.AddAttributeWarning(
			path.Root("push"),
			"Whether this pushes depends on whether the model exists",
			"If no model matches, Terraform creates it by pushing the configured source. If one already "+
				"matches, Terraform takes it over and records the source hash without pushing, leaving the "+
				"running deployment alone.",
		)

	// State exists with no recorded baseline, so the model is being taken over
	// rather than updated. The hash describes the local source, which Baseten
	// cannot confirm matches what is deployed.
	case !hadPush || !modelIsSet(statePush.SourceHash):
		resp.Diagnostics.AddAttributeWarning(
			path.Root("push"),
			"Taking over the model's source without pushing",
			"Terraform recorded the hash of the local source as its starting point and will not push. "+
				"Baseten does not report what a deployment was built from, so confirm the source matches what "+
				"is deployed. Editing the source pushes on the next apply.",
		)

	case modelPushDeploys(plannedPush, statePush):
		detail := "Applying this pushes a new deployment."
		if changeSummary != "" {
			detail = fmt.Sprintf("Applying this pushes a new deployment: %s.", changeSummary)
		}
		resp.Diagnostics.AddAttributeWarning(path.Root("push"), "A new deployment will be pushed", detail)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("deployment_id"), types.StringUnknown())...)

		// Have Baseten check the configuration now, so a bad one fails the plan
		// rather than the apply. Only on a plan that will actually push: doing it
		// on every plan would put a network call in front of every refresh to
		// validate a configuration that is not going anywhere.
		if source != nil {
			resp.Diagnostics.Append(source.Validate(ctx, r.client, state.ID.ValueString(), state.TeamID.ValueString())...)
		}

	default:
		if deferred := modelPushDeferredChanges(plannedPush, statePush); len(deferred) > 0 {
			resp.Diagnostics.AddAttributeWarning(
				path.Root("push"),
				"These changes apply to the next push",
				fmt.Sprintf("%s changed, which does not deploy anything by itself and leaves the deployment "+
					"already running as it is. Edit the model source, or add a push.triggers entry, to deploy "+
					"with the new values.", strings.Join(deferred, ", ")),
			)
		}
	}
}

func (r *modelResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	managementClient, ok := req.ProviderData.(*client.ManagementClient)
	if !ok {
		resp.Diagnostics.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *client.ManagementClient, got %T.", req.ProviderData),
		)
		return
	}
	r.client = managementClient
}

func (r *modelResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan modelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	push, hasPush, diags := modelPushEntry(ctx, plan.Push)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if hasPush {
		push, diags = r.settlePushSourceHash(ctx, &plan, push)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
	}

	timeout, diags := plan.Timeouts.Create(ctx, modelDefaultTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	model, teamID, found, diags := findModel(ctx, r.client, plan.identity())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Nothing has been deployed by Terraform yet, and deployment_id is Computed,
	// so it arrives unknown and has to be settled here whether or not this
	// creates a deployment. Leaving it unknown fails the apply.
	plan.DeploymentID = types.StringNull()

	switch {
	case !found && !hasPush:
		resp.Diagnostics.Append(modelNotFoundError(plan))
		return
	case !found:
		// The only way to create a model is to push one. The push also creates
		// its target environment at Baseten's defaults, so environment settings
		// have to be applied after it rather than before.
		result, pushDiags := r.pushModel(ctx, push, "", teamID, plan.Name.ValueString())
		resp.Diagnostics.Append(pushDiags...)
		if resp.Diagnostics.HasError() {
			return
		}
		model = result.Model
		plan.DeploymentID = types.StringValue(result.Deployment.Id)
	}
	// Anything else takes over a model that already exists, pushing nothing, so
	// whatever is already deployed keeps serving. That covers both adopted mode
	// and a managed resource whose model was created elsewhere, which records the
	// planned source hash as its starting point.
	applyModelToModel(&plan, teamID, model)

	// Nothing was managed before, so there is nothing to delete.
	environments, diags := r.reconcileEnvironments(
		ctx, model.Id, plan.Environments, types.MapNull(modelEnvironmentObjectType()), false)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.Environments = environments

	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *modelResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state modelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	model, err := r.client.API().GetModelsModelId(ctx, state.ID.ValueString())
	if err != nil {
		if modelIsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read Baseten model", err.Error())
		return
	}
	applyModelToModel(&state, state.TeamID.ValueString(), model)

	if state.Environments.IsNull() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	stateEnvironments, diags := modelEnvironmentEntries(ctx, state.Environments)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	existing, diags := r.fetchEnvironments(ctx, model.Id)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Settings are read back as Baseten reports them, so a change made elsewhere
	// shows as drift. An environment that no longer exists drops out of state, and
	// the next plan re-creates it.
	entries := make(map[string]modelEnvironmentModel, len(stateEnvironments))
	for name := range stateEnvironments {
		environment, found := existing[name]
		if !found {
			continue
		}
		entry, entryDiags := modelEnvironmentFromAPI(ctx, environment)
		resp.Diagnostics.Append(entryDiags...)
		entries[name] = entry
	}
	if resp.Diagnostics.HasError() {
		return
	}

	environments, diags := types.MapValueFrom(ctx, modelEnvironmentObjectType(), entries)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state.Environments = environments

	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *modelResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state modelResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	timeout, diags := plan.Timeouts.Update(ctx, modelDefaultTimeout)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Model identity and team force replacement, so they are already resolved.
	// Environment settings go first so that a push landing in an environment
	// rolls out under the settings this apply asked for.
	environments, diags := r.reconcileEnvironments(
		ctx,
		state.ID.ValueString(),
		plan.Environments,
		state.Environments,
		!plan.EnvironmentDeletionProtection.ValueBool(),
	)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.Environments = environments

	pushed, pushDiags := r.updatePush(ctx, &plan, state)
	resp.Diagnostics.Append(pushDiags...)
	if resp.Diagnostics.HasError() && !pushed {
		// A push that never reached Baseten records prior state plus the
		// environment writes that did land, rather than the plan. Recording the
		// plan would store the source hash of a push that never happened, and the
		// next plan would read the source as unchanged and never retry it. Nothing
		// else can recover from that, since Baseten does not report what a
		// deployment was built from, whereas an environment setting is re-read on
		// every refresh.
		resp.State = req.State
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("environments"), environments)...)
		// ModifyPlan recorded the new inventory in private state. Drop it, so the
		// retry describes its changes against the source that was last pushed
		// rather than against one that never was. An empty value removes the key.
		_ = resp.Private.SetKey(ctx, modelPushHashesKey, nil)
		return
	}

	// Either the update succeeded, or the deployment was created and only failed
	// to come up. Both leave the pushed source deployed, so state records it and
	// the next apply does not push it again.
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// updatePush pushes when something that deploys changed, which is the source's
// identity or the explicit triggers. Everything else in `push` is an argument the
// next push will use, so it lands in state and does nothing else.
//
// The reported bool is whether a deployment was created, which decides what a
// failed update records. It is not the same as succeeding: a push that Baseten
// accepted and then failed to bring up reports true along with the error.
func (r *modelResource) updatePush(
	ctx context.Context,
	plan *modelResourceModel,
	state modelResourceModel,
) (bool, diag.Diagnostics) {
	push, hasPush, diags := modelPushEntry(ctx, plan.Push)
	if diags.HasError() {
		return false, diags
	}
	statePush, hadPush, stateDiags := modelPushEntry(ctx, state.Push)
	diags.Append(stateDiags...)
	if diags.HasError() {
		return false, diags
	}

	// Removing push stops managing the artifact. It is not a delete and not a
	// final push, so the deployment already running is left alone.
	if !hasPush {
		plan.DeploymentID = types.StringNull()
		return false, diags
	}
	push, settleDiags := r.settlePushSourceHash(ctx, plan, push)
	diags.Append(settleDiags...)
	if diags.HasError() {
		return false, diags
	}

	// No prior hash means the model is being taken over, so record what the plan
	// computed and leave whatever is deployed serving.
	if !hadPush || !modelIsSet(statePush.SourceHash) {
		return false, diags
	}
	if !modelPushDeploys(push, statePush) {
		return false, diags
	}

	result, pushDiags := r.pushModel(ctx, push, state.ID.ValueString(), "", state.Name.ValueString())
	diags.Append(pushDiags...)
	// A result means Baseten created the deployment. Waiting for it to settle can
	// still fail, by timing out or by the deployment failing to build, and neither
	// un-pushes it: the deployment exists, and reporting otherwise would have the
	// next apply push the same source again alongside it.
	if result == nil {
		return false, diags
	}
	plan.DeploymentID = types.StringValue(result.Deployment.Id)
	return true, diags
}

// Delete forgets the model, which this resource never created. Environments are
// deleted only where protection is off, so the default destroy touches nothing.
func (r *modelResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state modelResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	_, hasPush, diags := modelPushEntry(ctx, state.Push)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Only a managed model can be deleted; an adopted one is forgotten from state.
	// Deleting it cascades to every deployment and environment under it, so
	// environment_deletion_protection has nothing left to guard and is not
	// consulted, matching the dashboard, which does not ask about environments
	// when deleting a model.
	if hasPush && !state.DeletionProtection.ValueBool() {
		if _, err := r.client.API().DeleteModels(ctx, state.ID.ValueString()); err != nil && !modelIsNotFound(err) {
			resp.Diagnostics.AddError("Unable to delete the Baseten model", err.Error())
		}
		return
	}

	if state.EnvironmentDeletionProtection.ValueBool() || state.Environments.IsNull() {
		return
	}

	stateEnvironments, diags := modelEnvironmentEntries(ctx, state.Environments)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	for _, name := range modelSortedNames(stateEnvironments) {
		if name == modelProductionEnvironmentName {
			resp.Diagnostics.AddWarning(
				"Production environment was not deleted",
				"Baseten does not allow deleting the production environment, so it stays as it is.",
			)
			continue
		}
		if _, err := r.client.API().DeleteModelsEnvironments(ctx, state.ID.ValueString(), name); err != nil {
			if modelIsNotFound(err) {
				continue
			}
			resp.Diagnostics.AddError(fmt.Sprintf("Unable to delete Baseten environment %q", name), err.Error())
			return
		}
	}
}

// settlePushSourceHash gives the plan's push a concrete source hash, computing
// one when the plan could not. ModifyPlan leaves it unknown if part of an inline
// configuration was unresolved then, and state cannot hold an unknown, so the
// apply has to finish the job or Terraform rejects the result.
func (r *modelResource) settlePushSourceHash(
	ctx context.Context,
	plan *modelResourceModel,
	push modelPushModel,
) (modelPushModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	if modelIsSet(push.SourceHash) {
		return push, diags
	}

	source, sourceDiags := modelPushResolveSource(ctx, push, "")
	diags.Append(sourceDiags...)
	if diags.HasError() {
		return push, diags
	}
	defer source.Close()

	hashes, err := source.Hashes(ctx)
	if err != nil {
		diags.AddAttributeError(path.Root("push"), "Unable to hash the model source", err.Error())
		return push, diags
	}
	push.SourceHash = types.StringValue(hashes.OverallHash())

	object, objectDiags := types.ObjectValueFrom(ctx, modelPushObjectType().AttrTypes, &push)
	diags.Append(objectDiags...)
	if diags.HasError() {
		return push, diags
	}
	plan.Push = object
	return push, diags
}

// pushModel resolves the configured source and pushes it. modelID is empty to
// create the model, in which case modelName has to be known, since a model
// cannot be created without a name.
func (r *modelResource) pushModel(
	ctx context.Context,
	push modelPushModel,
	modelID, teamID, modelName string,
) (*client.PushModelResult, diag.Diagnostics) {
	var diags diag.Diagnostics
	if modelID == "" && modelName == "" {
		diags.AddAttributeError(
			path.Root("id"),
			"Cannot create a model by ID",
			"No model has this ID, and creating one requires a name. Set name instead of id to have "+
				"Terraform create the model.",
		)
		return nil, diags
	}

	source, sourceDiags := modelPushResolveSource(ctx, push, modelName)
	diags.Append(sourceDiags...)
	if diags.HasError() {
		return nil, diags
	}
	defer source.Close()

	result, pushDiags := source.Push(ctx, r.client, modelID, teamID, push.Wait.ValueBool())
	diags.Append(pushDiags...)
	return result, diags
}

// modelNotFoundError reports a model this resource cannot bring into being,
// which is any missing model in adopted mode.
func modelNotFoundError(plan modelResourceModel) diag.Diagnostic {
	if modelIsSet(plan.ID) {
		return diag.NewAttributeErrorDiagnostic(
			path.Root("id"),
			"Model not found",
			fmt.Sprintf("No model has ID %q. Without a push block this resource adopts an existing model "+
				"and cannot create one, because creating a model means deploying code to it. Add push to "+
				"have Terraform create it.", plan.ID.ValueString()),
		)
	}
	return diag.NewAttributeErrorDiagnostic(
		path.Root("name"),
		"Model not found",
		fmt.Sprintf("No model is named %q. Without a push block this resource adopts an existing model "+
			"and cannot create one, because creating a model means deploying code to it. Add push to have "+
			"Terraform create it.", plan.Name.ValueString()),
	)
}

// reconcileEnvironments upserts every configured environment and deletes the ones
// dropped from the configuration, returning what to store in state.
func (r *modelResource) reconcileEnvironments(
	ctx context.Context,
	modelID string,
	planned, prior types.Map,
	deleteRemoved bool,
) (types.Map, diag.Diagnostics) {
	environmentType := modelEnvironmentObjectType()

	plannedEntries, diags := modelEnvironmentEntries(ctx, planned)
	if diags.HasError() {
		return types.MapNull(environmentType), diags
	}
	priorEntries, priorDiags := modelEnvironmentEntries(ctx, prior)
	diags.Append(priorDiags...)
	if diags.HasError() {
		return types.MapNull(environmentType), diags
	}

	existing, existingDiags := r.fetchEnvironments(ctx, modelID)
	diags.Append(existingDiags...)
	if diags.HasError() {
		return types.MapNull(environmentType), diags
	}

	entries := make(map[string]modelEnvironmentModel, len(plannedEntries))
	for _, name := range modelSortedNames(plannedEntries) {
		plannedEntry := plannedEntries[name]
		autoscaling, autoscalingDiags := modelAutoscalingUpdate(ctx, plannedEntry.Autoscaling)
		diags.Append(autoscalingDiags...)
		promotion, promotionDiags := modelPromotionUpdate(ctx, plannedEntry.Promotion)
		diags.Append(promotionDiags...)
		if diags.HasError() {
			return types.MapNull(environmentType), diags
		}

		environment, found := existing[name]
		switch {
		case !found:
			created, err := r.client.API().PostModelsEnvironments(ctx, modelID, managementapi.CreateEnvironmentRequest{
				Name:                name,
				AutoscalingSettings: autoscaling,
				PromotionSettings:   promotion,
			})
			if err != nil {
				diags.AddError(fmt.Sprintf("Unable to create Baseten environment %q", name), err.Error())
				return types.MapNull(environmentType), diags
			}
			environment = created
		case autoscaling != nil || promotion != nil:
			if promotion != nil && environment.InProgressPromotion != nil {
				diags.AddWarning(
					fmt.Sprintf("Promotion in progress on environment %q", name),
					"Promotion settings are being changed while a promotion is in flight, so the promotion "+
						"already running may not observe them.",
				)
			}
			response, err := r.client.API().PatchModelsEnvironments(ctx, modelID, name, managementapi.UpdateEnvironmentRequest{
				AutoscalingSettings: autoscaling,
				PromotionSettings:   promotion,
			})
			if err != nil {
				diags.AddError(fmt.Sprintf("Unable to update Baseten environment %q", name), err.Error())
				return types.MapNull(environmentType), diags
			}
			if response.Status == managementapi.UpdateAutoscalingSettingsStatus_QUEUED {
				diags.AddWarning(
					fmt.Sprintf("Settings queued for environment %q", name),
					response.Message,
				)
			}
			// The PATCH response carries only a status, and settings apply
			// asynchronously, so re-read to pick up everything not configured.
			refreshed, err := r.client.API().GetModelsEnvironmentsEnvName(ctx, modelID, name)
			if err != nil {
				diags.AddError(fmt.Sprintf("Unable to read Baseten environment %q", name), err.Error())
				return types.MapNull(environmentType), diags
			}
			environment = refreshed
		}

		fetchedEntry, fetchedDiags := modelEnvironmentFromAPI(ctx, environment)
		diags.Append(fetchedDiags...)
		if diags.HasError() {
			return types.MapNull(environmentType), diags
		}

		// The read-only attributes follow the same rule as the settings below: the
		// read fills them in for an environment the plan knew nothing about, and
		// the plan's own value stands for one it did. An instance type that moved
		// since the last refresh is drift, and reporting it here instead would
		// fail the apply for disagreeing with the plan.
		if !plannedEntry.CreatedAt.IsUnknown() {
			fetchedEntry.CreatedAt = plannedEntry.CreatedAt
		}
		if !plannedEntry.InstanceTypeName.IsUnknown() {
			fetchedEntry.InstanceTypeName = plannedEntry.InstanceTypeName
		}

		mergedAutoscaling, mergeDiags := modelMergeSettings(ctx, plannedEntry.Autoscaling, fetchedEntry.Autoscaling)
		diags.Append(mergeDiags...)
		mergedPromotion, mergeDiags := modelMergeSettings(ctx, plannedEntry.Promotion, fetchedEntry.Promotion)
		diags.Append(mergeDiags...)
		if diags.HasError() {
			return types.MapNull(environmentType), diags
		}
		fetchedEntry.Autoscaling = mergedAutoscaling
		fetchedEntry.Promotion = mergedPromotion
		entries[name] = fetchedEntry
	}

	if deleteRemoved {
		for _, name := range modelSortedNames(priorEntries) {
			if _, kept := plannedEntries[name]; kept {
				continue
			}
			// Baseten rejects deleting production, so removing it from the
			// configuration just stops managing it. ModifyPlan already said so.
			if name == modelProductionEnvironmentName {
				continue
			}
			if _, err := r.client.API().DeleteModelsEnvironments(ctx, modelID, name); err != nil {
				if modelIsNotFound(err) {
					continue
				}
				diags.AddError(fmt.Sprintf("Unable to delete Baseten environment %q", name), err.Error())
				return types.MapNull(environmentType), diags
			}
		}
	}

	if planned.IsNull() {
		return types.MapNull(environmentType), diags
	}
	environments, environmentsDiags := types.MapValueFrom(ctx, environmentType, entries)
	diags.Append(environmentsDiags...)
	return environments, diags
}

func (r *modelResource) fetchEnvironments(ctx context.Context, modelID string) (map[string]*managementapi.Environment, diag.Diagnostics) {
	var diags diag.Diagnostics
	environments, err := r.client.API().GetModelsEnvironments(ctx, modelID)
	if err != nil {
		diags.AddError("Unable to read Baseten environments", err.Error())
		return nil, diags
	}
	byName := make(map[string]*managementapi.Environment, len(environments.Environments))
	for i := range environments.Environments {
		byName[environments.Environments[i].Name] = &environments.Environments[i]
	}
	return byName, diags
}

// modelEnvironmentEntries unpacks the environments map, treating null and unknown
// as no entries.
func modelEnvironmentEntries(ctx context.Context, environments types.Map) (map[string]modelEnvironmentModel, diag.Diagnostics) {
	if environments.IsNull() || environments.IsUnknown() {
		return nil, nil
	}
	entries := make(map[string]modelEnvironmentModel, len(environments.Elements()))
	diags := environments.ElementsAs(ctx, &entries, true)
	return entries, diags
}

func modelSortedNames(entries map[string]modelEnvironmentModel) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// modelWarnDroppedSettings warns about a setting present in state that the
// configuration stopped mentioning. Optional+Computed shows no diff for it, and
// Baseten has no way to reset a setting to its default, so the value stays.
func modelWarnDroppedSettings(
	ctx context.Context,
	environmentName, attributeName string,
	state, config types.Object,
	diags *diag.Diagnostics,
) {
	if !modelIsSet(state) || config.IsUnknown() {
		return
	}
	for name, stateValue := range state.Attributes() {
		configValue, ok := config.Attributes()[name]
		if config.IsNull() {
			configValue = nil
			ok = false
		}
		if stateObject, isObject := stateValue.(types.Object); isObject {
			configObject, configIsObject := types.ObjectNull(stateObject.AttributeTypes(ctx)), true
			if ok {
				configObject, configIsObject = configValue.(types.Object)
			}
			if configIsObject {
				modelWarnDroppedSettings(ctx, environmentName, attributeName+"."+name, stateObject, configObject, diags)
			}
			continue
		}
		if !modelIsSet(stateValue) {
			continue
		}
		if ok && modelIsSet(configValue) {
			continue
		}
		diags.AddWarning(
			fmt.Sprintf("Setting no longer configured for environment %q", environmentName),
			fmt.Sprintf("%s.%s is set at Baseten but no longer in the configuration, so it keeps its current "+
				"value. Baseten has no way to reset a setting to its default. Set it explicitly to change it.",
				attributeName, name),
		)
	}
}

// modelWarnRollingDeployDisabled warns when rolling deploy is tuned without being
// turned on, which Baseten accepts and ignores.
func modelWarnRollingDeployDisabled(
	ctx context.Context,
	environmentName string,
	config, state modelEnvironmentModel,
	hasState bool,
	diags *diag.Diagnostics,
) {
	var configPromotion modelPromotionModel
	if diags.Append(config.Promotion.As(ctx, &configPromotion, modelObjectAsOptions)...); diags.HasError() {
		return
	}
	var configRollingDeploy modelRollingDeployModel
	if diags.Append(configPromotion.RollingDeploy.As(ctx, &configRollingDeploy, modelObjectAsOptions)...); diags.HasError() {
		return
	}

	tuned := modelIsSet(configRollingDeploy.Strategy) ||
		modelIsSet(configRollingDeploy.MaxSurgePercent) ||
		modelIsSet(configRollingDeploy.MaxUnavailablePercent) ||
		modelIsSet(configRollingDeploy.StabilizationTimeSeconds) ||
		modelIsSet(configRollingDeploy.ReplicaOverheadPercent)
	if !tuned {
		return
	}

	// An omitted enabled keeps whatever Baseten already has, which prior state holds.
	enabled := configRollingDeploy.Enabled
	if !modelIsSet(enabled) && hasState {
		var statePromotion modelPromotionModel
		if diags.Append(state.Promotion.As(ctx, &statePromotion, modelObjectAsOptions)...); diags.HasError() {
			return
		}
		var stateRollingDeploy modelRollingDeployModel
		if diags.Append(statePromotion.RollingDeploy.As(ctx, &stateRollingDeploy, modelObjectAsOptions)...); diags.HasError() {
			return
		}
		enabled = stateRollingDeploy.Enabled
	}
	if enabled.ValueBool() {
		return
	}

	diags.AddWarning(
		fmt.Sprintf("Rolling deploy is not enabled for environment %q", environmentName),
		"Rolling deploy settings are configured while rolling deploy is off, so Baseten stores them without "+
			"using them. Set promotion.rolling_deploy.enabled = true to turn it on.",
	)
}

func applyModelToModel(model *modelResourceModel, teamID string, apiModel *managementapi.Model) {
	model.ID = types.StringValue(apiModel.Id)
	model.Name = types.StringValue(apiModel.Name)
	model.TeamName = types.StringValue(apiModel.TeamName)
	model.CreatedAt = types.StringValue(apiModel.CreatedAt.Format(time.RFC3339))
	if teamID == "" {
		model.TeamID = types.StringNull()
	} else {
		model.TeamID = types.StringValue(teamID)
	}
}
