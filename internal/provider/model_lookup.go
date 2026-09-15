package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// modelIdentity is how a configuration points at a model: exactly one of name or
// id, optionally narrowed to a team, since model names are unique only within
// one. Shared by the resource and the data source so the two resolve a model the
// same way and report the same mistakes.
type modelIdentity struct {
	ID       types.String
	Name     types.String
	TeamID   types.String
	TeamName types.String
}

// validateModelIdentity reports the identity mistakes visible in configuration
// alone. Configuration never carries a backfilled value, so null is exactly what
// the user left out, and an unknown value counts as set: it is a reference that
// resolves later.
func validateModelIdentity(identity modelIdentity, diags *diag.Diagnostics) {
	if !identity.TeamID.IsNull() && !identity.TeamName.IsNull() {
		diags.AddError(
			"Conflicting team attributes",
			"Set team_id or team_name, not both.",
		)
	}
	switch {
	case !identity.ID.IsNull() && !identity.Name.IsNull():
		diags.AddError(
			"Conflicting model identifiers",
			"Set name or id, not both.",
		)
	case identity.ID.IsNull() && identity.Name.IsNull():
		diags.AddError(
			"Missing model identifier",
			"Set name or id so the provider can find the model.",
		)
	}
}

// findModel resolves the identity, reporting whether the model exists rather than
// treating absence as an error: the resource creates an absent model when it has
// a push, and the data source reports the failure itself. The resolved team ID is
// returned alongside, empty for the organization's default team.
//
// Every attribute here is Optional+Computed, so the one the user did not set
// arrives unknown rather than null and unknown has to read as absent.
func findModel(
	ctx context.Context,
	managementClient *client.ManagementClient,
	identity modelIdentity,
) (*managementapi.Model, string, bool, diag.Diagnostics) {
	teamID, diags := resolveTeamID(ctx, managementClient, identity.TeamID, identity.TeamName)
	if diags.HasError() {
		return nil, "", false, diags
	}

	if modelIsSet(identity.ID) {
		model, err := managementClient.API().GetModelsModelId(ctx, identity.ID.ValueString())
		if err != nil {
			if modelIsNotFound(err) {
				return nil, teamID, false, diags
			}
			diags.AddError("Unable to read Baseten model", err.Error())
			return nil, "", false, diags
		}
		return model, teamID, true, diags
	}

	name := identity.Name.ValueString()
	var models *managementapi.Models
	var err error
	if teamID == "" {
		models, err = managementClient.API().GetModels(ctx, managementapi.GetV1ModelsParams{Name: &name})
	} else {
		models, err = managementClient.API().GetTeamsModels(ctx, teamID, managementapi.GetV1TeamsTeamIdModelsParams{Name: &name})
	}
	if err != nil {
		diags.AddError("Unable to list Baseten models", err.Error())
		return nil, "", false, diags
	}

	if len(models.Models) == 0 {
		return nil, teamID, false, diags
	} else if len(models.Models) > 1 {
		diags.AddAttributeError(
			path.Root("name"),
			"Ambiguous model name",
			fmt.Sprintf("Several teams have a model named %q, because model names are unique only within "+
				"a team. Set team_id or team_name.", name),
		)
		return nil, "", false, diags
	}
	return &models.Models[0], teamID, true, diags
}

func modelIsNotFound(err error) bool {
	var respErr *managementapi.ResponseError
	return errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound
}
