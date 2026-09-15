package provider

import (
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestResolveTeamID(t *testing.T) {
	teams := managementapi.Teams{Teams: []managementapi.Team{
		{Id: "team-default", Name: "Default Team", Default: true},
		{Id: "team-ml", Name: "ml", Default: false},
		{Id: "team-dup-1", Name: "dup", Default: false},
		{Id: "team-dup-2", Name: "dup", Default: false},
	}}

	tests := []struct {
		name      string
		teamID    types.String
		teamName  types.String
		wantID    string
		wantError bool
	}{
		{
			name:     "IDPassesThrough",
			teamID:   types.StringValue("team-ml"),
			teamName: types.StringNull(),
			wantID:   "team-ml",
		},
		{
			name:     "NeitherSetUsesDefaultTeam",
			teamID:   types.StringNull(),
			teamName: types.StringNull(),
			wantID:   "",
		},
		{
			name:     "NameResolvesToID",
			teamID:   types.StringNull(),
			teamName: types.StringValue("ml"),
			wantID:   "team-ml",
		},
		{
			name:      "AmbiguousNameFails",
			teamID:    types.StringNull(),
			teamName:  types.StringValue("dup"),
			wantError: true,
		},
		{
			name:      "UnknownNameFails",
			teamID:    types.StringNull(),
			teamName:  types.StringValue("nope"),
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, managementClient := newFakeAPI(t, map[string]any{"GET /v1/teams": teams})

			gotID, diags := resolveTeamID(t.Context(), managementClient, test.teamID, test.teamName)
			if test.wantError {
				if !diags.HasError() {
					t.Fatalf("got no error, want one")
				}
				return
			}
			if diags.HasError() {
				t.Fatalf("got diagnostics %v, want none", diags)
			}
			if gotID != test.wantID {
				t.Errorf("got team ID %q, want %q", gotID, test.wantID)
			}
		})
	}
}
