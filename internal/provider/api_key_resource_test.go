package provider

import (
	"encoding/json"
	"testing"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	fwschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestAPIKeyResourceSchema(t *testing.T) {
	resp := &fwresource.SchemaResponse{}
	newAPIKeyResource().Schema(t.Context(), fwresource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema is invalid: %v", diags)
	}
}

func TestAPIKeyResourceValidateConfigRejectsBothTeamAttributes(t *testing.T) {
	r := newAPIKeyResource()
	apiKeySchema, objectType := apiKeyTestSchema(t, r)

	values := apiKeyTestValues()
	values["team_id"] = tftypes.NewValue(tftypes.String, "team-ml")
	values["team_name"] = tftypes.NewValue(tftypes.String, "ml")

	validator, ok := r.(fwresource.ResourceWithValidateConfig)
	if !ok {
		t.Fatalf("got %T, want a resource implementing ResourceWithValidateConfig", r)
	}
	resp := &fwresource.ValidateConfigResponse{}
	validator.ValidateConfig(t.Context(), fwresource.ValidateConfigRequest{
		Config: tfsdk.Config{Schema: apiKeySchema, Raw: tftypes.NewValue(objectType, values)},
	}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("got no error for team_id and team_name together, want one")
	}
}

func TestAPIKeyResourceCreate(t *testing.T) {
	tests := []struct {
		name      string
		teamID    string
		returned  string
		wantPath  string
		wantError bool
		wantKey   string
	}{
		{
			name:     "OrgScopedDerivesPrefix",
			returned: "abcd1234.Zm9vYmFy",
			wantPath: "/v1/api_keys",
			wantKey:  "abcd1234",
		},
		{
			name:     "TeamScopedPostsToTeamEndpoint",
			teamID:   "team-ml",
			returned: "efgh5678.YmFyYmF6",
			wantPath: "/v1/teams/team-ml/api_keys",
			wantKey:  "efgh5678",
		},
		{
			// Every later call identifies the key by prefix, so a value it cannot be
			// cut out of has to fail loudly rather than record a wrong prefix.
			name:      "ValueWithoutPrefixFails",
			returned:  "novalidprefix",
			wantPath:  "/v1/api_keys",
			wantError: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			r := newAPIKeyResource()
			apiKeySchema, objectType := apiKeyTestSchema(t, r)

			fake, managementClient := newFakeAPI(t, map[string]any{
				"POST " + test.wantPath: managementapi.APIKey{ApiKey: test.returned},
			})
			r.(*apiKeyResource).client = managementClient

			values := apiKeyTestValues()
			if test.teamID != "" {
				values["team_id"] = tftypes.NewValue(tftypes.String, test.teamID)
			}
			planRaw := tftypes.NewValue(objectType, values)

			resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: apiKeySchema, Raw: planRaw}}
			r.Create(ctx, fwresource.CreateRequest{
				Plan:   tfsdk.Plan{Schema: apiKeySchema, Raw: planRaw},
				Config: tfsdk.Config{Schema: apiKeySchema, Raw: planRaw},
			}, resp)

			if test.wantError {
				if !resp.Diagnostics.HasError() {
					t.Fatal("got no error for a key without a prefix, want one")
				}
				return
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
			}
			if posts := fake.requestsTo("POST", test.wantPath); len(posts) != 1 {
				t.Fatalf("got %d POSTs to %s, want 1", len(posts), test.wantPath)
			}

			var got apiKeyResourceModel
			if diags := resp.State.Get(ctx, &got); diags.HasError() {
				t.Fatalf("reading resulting state: %v", diags)
			}
			if got.Prefix.ValueString() != test.wantKey {
				t.Errorf("got prefix %q, want %q", got.Prefix.ValueString(), test.wantKey)
			}
			if got.APIKey.ValueString() != test.returned {
				t.Errorf("got api_key %q, want %q", got.APIKey.ValueString(), test.returned)
			}
			// An unset team records as null, meaning the organization's default.
			if test.teamID == "" && !got.TeamID.IsNull() {
				t.Errorf("got team_id %q, want null", got.TeamID.ValueString())
			}
			if test.teamID != "" && got.TeamID.ValueString() != test.teamID {
				t.Errorf("got team_id %q, want %q", got.TeamID.ValueString(), test.teamID)
			}
		})
	}
}

func TestAPIKeyResourceCreateSendsNameAndModelIDs(t *testing.T) {
	ctx := t.Context()
	r := newAPIKeyResource()
	apiKeySchema, objectType := apiKeyTestSchema(t, r)

	fake, managementClient := newFakeAPI(t, map[string]any{
		"POST /v1/api_keys": managementapi.APIKey{ApiKey: "abcd1234.Zm9vYmFy"},
	})
	r.(*apiKeyResource).client = managementClient

	values := apiKeyTestValues()
	values["model_ids"] = tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{
		tftypes.NewValue(tftypes.String, "model-a"),
	})
	planRaw := tftypes.NewValue(objectType, values)

	resp := &fwresource.CreateResponse{State: tfsdk.State{Schema: apiKeySchema, Raw: planRaw}}
	r.Create(ctx, fwresource.CreateRequest{
		Plan:   tfsdk.Plan{Schema: apiKeySchema, Raw: planRaw},
		Config: tfsdk.Config{Schema: apiKeySchema, Raw: planRaw},
	}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	// Asserted against the recorded request rather than the resulting state,
	// which only ever echoes the plan and so cannot catch a dropped field.
	posts := fake.requestsTo("POST", "/v1/api_keys")
	if len(posts) != 1 {
		t.Fatalf("got %d POSTs to /v1/api_keys, want 1", len(posts))
	}
	var sent managementapi.CreateAPIKeyRequest
	if err := json.Unmarshal([]byte(posts[0].Body), &sent); err != nil {
		t.Fatalf("decoding request body %q: %v", posts[0].Body, err)
	}
	if sent.Type != managementapi.APIKeyCategory_WORKSPACE_INVOKE {
		t.Errorf("got type %q, want %q", sent.Type, managementapi.APIKeyCategory_WORKSPACE_INVOKE)
	}
	if sent.Name == nil {
		t.Error("got no name in the request, want ci")
	} else if *sent.Name != "ci" {
		t.Errorf("got name %q, want %q", *sent.Name, "ci")
	}
	if sent.ModelIds == nil {
		t.Fatal("got no model_ids in the request, want [model-a]")
	}
	if len(*sent.ModelIds) != 1 || (*sent.ModelIds)[0] != "model-a" {
		t.Errorf("got model_ids %v, want [model-a]", *sent.ModelIds)
	}
}

// Baseten reports "scoped to no models" and "scoped to every model" alike, so a
// read that collapsed a configured empty set to null would disagree with the
// configuration forever, replacing the key on every apply.
func TestAPIKeyResourceReadKeepsEmptyModelIDs(t *testing.T) {
	ctx := t.Context()
	r := newAPIKeyResource()
	apiKeySchema, objectType := apiKeyTestSchema(t, r)

	_, managementClient := newFakeAPI(t, map[string]any{
		"GET /v1/api_keys": managementapi.APIKeys{Keys: []managementapi.APIKeyInfo{
			{Prefix: "abcd1234", Type: managementapi.APIKeyCategory_WORKSPACE_INVOKE},
		}},
	})
	r.(*apiKeyResource).client = managementClient

	values := apiKeyTestValues()
	values["prefix"] = tftypes.NewValue(tftypes.String, "abcd1234")
	values["model_ids"] = tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, []tftypes.Value{})
	stateRaw := tftypes.NewValue(objectType, values)

	resp := &fwresource.ReadResponse{State: tfsdk.State{Schema: apiKeySchema, Raw: stateRaw}}
	r.Read(ctx, fwresource.ReadRequest{State: tfsdk.State{Schema: apiKeySchema, Raw: stateRaw}}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	var got apiKeyResourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	if got.ModelIDs.IsNull() {
		t.Error("got null model_ids, want the empty set preserved")
	}
	if elements := got.ModelIDs.Elements(); len(elements) != 0 {
		t.Errorf("got model_ids %v, want empty", elements)
	}
}

// An imported key seeds only its prefix, so the refresh is the only chance to
// learn which team owns it. Without this, a team key reads as default-team
// scoped and a configuration naming its real team replaces it.
func TestAPIKeyResourceReadPopulatesTeamName(t *testing.T) {
	ctx := t.Context()
	r := newAPIKeyResource()
	apiKeySchema, objectType := apiKeyTestSchema(t, r)

	teamName := "research"
	_, managementClient := newFakeAPI(t, map[string]any{
		"GET /v1/api_keys": managementapi.APIKeys{Keys: []managementapi.APIKeyInfo{
			{
				Prefix:   "abcd1234",
				Type:     managementapi.APIKeyCategory_WORKSPACE_INVOKE,
				TeamName: &teamName,
			},
		}},
	})
	r.(*apiKeyResource).client = managementClient

	values := apiKeyTestValues()
	values["prefix"] = tftypes.NewValue(tftypes.String, "abcd1234")
	stateRaw := tftypes.NewValue(objectType, values)

	resp := &fwresource.ReadResponse{State: tfsdk.State{Schema: apiKeySchema, Raw: stateRaw}}
	r.Read(ctx, fwresource.ReadRequest{State: tfsdk.State{Schema: apiKeySchema, Raw: stateRaw}}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
	}

	var got apiKeyResourceModel
	if diags := resp.State.Get(ctx, &got); diags.HasError() {
		t.Fatalf("reading resulting state: %v", diags)
	}
	if got.TeamName.ValueString() != teamName {
		t.Errorf("got team_name %q, want %q", got.TeamName.ValueString(), teamName)
	}
}

func TestAPIKeyResourceRead(t *testing.T) {
	name := "ci"
	tests := []struct {
		name      string
		keys      []managementapi.APIKeyInfo
		wantGone  bool
		wantName  string
		wantType  string
		wantModel []string
	}{
		{
			name: "RefreshesFromList",
			keys: []managementapi.APIKeyInfo{
				{Prefix: "other999", Type: managementapi.APIKeyCategory_WORKSPACE_MANAGE_ALL},
				{
					Prefix:   "abcd1234",
					Name:     &name,
					Type:     managementapi.APIKeyCategory_WORKSPACE_INVOKE,
					ModelIds: &[]string{"model-a"},
				},
			},
			wantName:  name,
			wantType:  string(managementapi.APIKeyCategory_WORKSPACE_INVOKE),
			wantModel: []string{"model-a"},
		},
		{
			// The list holds only keys that have not been revoked, so a rotated or
			// externally revoked key drops out of state and is recreated.
			name:     "RevokedKeyLeavesState",
			keys:     []managementapi.APIKeyInfo{{Prefix: "other999"}},
			wantGone: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			r := newAPIKeyResource()
			apiKeySchema, objectType := apiKeyTestSchema(t, r)

			_, managementClient := newFakeAPI(t, map[string]any{
				"GET /v1/api_keys": managementapi.APIKeys{Keys: test.keys},
			})
			r.(*apiKeyResource).client = managementClient

			values := apiKeyTestValues()
			values["prefix"] = tftypes.NewValue(tftypes.String, "abcd1234")
			values["api_key"] = tftypes.NewValue(tftypes.String, "abcd1234.Zm9vYmFy")
			stateRaw := tftypes.NewValue(objectType, values)

			resp := &fwresource.ReadResponse{State: tfsdk.State{Schema: apiKeySchema, Raw: stateRaw}}
			r.Read(ctx, fwresource.ReadRequest{
				State: tfsdk.State{Schema: apiKeySchema, Raw: stateRaw},
			}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
			}
			if test.wantGone {
				if !resp.State.Raw.IsNull() {
					t.Error("got a populated state for a revoked key, want it removed")
				}
				return
			}

			var got apiKeyResourceModel
			if diags := resp.State.Get(ctx, &got); diags.HasError() {
				t.Fatalf("reading resulting state: %v", diags)
			}
			if got.Name.ValueString() != test.wantName {
				t.Errorf("got name %q, want %q", got.Name.ValueString(), test.wantName)
			}
			if got.Type.ValueString() != test.wantType {
				t.Errorf("got type %q, want %q", got.Type.ValueString(), test.wantType)
			}
			var modelIDs []string
			if diags := got.ModelIDs.ElementsAs(ctx, &modelIDs, false); diags.HasError() {
				t.Fatalf("reading model_ids: %v", diags)
			}
			if len(modelIDs) != len(test.wantModel) {
				t.Fatalf("got model_ids %v, want %v", modelIDs, test.wantModel)
			}
			for i, want := range test.wantModel {
				if modelIDs[i] != want {
					t.Errorf("got model_ids[%d] %q, want %q", i, modelIDs[i], want)
				}
			}
			// The value is never returned by a read, so it survives from prior state.
			if got.APIKey.ValueString() != "abcd1234.Zm9vYmFy" {
				t.Errorf("got api_key %q, want it preserved from state", got.APIKey.ValueString())
			}
		})
	}
}

func apiKeyTestSchema(t *testing.T, r fwresource.Resource) (fwschema.Schema, tftypes.Object) {
	t.Helper()

	resp := &fwresource.SchemaResponse{}
	r.Schema(t.Context(), fwresource.SchemaRequest{}, resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", resp.Diagnostics)
	}
	objectType, ok := resp.Schema.Type().TerraformType(t.Context()).(tftypes.Object)
	if !ok {
		t.Fatalf("got schema type %T, want tftypes.Object", resp.Schema.Type().TerraformType(t.Context()))
	}
	return resp.Schema, objectType
}

// apiKeyTestValues is a minimal workspace-invoke key, for tests to adjust.
func apiKeyTestValues() map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"type":                tftypes.NewValue(tftypes.String, string(managementapi.APIKeyCategory_WORKSPACE_INVOKE)),
		"name":                tftypes.NewValue(tftypes.String, "ci"),
		"model_ids":           tftypes.NewValue(tftypes.Set{ElementType: tftypes.String}, nil),
		"team_id":             tftypes.NewValue(tftypes.String, nil),
		"team_name":           tftypes.NewValue(tftypes.String, nil),
		"rotate_when_changed": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, nil),
		"api_key":             tftypes.NewValue(tftypes.String, nil),
		"prefix":              tftypes.NewValue(tftypes.String, nil),
	}
}
