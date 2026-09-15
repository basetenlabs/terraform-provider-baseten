package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	fwresource "github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestModelPushValidateConfig(t *testing.T) {
	tests := []struct {
		name      string
		push      modelPushModel
		wantError string
	}{
		{
			name: "ConfigDirOnly",
			push: modelPushModel{ConfigDir: types.StringValue("./whisper")},
		},
		{
			name: "InlineConfigOnly",
			push: modelPushModel{Config: testModelConfig(t, "whisper")},
		},
		{
			name: "BothConflict",
			push: modelPushModel{
				ConfigDir: types.StringValue("./whisper"),
				Config:    testModelConfig(t, "whisper"),
			},
			wantError: "not both",
		},
		{
			name:      "NeitherIsMissingConfiguration",
			push:      modelPushModel{},
			wantError: "Missing model configuration",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, _, modelSchema := testModelResource(t, nil)
			config := tfsdk.Config{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
				Name: types.StringValue("whisper"),
				Push: testModelPushObject(t, test.push),
			})}

			resp := &fwresource.ValidateConfigResponse{}
			r.ValidateConfig(t.Context(), fwresource.ValidateConfigRequest{Config: config}, resp)

			if test.wantError == "" {
				if resp.Diagnostics.HasError() {
					t.Fatalf("got diagnostics %v, want none", resp.Diagnostics)
				}
				return
			}
			if !strings.Contains(testDiagnosticsText(resp.Diagnostics.Errors()), test.wantError) {
				t.Fatalf("got errors %v, want one containing %q", resp.Diagnostics.Errors(), test.wantError)
			}
		})
	}
}

// TestModelPushDeletionProtectionWithoutPushWarns covers the flag being
// meaningless in adopted mode, where destroy deletes no model.
func TestModelPushDeletionProtectionWithoutPushWarns(t *testing.T) {
	r, _, modelSchema := testModelResource(t, nil)
	config := tfsdk.Config{Schema: modelSchema, Raw: testModelValue(t, modelSchema, modelResourceModel{
		Name:               types.StringValue("whisper"),
		DeletionProtection: types.BoolValue(false),
	})}

	resp := &fwresource.ValidateConfigResponse{}
	r.ValidateConfig(t.Context(), fwresource.ValidateConfigRequest{Config: config}, resp)

	if resp.Diagnostics.HasError() {
		t.Fatalf("got errors %v, want warnings only", resp.Diagnostics)
	}
	if got := testDiagnosticsText(resp.Diagnostics.Warnings()); !strings.Contains(got, "does nothing without push") {
		t.Errorf("got warnings %q, want one saying the flag does nothing", got)
	}
}

func TestModelPushSourceHashesDirectory(t *testing.T) {
	dir := testModelDir(t, map[string]string{
		"config.yaml":    "model_name: whisper\n",
		"model/model.py": "print('hi')\n",
	})

	source, diags := modelPushResolveSource(t.Context(), modelPushModel{
		ConfigDir: types.StringValue(dir),
	}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving source: %v", diags)
	}
	defer source.Close()

	hashes, err := source.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing source: %v", err)
	}
	if _, ok := hashes.Files["config.yaml"]; !ok {
		t.Errorf("got files %v, want config.yaml among them", hashes.Files)
	}
	if _, ok := hashes.Files["model/model.py"]; !ok {
		t.Errorf("got files %v, want model/model.py among them", hashes.Files)
	}

	// Hashing again over the same bytes has to agree, or every plan would push.
	repeat, err := source.Hashes(t.Context())
	if err != nil {
		t.Fatalf("re-hashing source: %v", err)
	}
	if repeat.OverallHash() != hashes.OverallHash() {
		t.Errorf("got hash %q on the second walk, want the first %q", repeat.OverallHash(), hashes.OverallHash())
	}

	// Editing a file has to change the hash, or a push would never happen.
	if err := os.WriteFile(filepath.Join(dir, "model", "model.py"), []byte("print('bye')\n"), 0o600); err != nil {
		t.Fatalf("editing the model source: %v", err)
	}
	edited, err := source.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing edited source: %v", err)
	}
	if edited.OverallHash() == hashes.OverallHash() {
		t.Error("got the same hash after editing model/model.py, want a different one")
	}
	if summary := edited.ChangeSummary(hashes); !strings.Contains(summary, "model/model.py") {
		t.Errorf("got change summary %q, want it to name model/model.py", summary)
	}
}

// TestModelPushSourceHashesInlineConfig covers the inline path, which has no
// directory to walk: equal configurations must hash equal so a plan does not
// push, and the synthesized directory has to hold what the push builds from.
func TestModelPushSourceHashesInlineConfig(t *testing.T) {
	first, diags := modelPushResolveSource(t.Context(),
		modelPushModel{Config: testModelConfig(t, "whisper")}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving first source: %v", diags)
	}
	defer first.Close()

	second, diags := modelPushResolveSource(t.Context(),
		modelPushModel{Config: testModelConfig(t, "whisper")}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving second source: %v", diags)
	}
	defer second.Close()

	firstHashes, err := first.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing first source: %v", err)
	}
	secondHashes, err := second.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing second source: %v", err)
	}
	if firstHashes.OverallHash() != secondHashes.OverallHash() {
		t.Errorf("got hashes %q and %q for the same configuration, want them equal",
			firstHashes.OverallHash(), secondHashes.OverallHash())
	}

	changed, diags := modelPushResolveSource(t.Context(),
		modelPushModel{Config: testModelConfig(t, "whisper-v2")}, "whisper-v2")
	if diags.HasError() {
		t.Fatalf("resolving changed source: %v", diags)
	}
	defer changed.Close()
	changedHashes, err := changed.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing changed source: %v", err)
	}
	if changedHashes.OverallHash() == firstHashes.OverallHash() {
		t.Error("got the same hash for a different configuration, want a different one")
	}

	written, err := os.ReadFile(filepath.Join(first.options.Archive.Dir, modelPushConfigFileName))
	if err != nil {
		t.Fatalf("reading the synthesized config: %v", err)
	}
	if !strings.Contains(string(written), "model_name: whisper") {
		t.Errorf("got synthesized config %q, want it to carry model_name", written)
	}
}

// TestModelPushInlineAndDirectorySourcesDiffer settles what moving between the
// two ways of writing the same model does. It pushes, and it has to: the same
// configuration written in HCL and written as YAML are equal only in intent, and
// nothing here can prove the resulting model is the same one. What the plan must
// not do is call it an edit to config.yaml, since no file changed, so the
// inventory names an inline config as itself and the swap reads as a swap.
func TestModelPushInlineAndDirectorySourcesDiffer(t *testing.T) {
	dir := testModelDir(t, map[string]string{"config.yaml": "model_name: whisper\n"})

	directorySource, diags := modelPushResolveSource(t.Context(),
		modelPushModel{ConfigDir: types.StringValue(dir)}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving directory source: %v", diags)
	}
	defer directorySource.Close()

	inlineSource, diags := modelPushResolveSource(t.Context(),
		modelPushModel{Config: testModelConfig(t, "whisper")}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving inline source: %v", diags)
	}
	defer inlineSource.Close()

	directoryHashes, err := directorySource.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing directory source: %v", err)
	}
	inlineHashes, err := inlineSource.Hashes(t.Context())
	if err != nil {
		t.Fatalf("hashing inline source: %v", err)
	}

	if inlineHashes.OverallHash() == directoryHashes.OverallHash() {
		t.Error("got the same hash for an inline config and a directory, want different: " +
			"the provider cannot claim the two produce the same model")
	}
	if _, named := inlineHashes.Files[modelPushConfigFileName]; named {
		t.Errorf("got inventory %v, want it not to name config.yaml: no file changed",
			inlineHashes.Files)
	}
	summary := inlineHashes.ChangeSummary(directoryHashes)
	if !strings.Contains(summary, "added "+modelPushInlineConfigLabel) ||
		!strings.Contains(summary, "removed "+modelPushConfigFileName) {
		t.Errorf("got summary %q, want it to read as one source swapped for another", summary)
	}
}

// TestModelPushInlineSourceCleansUp covers the synthesized directory not being
// left behind, since one is written on every plan that hashes an inline config.
func TestModelPushInlineSourceCleansUp(t *testing.T) {
	source, diags := modelPushResolveSource(t.Context(),
		modelPushModel{Config: testModelConfig(t, "whisper")}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving source: %v", diags)
	}
	dir := source.options.Archive.Dir
	source.Close()

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("got the synthesized directory %s still present, want it removed", dir)
	}
}

// TestModelPushInlineConfigTakesModelName covers model_name being filled in from
// the resource, so an inline configuration never has to repeat it.
func TestModelPushInlineConfigTakesModelName(t *testing.T) {
	config := types.DynamicValue(types.ObjectValueMust(
		map[string]attr.Type{"python_version": types.StringType},
		map[string]attr.Value{"python_version": types.StringValue("py311")},
	))

	source, diags := modelPushResolveSource(t.Context(), modelPushModel{Config: config}, "whisper")
	if diags.HasError() {
		t.Fatalf("resolving source: %v", diags)
	}
	defer source.Close()

	if got := source.options.Config["model_name"]; got != "whisper" {
		t.Errorf("got model_name %v, want %q", got, "whisper")
	}
}

// TestModelPushRejectsDisagreeingModelName covers the resource owning identity:
// a configuration naming a different model is an error, not a silent rename.
func TestModelPushRejectsDisagreeingModelName(t *testing.T) {
	_, diags := modelPushResolveSource(t.Context(),
		modelPushModel{Config: testModelConfig(t, "something-else")}, "whisper")

	if !diags.HasError() {
		t.Fatal("got no error for a disagreeing model_name, want one")
	}
	if got := testDiagnosticsText(diags.Errors()); !strings.Contains(got, "disagrees") {
		t.Errorf("got errors %q, want one about the name disagreeing", got)
	}
}

// TestModelPushReadConfigDirExternalPackageDirs covers the coupling between the
// configuration and the archive: external dirs are inlined, so the configuration
// the build reads must no longer name them.
func TestModelPushReadConfigDirExternalPackageDirs(t *testing.T) {
	dir := testModelDir(t, map[string]string{
		"config.yaml": "model_name: whisper\nexternal_package_dirs:\n  - ../shared\n",
	})

	var options = testModelPushOptions(t, dir)

	if got := options.Archive.ExternalPackageDirs; len(got) != 1 || got[0] != "../shared" {
		t.Errorf("got external package dirs %v, want [../shared]", got)
	}
	if _, still := options.Config["external_package_dirs"]; still {
		t.Error("got external_package_dirs still in the parsed config, want it cleared")
	}
	if options.Archive.ConfigYAMLOverride == nil {
		t.Error("got no archived config override, want the cleared config")
	} else if strings.Contains(string(options.Archive.ConfigYAMLOverride), "external_package_dirs") {
		t.Errorf("got archived config %q, want it without external_package_dirs",
			options.Archive.ConfigYAMLOverride)
	}
	if options.Archive.BundledPackagesDir != modelPushDefaultBundledPkgDir {
		t.Errorf("got bundled packages dir %q, want the default %q",
			options.Archive.BundledPackagesDir, modelPushDefaultBundledPkgDir)
	}
	// RawConfig is stored verbatim, so it keeps what the parsed config dropped.
	if !strings.Contains(options.RawConfig, "external_package_dirs") {
		t.Errorf("got raw config %q, want it to keep external_package_dirs verbatim", options.RawConfig)
	}
}

func TestModelPushReadConfigDirBundledPackagesDir(t *testing.T) {
	dir := testModelDir(t, map[string]string{
		"config.yaml": "model_name: whisper\nbundled_packages_dir: vendor\n",
	})

	if got := testModelPushOptions(t, dir).Archive.BundledPackagesDir; got != "vendor" {
		t.Errorf("got bundled packages dir %q, want %q", got, "vendor")
	}
}

func TestModelPushDeploys(t *testing.T) {
	base := modelPushModel{
		SourceHash:  types.StringValue("hash-1"),
		Triggers:    types.MapNull(types.StringType),
		Environment: types.StringValue("production"),
	}

	tests := []struct {
		name    string
		mutate  func(*modelPushModel)
		deploys bool
	}{
		{name: "Unchanged", mutate: func(*modelPushModel) {}},
		{
			name:    "SourceHashChanged",
			mutate:  func(p *modelPushModel) { p.SourceHash = types.StringValue("hash-2") },
			deploys: true,
		},
		{
			name: "TriggersChanged",
			mutate: func(p *modelPushModel) {
				p.Triggers = types.MapValueMust(types.StringType, map[string]attr.Value{
					"base_image": types.StringValue("v2"),
				})
			},
			deploys: true,
		},
		// Everything below is an argument for the next push, so none of it deploys.
		{name: "EnvironmentChanged", mutate: func(p *modelPushModel) { p.Environment = types.StringValue("staging") }},
		{name: "LabelsChanged", mutate: func(p *modelPushModel) {
			p.Labels = types.MapValueMust(types.StringType, map[string]attr.Value{
				"team": types.StringValue("ml"),
			})
		}},
		{name: "DeploymentNameChanged", mutate: func(p *modelPushModel) {
			p.DeploymentName = types.StringValue("v2")
		}},
		{name: "DeployTimeoutChanged", mutate: func(p *modelPushModel) {
			p.DeployTimeoutMinutes = types.Int64Value(30)
		}},
		{name: "PreserveInstanceTypeChanged", mutate: func(p *modelPushModel) {
			p.PreserveEnvInstanceType = types.BoolValue(false)
		}},
		{name: "WaitChanged", mutate: func(p *modelPushModel) { p.Wait = types.BoolValue(true) }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			planned := base
			test.mutate(&planned)

			if got := modelPushDeploys(planned, base); got != test.deploys {
				t.Errorf("got deploys %v, want %v", got, test.deploys)
			}
		})
	}
}

func TestModelPushDeferredChanges(t *testing.T) {
	// Typed nulls, as the framework always produces: a zero-value map has no
	// element type and never compares equal, even to another zero value.
	prior := modelPushModel{
		Environment: types.StringValue("production"),
		Labels:      types.MapNull(types.StringType),
	}
	planned := modelPushModel{
		Environment:    types.StringValue("staging"),
		DeploymentName: types.StringValue("v2"),
		Labels:         types.MapNull(types.StringType),
	}

	got := modelPushDeferredChanges(planned, prior)
	want := []string{"deployment_name", "environment"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("got deferred %v, want %v", got, want)
	}
}

func TestModelPushDeploying(t *testing.T) {
	deploying := []managementapi.DeploymentStatus{
		managementapi.DeploymentStatus_BUILDING,
		managementapi.DeploymentStatus_DEPLOYING,
		managementapi.DeploymentStatus_LOADING_MODEL,
		managementapi.DeploymentStatus_UPDATING,
	}
	for _, status := range deploying {
		if !modelPushDeploying(status) {
			t.Errorf("got settled for %s, want still deploying", status)
		}
	}

	// Everything else settles, matching the CLI's --wait, so a scaled-to-zero or
	// unhealthy deployment ends the wait instead of polling forever.
	settled := []managementapi.DeploymentStatus{
		managementapi.DeploymentStatus_ACTIVE,
		managementapi.DeploymentStatus_BUILD_FAILED,
		managementapi.DeploymentStatus_SCALED_TO_ZERO,
		managementapi.DeploymentStatus_UNHEALTHY,
		managementapi.DeploymentStatus_INACTIVE,
	}
	for _, status := range settled {
		if modelPushDeploying(status) {
			t.Errorf("got still deploying for %s, want settled", status)
		}
	}
}

func TestModelPushValueToGo(t *testing.T) {
	tests := []struct {
		name  string
		value tftypes.Value
		want  any
	}{
		{name: "String", value: tftypes.NewValue(tftypes.String, "whisper"), want: "whisper"},
		{name: "Bool", value: tftypes.NewValue(tftypes.Bool, true), want: true},
		// Whole numbers stay whole so a replica count serializes as 3, not 3.0.
		{name: "WholeNumber", value: tftypes.NewValue(tftypes.Number, 3), want: int64(3)},
		{name: "FractionalNumber", value: tftypes.NewValue(tftypes.Number, 1.5), want: 1.5},
		{name: "Null", value: tftypes.NewValue(tftypes.String, nil), want: nil},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := modelPushValueToGo(test.value, tftypes.NewAttributePath())
			if err != nil {
				t.Fatalf("converting: %v", err)
			}
			if got != test.want {
				t.Errorf("got %#v, want %#v", got, test.want)
			}
		})
	}
}

// TestModelPushValueToGoNested covers the collection cases together, since a
// config is nested objects and lists all the way down.
func TestModelPushValueToGoNested(t *testing.T) {
	listType := tftypes.List{ElementType: tftypes.String}
	objectType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"model_name":            tftypes.String,
		"external_package_dirs": listType,
		"resources": tftypes.Object{AttributeTypes: map[string]tftypes.Type{
			"accelerator": tftypes.String,
			"count":       tftypes.Number,
		}},
	}}

	value := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"model_name": tftypes.NewValue(tftypes.String, "whisper"),
		"external_package_dirs": tftypes.NewValue(listType, []tftypes.Value{
			tftypes.NewValue(tftypes.String, "../shared"),
			tftypes.NewValue(tftypes.String, "../common"),
		}),
		"resources": tftypes.NewValue(objectType.AttributeTypes["resources"], map[string]tftypes.Value{
			"accelerator": tftypes.NewValue(tftypes.String, "A10G"),
			"count":       tftypes.NewValue(tftypes.Number, 2),
		}),
	})

	got, err := modelPushValueToGo(value, tftypes.NewAttributePath())
	if err != nil {
		t.Fatalf("converting: %v", err)
	}
	fields, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %T, want map[string]any", got)
	}
	if fields["model_name"] != "whisper" {
		t.Errorf("got model_name %v, want whisper", fields["model_name"])
	}
	// List order is meaningful and has to survive, unlike mapping key order.
	dirs, ok := fields["external_package_dirs"].([]any)
	if !ok || len(dirs) != 2 || dirs[0] != "../shared" || dirs[1] != "../common" {
		t.Errorf("got external_package_dirs %#v, want [../shared ../common] in order", fields["external_package_dirs"])
	}
	resources, ok := fields["resources"].(map[string]any)
	if !ok {
		t.Fatalf("got resources %T, want map[string]any", fields["resources"])
	}
	if resources["count"] != int64(2) {
		t.Errorf("got resources.count %#v, want int64(2)", resources["count"])
	}
}

// TestModelPushValueToGoRejectsUnknown covers a push never being attempted with
// an unresolved configuration, and the error naming the offending key.
func TestModelPushValueToGoRejectsUnknown(t *testing.T) {
	objectType := tftypes.Object{AttributeTypes: map[string]tftypes.Type{
		"model_name":     tftypes.String,
		"python_version": tftypes.String,
	}}
	value := tftypes.NewValue(objectType, map[string]tftypes.Value{
		"model_name":     tftypes.NewValue(tftypes.String, "whisper"),
		"python_version": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	})

	_, err := modelPushValueToGo(value, tftypes.NewAttributePath())
	if err == nil {
		t.Fatal("got no error for an unknown value, want one")
	}
	if !strings.Contains(err.Error(), "python_version") {
		t.Errorf("got error %q, want it to name python_version", err)
	}
}

func TestModelPushHashesOverallHashOrderIndependent(t *testing.T) {
	first := modelPushHashes{Files: map[string]string{"a": "1", "b": "2"}}
	second := modelPushHashes{Files: map[string]string{"b": "2", "a": "1"}}
	if first.OverallHash() != second.OverallHash() {
		t.Errorf("got %q and %q for the same files, want them equal", first.OverallHash(), second.OverallHash())
	}

	// Paths are length-delimited, so no rearrangement collides.
	shifted := modelPushHashes{Files: map[string]string{"ab": "1", "": "2"}}
	if shifted.OverallHash() == first.OverallHash() {
		t.Error("got the same hash for differently split paths, want them different")
	}
}

func TestModelPushHashesChangeSummary(t *testing.T) {
	prior := modelPushHashes{Files: map[string]string{"kept": "1", "edited": "1", "gone": "1"}}
	next := modelPushHashes{Files: map[string]string{"kept": "1", "edited": "2", "new": "1"}}

	got := next.ChangeSummary(prior)
	for _, want := range []string{"added new", "changed edited", "removed gone"} {
		if !strings.Contains(got, want) {
			t.Errorf("got summary %q, want it to contain %q", got, want)
		}
	}
	if next.ChangeSummary(next) != "" {
		t.Errorf("got summary %q for an unchanged inventory, want empty", next.ChangeSummary(next))
	}
}

// TestModelPushHashesChangeSummaryBounded covers a churning build directory not
// producing an unreadable wall of paths.
func TestModelPushHashesChangeSummaryBounded(t *testing.T) {
	next := modelPushHashes{Files: map[string]string{}}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		next.Files[name] = "1"
	}

	got := next.ChangeSummary(modelPushHashes{Files: map[string]string{}})
	if !strings.Contains(got, "and 2 more") {
		t.Errorf("got summary %q, want it to summarize the tail as \"and 2 more\"", got)
	}
}

// testModelDir writes a model directory, keyed by slash-separated path.
func testModelDir(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	for name, contents := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(contents), 0o600); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
	return dir
}

// testModelConfig is a minimal inline configuration naming a model.
func testModelConfig(t *testing.T, modelName string) types.Dynamic {
	t.Helper()

	return types.DynamicValue(types.ObjectValueMust(
		map[string]attr.Type{"model_name": types.StringType},
		map[string]attr.Value{"model_name": types.StringValue(modelName)},
	))
}

// testModelPushObject renders a push model as the object the schema carries,
// defaulting unset collections to typed nulls so they can convert.
func testModelPushObject(t *testing.T, push modelPushModel) types.Object {
	t.Helper()

	if push.Triggers.ElementType(t.Context()) == nil {
		push.Triggers = types.MapNull(types.StringType)
	}
	if push.Labels.ElementType(t.Context()) == nil {
		push.Labels = types.MapNull(types.StringType)
	}
	object, diags := types.ObjectValueFrom(t.Context(), modelPushObjectType().AttrTypes, &push)
	if diags.HasError() {
		t.Fatalf("building push object: %v", diags)
	}
	return object
}

// testModelPushOptions reads a model directory the way a push would.
func testModelPushOptions(t *testing.T, dir string) client.PushModelOptions {
	t.Helper()

	var options client.PushModelOptions
	if err := modelPushReadConfigDir(dir, &options); err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	return options
}

// testDiagnosticsText flattens diagnostics so a test can assert on the wording a
// user would read.
func testDiagnosticsText(diagnostics []diag.Diagnostic) string {
	rendered := make([]string, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		rendered = append(rendered, diagnostic.Summary()+": "+diagnostic.Detail())
	}
	return strings.Join(rendered, "\n")
}
