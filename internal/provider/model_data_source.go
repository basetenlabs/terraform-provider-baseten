package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The optional interfaces are asserted because the framework discovers them by
// type assertion: a method whose signature drifts still compiles, and the
// behavior silently stops running.
var (
	_ datasource.DataSource                   = &modelDataSource{}
	_ datasource.DataSourceWithConfigure      = &modelDataSource{}
	_ datasource.DataSourceWithValidateConfig = &modelDataSource{}
)

// modelDataSource reads a model without managing it, which is how a
// configuration references one it does not own.
type modelDataSource struct {
	client *client.ManagementClient
}

type modelDataSourceModel struct {
	ID                      types.String `tfsdk:"id"`
	Name                    types.String `tfsdk:"name"`
	TeamID                  types.String `tfsdk:"team_id"`
	TeamName                types.String `tfsdk:"team_name"`
	CreatedAt               types.String `tfsdk:"created_at"`
	DeploymentsCount        types.Int64  `tfsdk:"deployments_count"`
	InstanceTypeName        types.String `tfsdk:"instance_type_name"`
	ProductionDeploymentID  types.String `tfsdk:"production_deployment_id"`
	DevelopmentDeploymentID types.String `tfsdk:"development_deployment_id"`
}

func (m modelDataSourceModel) identity() modelIdentity {
	return modelIdentity{ID: m.ID, Name: m.Name, TeamID: m.TeamID, TeamName: m.TeamName}
}

func newModelDataSource() datasource.DataSource {
	return &modelDataSource{}
}

func (d *modelDataSource) Metadata(ctx context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model"
}

func (d *modelDataSource) Schema(ctx context.Context, req datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Reads an existing Baseten model, so a configuration can refer to it without " +
			"managing it. Use this to build a predict URL or to pass a model's ID to another provider. " +
			"Managing a model's environments or its code takes the `baseten_model` resource instead.\n\n" +
			"Deployment identity is exposed here and not on the resource: a data source is read fresh on " +
			"every plan, so a promotion or a push made elsewhere is reported as the current value rather " +
			"than as drift.",
		Attributes: map[string]schema.Attribute{
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the model. Set this or `id`.",
				Optional:            true,
				Computed:            true,
			},
			"id": schema.StringAttribute{
				MarkdownDescription: "Unique identifier of the model. Set this or `name`.",
				Optional:            true,
				Computed:            true,
			},
			"team_id": schema.StringAttribute{
				MarkdownDescription: "ID of the team owning the model. Conflicts with `team_name`. Set either " +
					"one to resolve `name` within a single team, since model names are unique only within a team.",
				Optional: true,
				Computed: true,
			},
			"team_name": schema.StringAttribute{
				MarkdownDescription: "Name of the team owning the model, resolved to an ID. Conflicts with " +
					"`team_id`.",
				Optional: true,
				Computed: true,
			},
			"created_at": schema.StringAttribute{
				MarkdownDescription: "Time the model was created, in ISO 8601 format.",
				Computed:            true,
			},
			"deployments_count": schema.Int64Attribute{
				MarkdownDescription: "Number of deployments of the model.",
				Computed:            true,
			},
			"instance_type_name": schema.StringAttribute{
				MarkdownDescription: "Name of the instance type for the production deployment of the model.",
				Computed:            true,
			},
			"production_deployment_id": schema.StringAttribute{
				MarkdownDescription: "Unique identifier of the production deployment of the model. Null when " +
					"nothing is promoted to production.",
				Computed: true,
			},
			"development_deployment_id": schema.StringAttribute{
				MarkdownDescription: "Unique identifier of the development deployment of the model. Null when " +
					"the model has no development deployment.",
				Computed: true,
			},
		},
	}
}

func (d *modelDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var config modelDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	validateModelIdentity(config.identity(), &resp.Diagnostics)
}

func (d *modelDataSource) Configure(ctx context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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
	d.client = managementClient
}

func (d *modelDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config modelDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	model, teamID, found, diags := findModel(ctx, d.client, config.identity())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// Unlike the resource, a data source has no way to bring a model into being,
	// so an absent one is always an error.
	if !found {
		if modelIsSet(config.ID) {
			resp.Diagnostics.AddAttributeError(
				path.Root("id"),
				"Model not found",
				fmt.Sprintf("No model has ID %q.", config.ID.ValueString()),
			)
			return
		}
		resp.Diagnostics.AddAttributeError(
			path.Root("name"),
			"Model not found",
			fmt.Sprintf("No model is named %q.", config.Name.ValueString()),
		)
		return
	}

	config.ID = types.StringValue(model.Id)
	config.Name = types.StringValue(model.Name)
	config.TeamName = types.StringValue(model.TeamName)
	config.CreatedAt = types.StringValue(model.CreatedAt.Format(time.RFC3339))
	config.DeploymentsCount = types.Int64Value(int64(model.DeploymentsCount))
	config.InstanceTypeName = types.StringValue(model.InstanceTypeName)
	config.ProductionDeploymentID = types.StringPointerValue(model.ProductionDeploymentId)
	config.DevelopmentDeploymentID = types.StringPointerValue(model.DevelopmentDeploymentId)
	// The organization's default team is addressed by taking no team, so there is
	// no ID to report for it.
	if teamID == "" {
		config.TeamID = types.StringNull()
	} else {
		config.TeamID = types.StringValue(teamID)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &config)...)
}
