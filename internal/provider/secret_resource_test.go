package provider

import (
	"testing"
	"time"

	"github.com/basetenlabs/baseten-go/client/managementapi"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestSecretResourceSchema(t *testing.T) {
	resp := &fwresource.SchemaResponse{}
	newSecretResource().Schema(t.Context(), fwresource.SchemaRequest{}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("schema returned diagnostics: %v", resp.Diagnostics)
	}
	if diags := resp.Schema.ValidateImplementation(t.Context()); diags.HasError() {
		t.Fatalf("schema is invalid: %v", diags)
	}
}

func TestSecretResourceValidateConfigRejectsBothTeamAttributes(t *testing.T) {
	r := newSecretResource()

	schemaResp := &fwresource.SchemaResponse{}
	r.Schema(t.Context(), fwresource.SchemaRequest{}, schemaResp)

	objectType, ok := schemaResp.Schema.Type().TerraformType(t.Context()).(tftypes.Object)
	if !ok {
		t.Fatalf("got schema type %T, want tftypes.Object", schemaResp.Schema.Type().TerraformType(t.Context()))
	}
	config := tfsdk.Config{
		Schema: schemaResp.Schema,
		Raw: tftypes.NewValue(objectType, map[string]tftypes.Value{
			"name":                tftypes.NewValue(tftypes.String, "hf_access_token"),
			"team_id":             tftypes.NewValue(tftypes.String, "team-ml"),
			"team_name":           tftypes.NewValue(tftypes.String, "ml"),
			"value_wo":            tftypes.NewValue(tftypes.String, "secret-value"),
			"value_wo_version":    tftypes.NewValue(tftypes.Number, 1),
			"deletion_protection": tftypes.NewValue(tftypes.Bool, false),
			"id":                  tftypes.NewValue(tftypes.String, nil),
			"created_at":          tftypes.NewValue(tftypes.String, nil),
		}),
	}

	validator, ok := r.(fwresource.ResourceWithValidateConfig)
	if !ok {
		t.Fatalf("got %T, want a resource implementing ResourceWithValidateConfig", r)
	}
	resp := &fwresource.ValidateConfigResponse{}
	validator.ValidateConfig(t.Context(), fwresource.ValidateConfigRequest{Config: config}, resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("got no error for team_id and team_name together, want one")
	}
}

func TestSecretResourceUpdateWritesValueOnlyOnVersionChange(t *testing.T) {
	tests := []struct {
		name        string
		planVersion int64
		wantPosts   int
	}{
		// The new value_wo is ignored without a version bump, which is what makes
		// flipping deletion_protection safe.
		{name: "SameVersionSkipsWrite", planVersion: 1, wantPosts: 0},
		{name: "BumpedVersionWrites", planVersion: 2, wantPosts: 1},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			r := newSecretResource()

			schemaResp := &fwresource.SchemaResponse{}
			r.Schema(ctx, fwresource.SchemaRequest{}, schemaResp)
			if schemaResp.Diagnostics.HasError() {
				t.Fatalf("schema returned diagnostics: %v", schemaResp.Diagnostics)
			}
			secretSchema := schemaResp.Schema
			objectType, ok := secretSchema.Type().TerraformType(ctx).(tftypes.Object)
			if !ok {
				t.Fatalf("got schema type %T, want tftypes.Object", secretSchema.Type().TerraformType(ctx))
			}

			createdAt := testTimestamp(t)
			fake, managementClient := newFakeAPI(t, map[string]any{
				"POST /v1/secrets": managementapi.Secret{
					Id:        "secret-1",
					Name:      "hf_access_token",
					TeamName:  "Default Team",
					CreatedAt: createdAt,
				},
			})
			r.(*secretResource).client = managementClient

			// Prior state holds version 1 and protection on; the change under test turns
			// protection off while value_wo carries a different value than was written.
			state := tfsdk.State{Schema: secretSchema, Raw: tftypes.NewValue(objectType, map[string]tftypes.Value{
				"name":                tftypes.NewValue(tftypes.String, "hf_access_token"),
				"team_id":             tftypes.NewValue(tftypes.String, nil),
				"team_name":           tftypes.NewValue(tftypes.String, "Default Team"),
				"value_wo":            tftypes.NewValue(tftypes.String, nil),
				"value_wo_version":    tftypes.NewValue(tftypes.Number, 1),
				"deletion_protection": tftypes.NewValue(tftypes.Bool, true),
				"id":                  tftypes.NewValue(tftypes.String, "secret-1"),
				"created_at":          tftypes.NewValue(tftypes.String, createdAt.Format(time.RFC3339)),
			})}
			planValues := map[string]tftypes.Value{
				"name":                tftypes.NewValue(tftypes.String, "hf_access_token"),
				"team_id":             tftypes.NewValue(tftypes.String, nil),
				"team_name":           tftypes.NewValue(tftypes.String, "Default Team"),
				"value_wo":            tftypes.NewValue(tftypes.String, nil),
				"value_wo_version":    tftypes.NewValue(tftypes.Number, test.planVersion),
				"deletion_protection": tftypes.NewValue(tftypes.Bool, false),
				"id":                  tftypes.NewValue(tftypes.String, "secret-1"),
				"created_at":          tftypes.NewValue(tftypes.String, createdAt.Format(time.RFC3339)),
			}
			configValues := make(map[string]tftypes.Value, len(planValues))
			for name, value := range planValues {
				configValues[name] = value
			}
			configValues["value_wo"] = tftypes.NewValue(tftypes.String, "rotated-value")

			resp := &fwresource.UpdateResponse{
				State: tfsdk.State{Schema: secretSchema, Raw: state.Raw},
			}
			r.Update(ctx, fwresource.UpdateRequest{
				Plan:   tfsdk.Plan{Schema: secretSchema, Raw: tftypes.NewValue(objectType, planValues)},
				Config: tfsdk.Config{Schema: secretSchema, Raw: tftypes.NewValue(objectType, configValues)},
				State:  state,
			}, resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
			}
			if posts := fake.requestsTo("POST", "/v1/secrets"); len(posts) != test.wantPosts {
				t.Errorf("got %d POSTs to /v1/secrets, want %d", len(posts), test.wantPosts)
			}

			var got secretResourceModel
			if diags := resp.State.Get(ctx, &got); diags.HasError() {
				t.Fatalf("reading resulting state: %v", diags)
			}
			if got.DeletionProtection.ValueBool() {
				t.Error("got deletion_protection true in state, want false")
			}
			if got.ValueWOVersion.ValueInt64() != test.planVersion {
				t.Errorf("got value_wo_version %d, want %d", got.ValueWOVersion.ValueInt64(), test.planVersion)
			}
			if got.ID.ValueString() != "secret-1" {
				t.Errorf("got id %q, want %q", got.ID.ValueString(), "secret-1")
			}
		})
	}
}

func testTimestamp(t *testing.T) time.Time {
	t.Helper()

	timestamp, err := time.Parse(time.RFC3339, "2026-08-19T12:00:00Z")
	if err != nil {
		t.Fatalf("parsing timestamp: %v", err)
	}
	return timestamp
}
