package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// apiKeyResource manages an API key.
//
// It has no deletion_protection attribute, unlike secretResource, which is
// deliberate. Rotation is the workflow this resource exists for, Terraform
// replaces a resource by destroying and recreating it, and with no update
// endpoint every attribute change is a replacement, so a deletion guard would
// block rotation rather than protect it. Terraform's own prevent_destroy already
// covers pinning one key, and it blocks replacement too, which is the honest
// semantic for a flag named after deletion.
//
// A survey of aws_iam_access_key, google_service_account_key,
// azuread_application_password, datadog_api_key, cloudflare_api_token, and
// gitlab_personal_access_token found five of the six carry no destroy protection
// at all. The exception, google_service_account_key, defaults its deletion_policy
// to DELETE, even though that provider defaults google_sql_database_instance to
// protected. So the precedent is not just that credentials go unprotected, but
// that a provider protecting stateful resources still leaves credentials
// deletable, which is the same line drawn here against baseten_secret.
type apiKeyResource struct {
	client *client.ManagementClient
}

type apiKeyResourceModel struct {
	Type              types.String `tfsdk:"type"`
	Name              types.String `tfsdk:"name"`
	ModelIDs          types.Set    `tfsdk:"model_ids"`
	TeamID            types.String `tfsdk:"team_id"`
	TeamName          types.String `tfsdk:"team_name"`
	RotateWhenChanged types.Map    `tfsdk:"rotate_when_changed"`
	APIKey            types.String `tfsdk:"api_key"`
	Prefix            types.String `tfsdk:"prefix"`
}

var (
	_ resource.Resource                   = &apiKeyResource{}
	_ resource.ResourceWithConfigure      = &apiKeyResource{}
	_ resource.ResourceWithValidateConfig = &apiKeyResource{}
	_ resource.ResourceWithImportState    = &apiKeyResource{}
)

func newAPIKeyResource() resource.Resource {
	return &apiKeyResource{}
}

func (r *apiKeyResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_api_key"
}

func (r *apiKeyResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "An API key, for authenticating against the Baseten API.\n\n" +
			"Baseten returns a key's value once, when it is created, so `api_key` is stored in Terraform " +
			"state. Treat the state as a secret.\n\n" +
			"There is no endpoint for changing an existing key, so every attribute replaces the key when " +
			"it changes. That is what makes rotation work: change `rotate_when_changed` and the next apply " +
			"revokes the old key and mints a new one.\n\n" +
			"Unlike `baseten_secret`, this resource has no `deletion_protection` attribute, because a guard " +
			"on deletion would also block rotation: Terraform replaces a resource by destroying it and " +
			"creating it again. Use `lifecycle { prevent_destroy = true }` to pin one key, accepting that " +
			"it blocks rotation too.",
		Attributes: map[string]schema.Attribute{
			"type": schema.StringAttribute{
				MarkdownDescription: "Scope of the API key. One of `WORKSPACE_INVOKE`, " +
					"`WORKSPACE_EXPORT_METRICS`, `WORKSPACE_MANAGE_ALL`, `WORKSPACE_MANAGE_API_KEYS`, or " +
					"`PERSONAL`. Which of them you can create depends on the API key the provider itself " +
					"authenticates with. Changing this creates a new key and deletes the old one.",
				Required: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"name": schema.StringAttribute{
				MarkdownDescription: "Name of the API key. Must be unique among keys that have not been " +
					"revoked, within a scope that depends on `type`: the team for workspace keys, whatever " +
					"their type, the organization for `WORKSPACE_MANAGE_API_KEYS`, and your own keys for " +
					"`PERSONAL`. Leave it unset to skip the uniqueness rule entirely. Changing this creates a " +
					"new key and deletes the old one.",
				Optional: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"model_ids": schema.SetAttribute{
				MarkdownDescription: "IDs of the models the key is limited to, from " +
					"`baseten_model.example.id` or `data.baseten_model.example.id`. Accepted only for " +
					"`WORKSPACE_INVOKE` and `WORKSPACE_EXPORT_METRICS`; Baseten rejects it for every other " +
					"`type`. When unset, the key reaches every model in its scope. Changing this creates a " +
					"new key and deletes the old one.",
				Optional:    true,
				ElementType: types.StringType,
				PlanModifiers: []planmodifier.Set{
					setplanmodifier.RequiresReplace(),
				},
			},
			"team_id": schema.StringAttribute{
				MarkdownDescription: "ID of the team owning the key. Conflicts with `team_name`. When " +
					"neither is set, the organization's default team is used. Not supported for `PERSONAL` " +
					"or `WORKSPACE_MANAGE_API_KEYS` keys.",
				Optional: true,
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplaceIfConfigured(),
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"team_name": schema.StringAttribute{
				MarkdownDescription: "Name of the team owning the key, resolved to an ID. Conflicts with " +
					"`team_id`. Recorded only as written here, because Baseten does not return it when " +
					"creating a key.",
				Optional: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
				},
			},
			"rotate_when_changed": schema.MapAttribute{
				MarkdownDescription: "Arbitrary key/value pairs that replace the API key when they change, " +
					"so a key can be rotated on an external condition such as a rotating timestamp. Pair it " +
					"with the `time_rotating` resource rather than `timestamp()`, which changes on every " +
					"plan.",
				Optional:    true,
				ElementType: types.StringType,
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.RequiresReplace(),
				},
			},
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Value of the API key. Baseten returns it once, when the key is " +
					"created, so it is stored in Terraform state and cannot be read back afterwards. It is " +
					"null for a key brought under management with `terraform import`.",
				Computed:  true,
				Sensitive: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
			"prefix": schema.StringAttribute{
				MarkdownDescription: "Prefix of the API key, which identifies it without revealing it. This " +
					"is what the Baseten UI and `baseten org api-key list` display, and what identifies the " +
					"key to `terraform import`.",
				Computed: true,
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.UseStateForUnknown(),
				},
			},
		},
	}
}

func (r *apiKeyResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config apiKeyResourceModel
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

func (r *apiKeyResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

func (r *apiKeyResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan apiKeyResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	teamID, diags := resolveTeamID(ctx, r.client, plan.TeamID, plan.TeamName)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Baseten decides which types and attribute combinations are legal, so the
	// request is assembled from the configuration as written.
	body := managementapi.CreateAPIKeyRequest{Type: managementapi.APIKeyCategory(plan.Type.ValueString())}
	if modelIsSet(plan.Name) {
		name := plan.Name.ValueString()
		body.Name = &name
	}
	if modelIsSet(plan.ModelIDs) {
		var modelIDs []string
		resp.Diagnostics.Append(plan.ModelIDs.ElementsAs(ctx, &modelIDs, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.ModelIds = &modelIDs
	}

	var key *managementapi.APIKey
	var err error
	if teamID == "" {
		key, err = r.client.API().PostApiKeys(ctx, body)
	} else {
		key, err = r.client.API().PostTeamsApiKeys(ctx, teamID, body)
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Baseten API key", err.Error())
		return
	}

	// Creating a key returns the value alone, but every later call identifies it
	// by prefix, which is the part of the value before the first dot.
	prefix, _, found := strings.Cut(key.ApiKey, ".")
	if !found {
		resp.Diagnostics.AddError(
			"Unrecognized Baseten API key format",
			"Baseten returned a key with no prefix, so Terraform cannot identify it well enough to read "+
				"or delete it. The key was created: revoke it in the Baseten UI, because Terraform has not "+
				"recorded it.",
		)
		return
	}

	plan.APIKey = types.StringValue(key.ApiKey)
	plan.Prefix = types.StringValue(prefix)
	plan.TeamID = apiKeyTeamIDValue(teamID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *apiKeyResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state apiKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// There is no get-by-prefix endpoint, so the list is filtered here. It holds
	// only keys that have not been revoked, so a revoked key reads as absent.
	keys, err := r.client.API().GetApiKeys(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read Baseten API keys", err.Error())
		return
	}

	prefix := state.Prefix.ValueString()
	for _, key := range keys.Keys {
		if key.Prefix != prefix {
			continue
		}
		state.Type = types.StringValue(string(key.Type))
		if key.Name == nil {
			state.Name = types.StringNull()
		} else {
			state.Name = types.StringValue(*key.Name)
		}
		modelIDs, diags := apiKeyModelIDs(ctx, key.ModelIds)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		state.ModelIDs = modelIDs
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}
	resp.State.RemoveResource(ctx)
}

// Update exists to satisfy the interface. Every attribute requires replacement,
// because Baseten has no endpoint for changing a key, so it is never called.
func (r *apiKeyResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	resp.Diagnostics.AddError(
		"API keys cannot be updated in place",
		"Every attribute of an API key requires the key to be replaced, so this is a bug in the provider.",
	)
}

func (r *apiKeyResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state apiKeyResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if _, err := r.client.API().DeleteApiKeys(ctx, state.Prefix.ValueString()); err != nil {
		var respErr *managementapi.ResponseError
		if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
			return
		}
		resp.Diagnostics.AddError("Unable to delete Baseten API key", err.Error())
	}
}

func (r *apiKeyResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Read fills in the rest from the prefix. api_key stays null, because Baseten
	// returns a key's value only when it is created.
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("prefix"), req.ID)...)
}

// apiKeyTeamIDValue reports the team to record in state, where an unset team
// means the organization's default rather than a known ID.
func apiKeyTeamIDValue(teamID string) types.String {
	if teamID == "" {
		return types.StringNull()
	}
	return types.StringValue(teamID)
}

// apiKeyModelIDs converts the scoped model IDs of a key, which Baseten omits
// rather than sending an empty list.
func apiKeyModelIDs(ctx context.Context, modelIDs *[]string) (types.Set, diag.Diagnostics) {
	if modelIDs == nil || len(*modelIDs) == 0 {
		return types.SetNull(types.StringType), nil
	}
	return types.SetValueFrom(ctx, types.StringType, *modelIDs)
}
