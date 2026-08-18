package provider

import (
	"context"
	"fmt"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// resolveTeamID returns the team ID to address, empty when the organization's
// default team should be used. Only a configured team_name needs a lookup.
//
// Both attributes are Optional+Computed on every resource that has them, so an
// unset one arrives unknown on a create rather than null. Unknown therefore has
// to read as absent: treating it as set would resolve a configured team_name to
// the default team on the very first apply.
func resolveTeamID(ctx context.Context, managementClient *client.ManagementClient, teamID, teamName types.String) (string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if modelIsSet(teamID) {
		return teamID.ValueString(), diags
	}
	if !modelIsSet(teamName) {
		return "", diags
	}

	name := teamName.ValueString()
	teams, err := managementClient.API().GetTeams(ctx, managementapi.GetV1TeamsParams{})
	if err != nil {
		diags.AddError("Unable to list Baseten teams", err.Error())
		return "", diags
	}

	found := ""
	for _, team := range teams.Teams {
		if team.Name != name {
			continue
		}
		if found != "" {
			diags.AddAttributeError(
				path.Root("team_name"),
				"Ambiguous team name",
				fmt.Sprintf("Multiple teams are named %q. Set team_id instead.", name),
			)
			return "", diags
		}
		found = team.Id
	}
	if found == "" {
		diags.AddAttributeError(
			path.Root("team_name"),
			"Team not found",
			fmt.Sprintf("No team is named %q.", name),
		)
	}
	return found, diags
}
