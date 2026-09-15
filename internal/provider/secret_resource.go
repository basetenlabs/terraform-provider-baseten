package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// secretResource manages a secret.
type secretResource struct {
	client *client.ManagementClient
}

type secretResourceModel struct {
	Name               types.String `tfsdk:"name"`
	TeamID             types.String `tfsdk:"team_id"`
	TeamName           types.String `tfsdk:"team_name"`
	ValueWO            types.String `tfsdk:"value_wo"`
	ValueWOVersion     types.Int64  `tfsdk:"value_wo_version"`
	DeletionProtection types.Bool   `tfsdk:"deletion_protection"`
	ID                 types.String `tfsdk:"id"`
	CreatedAt          types.String `tfsdk:"created_at"`
}

var (
	_ resource.Resource                   = &secretResource{}
	_ resource.ResourceWithConfigure      = &secretResource{}
	_ resource.ResourceWithValidateConfig = &secretResource{}
)

func newSecretResource() resource.Resource {
	return &secretResource{}
}

func (r *secretResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_secret"
}

func (r *secretResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A secret, referenced by name from the `secrets` section of a model's config.\n\n" +
			"Baseten never returns a secret's value, so the provider cannot detect a value changed outside " +
			"Terraform.\n\n" +
			"~> Terraform has no dependency edge between a secret and the model that reads it, because the " +
			"reference lives in the model's config rather than in Terraform. Use `depends_on` to ensure a secret " +
			"exists before a model that needs it is pushed.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the secret. Changing this creates a new secret and deletes the old one.",
				Required:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"team_id": schema.StringAttribute{
				MarkdownDescription: "ID of the team owning the secret. Conflicts with `team_name`. When neither " +
					"is set, the organization's default team is used.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"team_name": schema.StringAttribute{
				MarkdownDescription: "Name of the team owning the secret, resolved to an ID. Conflicts with " +
					"`team_id`. When neither is set, the organization's default team is used.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"value_wo": schema.StringAttribute{
				MarkdownDescription: "Value of the secret. Write-only, so it is never persisted to state or plan " +
					"files. Requires Terraform 1.11 or OpenTofu 1.11 and later. Increment `value_wo_version` to " +
					"push a new value.",
				Required:  true,
				WriteOnly: true,
				Sensitive: true,
			},
			"value_wo_version": schema.Int64Attribute{
				MarkdownDescription: "Increment to push the current `value_wo` to Baseten. Required because a " +
					"write-only value is never stored, so changing `value_wo` alone produces no plan diff.",
				Required: true,
			},
			"deletion_protection": schema.BoolAttribute{
				MarkdownDescription: "Whether Terraform is prevented from deleting this secret. Defaults to true, " +
					"because a deleted secret cannot be recovered through the API and any model reading it breaks. " +
					"Set to false and apply before destroying.",
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
			},
			"id": schema.StringAttribute{
				MarkdownDescription: "Stable identifier for the secret. Unchanged across rotation.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "Time the secret was created, in ISO 8601 format.",
				Computed:            true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *secretResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config secretResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !config.TeamID.IsNull() && !config.TeamName.IsNull() {
		resp.Diagnostics.AddError(
			"Conflicting team attributes",
			"Set team_id or team_name, not both.",
		)
	}
}

func (r *secretResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *secretResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan, config secretResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	// Write-only values are nulled out of the plan, so the value comes from the config.
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	teamID, diags := resolveTeamID(ctx, r.client, config.TeamID, config.TeamName)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	secret, err := r.upsertSecret(ctx, teamID, plan.Name.ValueString(), config.ValueWO.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Baseten secret", err.Error())
		return
	}

	applySecretToModel(&plan, teamID, secret)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state secretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// There is no get-by-name endpoint, so the list is filtered here.
	teamID := state.TeamID.ValueString()
	var secrets *managementapi.Secrets
	var err error
	if teamID == "" {
		secrets, err = r.client.API().GetSecrets(ctx)
	} else {
		secrets, err = r.client.API().GetTeamsSecrets(ctx, teamID)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Baseten secrets", err.Error())
		return
	}

	name := state.Name.ValueString()
	for _, secret := range secrets.Secrets {
		if secret.Name == name {
			applySecretToModel(&state, teamID, &secret)
			resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
			return
		}
	}
	resp.State.RemoveResource(ctx)
}

func (r *secretResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, config, state secretResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Other attributes are diffable, notably deletion_protection, which every destroy
	// must flip. Only a version bump means the value should be rewritten.
	if plan.ValueWOVersion.Equal(state.ValueWOVersion) {
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
		return
	}

	// A name or team change forces replacement, so the team is already resolved in state.
	teamID := plan.TeamID.ValueString()

	// POST is an upsert, so this rewrites the value of the existing secret.
	secret, err := r.upsertSecret(ctx, teamID, plan.Name.ValueString(), config.ValueWO.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to update Baseten secret", err.Error())
		return
	}

	applySecretToModel(&plan, teamID, secret)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *secretResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state secretResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if state.DeletionProtection.ValueBool() {
		resp.Diagnostics.AddAttributeError(
			path.Root("deletion_protection"),
			"Secret is protected from deletion",
			"Set deletion_protection = false and apply that change before destroying this secret. A deleted "+
				"secret cannot be recovered through the API.",
		)
		return
	}

	var err error
	if teamID := state.TeamID.ValueString(); teamID == "" {
		_, err = r.client.API().DeleteSecrets(ctx, state.Name.ValueString())
	} else {
		_, err = r.client.API().DeleteTeamsSecrets(ctx, teamID, state.Name.ValueString())
	}
	if err != nil {
		var respErr *managementapi.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			return
		}
		resp.Diagnostics.AddError("Unable to delete Baseten secret", err.Error())
	}
}

func (r *secretResource) upsertSecret(ctx context.Context, teamID, name, value string) (*managementapi.Secret, error) {
	body := managementapi.UpsertSecretRequest{Name: name, Value: value}
	if teamID == "" {
		return r.client.API().PostSecrets(ctx, body)
	}
	return r.client.API().PostTeamsSecrets(ctx, teamID, body)
}

func applySecretToModel(model *secretResourceModel, teamID string, secret *managementapi.Secret) {
	model.ID = types.StringValue(secret.Id)
	model.CreatedAt = types.StringValue(secret.CreatedAt.Format(time.RFC3339))
	model.TeamName = types.StringValue(secret.TeamName)
	if teamID == "" {
		model.TeamID = types.StringNull()
	} else {
		model.TeamID = types.StringValue(teamID)
	}
}
