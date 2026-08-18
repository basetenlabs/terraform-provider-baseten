package provider

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscreds "github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/transfermanager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/basetenlabs/baseten-go/client"
	"github.com/basetenlabs/baseten-go/client/managementapi"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"gopkg.in/yaml.v3"
)

const (
	modelPushConfigFileName = "config.yaml"

	// modelPushDefaultBundledPkgDir is where external package dirs are inlined
	// when the config does not say otherwise, matching Baseten's default.
	modelPushDefaultBundledPkgDir = "packages"

	// modelPushPollInterval paces the wait for a deployment to settle. The
	// resource-level timeouts block bounds the whole wait.
	modelPushPollInterval = 5 * time.Second

	// modelPushWarmupTimeout is how long a just-created deployment is allowed to
	// read as missing before that is treated as a real error.
	modelPushWarmupTimeout = 30 * time.Second
)

// modelPushModel describes a push and its arguments rather than desired state,
// which is what lets the rule for when it runs fit in a sentence: a push happens
// when the model source changes or when Triggers changes, and nothing else causes
// one. Every other attribute is an argument the next push will use, the same way
// the Baseten CLI's flags apply to the push being run rather than to the
// deployment already serving.
//
// Notably Environment does not push. Changing it reads as a promotion to whoever
// writes it, and a promotion is not what a push does, so acting on it would
// surprise people in the more expensive direction.
type modelPushModel struct {
	ConfigDir               types.String  `tfsdk:"config_dir"`
	Config                  types.Dynamic `tfsdk:"config"`
	SourceHash              types.String  `tfsdk:"source_hash"`
	Triggers                types.Map     `tfsdk:"triggers"`
	Environment             types.String  `tfsdk:"environment"`
	DeploymentName          types.String  `tfsdk:"deployment_name"`
	Labels                  types.Map     `tfsdk:"labels"`
	Wait                    types.Bool    `tfsdk:"wait"`
	DeployTimeoutMinutes    types.Int64   `tfsdk:"deploy_timeout_minutes"`
	PreserveEnvInstanceType types.Bool    `tfsdk:"preserve_env_instance_type"`
}

func modelPushSchemaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"config_dir": schema.StringAttribute{
			MarkdownDescription: "Path to the model directory to push, containing at least a `config.yaml`. " +
				"Set this or `config`. **Editing any file it uploads pushes a new deployment**, unless " +
				"`source_hash` is set. A `.truss_ignore` file in the directory decides what is uploaded.",
			Optional: true,
		},
		"config": schema.DynamicAttribute{
			MarkdownDescription: "Model configuration written inline, accepting any field `config.yaml` " +
				"accepts. Set this or `config_dir`. **Editing it pushes a new deployment**, unless `source_hash` " +
				"is set. Shorthand for a directory holding only this configuration, so it suits models built " +
				"from configuration alone and cannot carry model code. `model_name` is filled in from the " +
				"resource, and comments are not preserved. See the " +
				"[configuration reference](https://docs.baseten.co/reference/truss-configuration).",
			Optional: true,
		},
		"source_hash": schema.StringAttribute{
			MarkdownDescription: "How the provider recognizes that the source changed. Left unset it is " +
				"computed: from the files `config_dir` uploads, or from the canonical form of an inline `config`. " +
				"Hashing a directory reads all of it on every plan, so set this to skip that on a large one, " +
				"supplying any value that changes when the source does, such as a commit SHA.\n\n" +
				"~> This overrides *how the source is identified*; it is not a way to force a push. Taking it " +
				"over means the source is no longer read, and removing it later resumes hashing, which pushes " +
				"once. Use `triggers` to force a push.",
			Optional: true,
			Computed: true,
		},
		"triggers": schema.MapAttribute{
			MarkdownDescription: "Arbitrary values that push a new deployment when any of them changes, even " +
				"though the source did not. Use it to redeploy identical code, for instance because a base image " +
				"moved or a secret rotated. Keys are yours to choose and record why: " +
				"`triggers = { base_image = \"v2\" }`.\n\n" +
				"Unlike taking over `source_hash`, this composes with automatic hashing, so bumping a trigger " +
				"forces one push and leaves the source still tracked.",
			Optional:    true,
			ElementType: types.StringType,
		},
		"environment": schema.StringAttribute{
			MarkdownDescription: "Environment the next push deploys into, created if it does not exist.\n\n" +
				"Changing this does not push, and does not move the deployment already running. It applies to " +
				"the next push, the same way `--environment` applies to `baseten model push`. To move what is " +
				"deployed now, promote it outside Terraform.",
			Optional: true,
		},
		"wait": schema.BoolAttribute{
			MarkdownDescription: "Whether to wait for a pushed deployment to finish deploying, failing the " +
				"apply if it does not become active. Defaults to false, which returns as soon as the deployment " +
				"is created, so a later build failure surfaces as drift rather than an apply error. The " +
				"resource-level `timeouts` block bounds the wait.\n\n" +
				"Changing this does not push. It applies to the next push.",
			Optional: true,
		},
		"deployment_name": schema.StringAttribute{
			MarkdownDescription: "Name for the deployment a push creates. Names are unique per model and a " +
				"push never removes the deployment it replaces, so a literal name can be pushed only once. " +
				"Leave unset to let Baseten assign `deployment-N`.\n\n" +
				"Changing this does not push, and does not rename the deployment already running. It applies to " +
				"the next push.",
			Optional: true,
		},
		"labels": schema.MapAttribute{
			MarkdownDescription: "Labels for the deployment a push creates.\n\n" +
				"Changing this does not push, and does not relabel the deployment already running. It applies " +
				"to the next push.",
			Optional:    true,
			ElementType: types.StringType,
		},
		"deploy_timeout_minutes": schema.Int64Attribute{
			MarkdownDescription: "How long Baseten allows the deployment a push creates to take, from 10 to " +
				"1440 minutes. Leave unset to use Baseten's own default. This bounds the deploy itself, not how " +
				"long Terraform waits, which is the resource-level `timeouts` block.\n\n" +
				"Changing this does not push. It applies to the next push.",
			Optional: true,
		},
		"preserve_env_instance_type": schema.BoolAttribute{
			MarkdownDescription: "Whether a push into `environment` keeps that environment's current instance " +
				"type instead of the one in the configuration. Defaults to true at Baseten.\n\n" +
				"Changing this does not push. It applies to the next push.",
			Optional: true,
		},
	}
}

func modelPushObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: attrTypesOf(modelPushSchemaAttributes())}
}

func modelPushPath(attribute string) path.Path {
	return path.Root("push").AtName(attribute)
}

// modelPushDeploys reports whether the difference between two pushes deploys
// anything. Only the source's identity and the explicit triggers do.
//
// planned.SourceHash has to be settled first: an unknown hash compares unequal to
// everything, which pushes, and that is the safe answer but not a decision this
// should make by accident.
func modelPushDeploys(planned, prior modelPushModel) bool {
	return !planned.SourceHash.Equal(prior.SourceHash) || !planned.Triggers.Equal(prior.Triggers)
}

// modelPushDeferredChanges names the arguments that changed without a push to
// carry them, so a plan never silently drops an edit.
func modelPushDeferredChanges(planned, prior modelPushModel) []string {
	changed := map[string]bool{
		"deploy_timeout_minutes":     !planned.DeployTimeoutMinutes.Equal(prior.DeployTimeoutMinutes),
		"deployment_name":            !planned.DeploymentName.Equal(prior.DeploymentName),
		"environment":                !planned.Environment.Equal(prior.Environment),
		"labels":                     !planned.Labels.Equal(prior.Labels),
		"preserve_env_instance_type": !planned.PreserveEnvInstanceType.Equal(prior.PreserveEnvInstanceType),
	}
	deferred := make([]string, 0, len(changed))
	for name, differs := range changed {
		if differs {
			deferred = append(deferred, name)
		}
	}
	sort.Strings(deferred)
	return deferred
}

// modelPushSource is a resolved, pushable model source: the options baseten-go
// needs, plus what this provider must remember to hash it and to clean up after
// an inline configuration.
type modelPushSource struct {
	options client.PushModelOptions

	// inlineHashes is set for an inline configuration, which has no directory to
	// walk. It is taken before model_name is filled in, so the hash describes
	// what the user wrote and does not depend on resolving the model.
	inlineHashes *modelPushHashes

	// removeSynthesizedDir deletes the directory written for an inline
	// configuration.
	removeSynthesizedDir func()
}

// modelPushResolveSource reads the configuration and prepares everything needed
// to hash or push it. modelName fills in the configuration's model_name, and may
// be empty when only the hash is wanted, since the hash never depends on it.
func modelPushResolveSource(ctx context.Context, push modelPushModel, modelName string) (*modelPushSource, diag.Diagnostics) {
	var diags diag.Diagnostics
	source := &modelPushSource{}

	switch {
	case modelIsSet(push.ConfigDir):
		if err := modelPushReadConfigDir(push.ConfigDir.ValueString(), &source.options); err != nil {
			diags.AddAttributeError(modelPushPath("config_dir"), "Unable to read the model configuration", err.Error())
			return nil, diags
		}
	case modelIsSet(push.Config):
		config, configDiags := modelPushDynamicToConfig(ctx, push.Config)
		diags.Append(configDiags...)
		if diags.HasError() {
			return nil, diags
		}
		source.options.Config = config

		hashes, err := modelPushInlineHashes(config)
		if err != nil {
			diags.AddAttributeError(modelPushPath("config"), "Unable to hash the model configuration", err.Error())
			return nil, diags
		}
		source.inlineHashes = &hashes
	default:
		diags.AddError(
			"Missing model configuration",
			"Set push.config_dir or push.config so the provider knows what to deploy.",
		)
		return nil, diags
	}

	// The resource owns the model's identity, so a configuration disagreeing
	// with it is an error rather than a silent rename, which Baseten cannot do.
	if existing, ok := source.options.Config["model_name"].(string); ok && existing != "" && existing != modelName {
		if modelName != "" {
			diags.AddError(
				"Model name disagrees with the configuration",
				fmt.Sprintf("The resource manages the model %q but the configuration names %q. Baseten cannot "+
					"rename a model, so remove model_name from the configuration or point the resource at %q.",
					modelName, existing, existing),
			)
			return nil, diags
		}
	} else if modelName != "" {
		source.options.Config["model_name"] = modelName
	}

	// An inline configuration has no directory, and baseten-go requires one even
	// for formats built from the configuration alone, so write it out and archive
	// that. The path is an implementation detail that can appear in archive
	// errors.
	if source.inlineHashes != nil {
		dir, err := os.MkdirTemp("", "baseten-model-config-*")
		if err != nil {
			diags.AddAttributeError(modelPushPath("config"), "Unable to prepare the inline configuration", err.Error())
			return nil, diags
		}
		source.removeSynthesizedDir = func() { _ = os.RemoveAll(dir) }

		encoded, err := yaml.Marshal(source.options.Config)
		if err == nil {
			err = os.WriteFile(filepath.Join(dir, modelPushConfigFileName), encoded, 0o600)
		}
		if err != nil {
			source.Close()
			diags.AddAttributeError(modelPushPath("config"), "Unable to prepare the inline configuration", err.Error())
			return nil, diags
		}
		source.options.Archive.Dir = dir
	}

	source.options.DeploymentName = push.DeploymentName.ValueString()
	source.options.EnvironmentName = push.Environment.ValueString()
	if modelIsSet(push.PreserveEnvInstanceType) {
		source.options.OverrideEnvInstanceType = !push.PreserveEnvInstanceType.ValueBool()
	}
	if modelIsSet(push.DeployTimeoutMinutes) {
		source.options.DeployTimeoutMinutes = int(push.DeployTimeoutMinutes.ValueInt64())
	}
	if modelIsSet(push.Labels) {
		labels := map[string]string{}
		diags.Append(push.Labels.ElementsAs(ctx, &labels, false)...)
		if diags.HasError() {
			source.Close()
			return nil, diags
		}
		// Baseten types label values as arbitrary JSON; the schema restricts them
		// to strings, which is what a label is.
		source.options.Labels = make(map[string]any, len(labels))
		for key, value := range labels {
			source.options.Labels[key] = value
		}
	}

	return source, diags
}

func (s *modelPushSource) Close() {
	if s.removeSynthesizedDir != nil {
		s.removeSynthesizedDir()
	}
}

// Hashes describes the source as the provider recognizes it, walking the model
// directory or reusing the inline configuration's hash.
func (s *modelPushSource) Hashes(ctx context.Context) (modelPushHashes, error) {
	if s.inlineHashes != nil {
		return *s.inlineHashes, nil
	}
	return modelPushWalkHashes(ctx, s.options.Archive)
}

// Push deploys the source, creating the model when modelID is empty, and waits
// for the deployment to settle when asked.
func (s *modelPushSource) Push(
	ctx context.Context,
	managementClient *client.ManagementClient,
	modelID, teamID string,
	wait bool,
) (*client.PushModelResult, diag.Diagnostics) {
	var diags diag.Diagnostics

	options := s.options
	options.ModelID = modelID
	if modelID == "" {
		options.TeamID = teamID
	}
	options.ModelUploader = modelPushUpload

	result, err := managementClient.PushModel(ctx, options)
	if err != nil {
		diags.AddError("Unable to push the Baseten model", err.Error())
		return nil, diags
	}
	if !wait {
		return result, diags
	}

	deployment, waitDiags := modelPushWaitForDeployment(ctx, managementClient, result.Model.Id, result.Deployment.Id)
	diags.Append(waitDiags...)
	if deployment != nil {
		result.Deployment = deployment
	}
	return result, diags
}

// Validate asks Baseten to check the configuration without creating anything, so
// a bad configuration fails the plan rather than the apply. It also builds and
// discards the archive, which is the only way to catch the ignore rules and
// external package dirs the server cannot see.
func (s *modelPushSource) Validate(
	ctx context.Context,
	managementClient *client.ManagementClient,
	modelID, teamID string,
) diag.Diagnostics {
	var diags diag.Diagnostics

	options := s.options
	options.ModelID = modelID
	if modelID == "" {
		options.TeamID = teamID
	}
	options.DryRun = true

	if _, err := managementClient.PushModel(ctx, options); err != nil {
		diags.AddAttributeError(path.Root("push"), "Baseten rejected the model configuration", err.Error())
	}
	return diags
}

// modelPushDeploying reports whether a deployment is still on its way to a
// terminal status. Every other status is settled, matching the Baseten CLI's
// --wait so both tools agree on when a push has finished.
func modelPushDeploying(status managementapi.DeploymentStatus) bool {
	switch status {
	case managementapi.DeploymentStatus_BUILDING,
		managementapi.DeploymentStatus_DEPLOYING,
		managementapi.DeploymentStatus_LOADING_MODEL,
		managementapi.DeploymentStatus_UPDATING:
		return true
	default:
		return false
	}
}

// modelPushWaitForDeployment polls until the deployment settles, failing on
// anything but ACTIVE. The caller's context carries the resource timeout, whose
// expiry ends the wait without cancelling the deployment.
func modelPushWaitForDeployment(
	ctx context.Context,
	managementClient *client.ManagementClient,
	modelID, deploymentID string,
) (*managementapi.Deployment, diag.Diagnostics) {
	var diags diag.Diagnostics

	ticker := time.NewTicker(modelPushPollInterval)
	defer ticker.Stop()

	warmupDeadline := time.Now().Add(modelPushWarmupTimeout)
	warmedUp := false
	var lastSeen *managementapi.Deployment

	for {
		deployment, err := managementClient.API().GetModelsDeploymentsDeploymentId(ctx, modelID, deploymentID)
		switch {
		case err == nil:
			warmedUp = true
			lastSeen = deployment
			if !modelPushDeploying(deployment.Status) {
				if deployment.Status != managementapi.DeploymentStatus_ACTIVE {
					diags.AddError(
						"The pushed deployment did not become active",
						fmt.Sprintf("Deployment %s settled in status %s. It still exists, so nothing is rolled "+
							"back and the next plan reads the model as it now stands.",
							deployment.Id, deployment.Status),
					)
				}
				return deployment, diags
			}
		// A deployment Baseten has only just created can read as missing for a
		// few seconds. Retry that one case, and only until the first successful
		// read, so a genuine 404 later still fails.
		case !warmedUp && modelIsNotFound(err) && time.Now().Before(warmupDeadline):
		default:
			diags.AddError("Unable to read the pushed Baseten deployment", err.Error())
			return lastSeen, diags
		}

		select {
		case <-ctx.Done():
			diags.AddError(
				"Timed out waiting for the pushed deployment",
				fmt.Sprintf("Deployment %s is still deploying at Baseten and keeps going; only the wait "+
					"stopped. Read it on the next plan, raise the create or update timeout, or set "+
					"push.wait = false.", deploymentID),
			)
			return lastSeen, diags
		case <-ticker.C:
		}
	}
}

// modelPushUpload streams the archive to the location Baseten issued credentials
// for. baseten-go leaves this to the caller so it takes no AWS dependency.
func modelPushUpload(ctx context.Context, upload client.ModelUpload) error {
	awsConfig := aws.Config{
		Region: upload.Region,
		Credentials: awscreds.NewStaticCredentialsProvider(
			upload.AccessKeyID, upload.SecretAccessKey, upload.SessionToken),
	}
	uploader := transfermanager.New(s3.NewFromConfig(awsConfig))
	_, err := uploader.UploadObject(ctx, &transfermanager.UploadObjectInput{
		Bucket: &upload.Bucket,
		Key:    &upload.Key,
		Body:   upload.Body,
	})
	return err
}

// modelPushReadConfigDir loads config.yaml from dir into options: the parsed
// configuration, the verbatim text, and the archive's package-dir options.
func modelPushReadConfigDir(dir string, options *client.PushModelOptions) error {
	configPath := filepath.Join(dir, modelPushConfigFileName)
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}

	if err := yaml.Unmarshal(raw, &options.Config); err != nil {
		return fmt.Errorf("parse %s: %w", configPath, err)
	}
	if options.Config == nil {
		options.Config = map[string]any{}
	}
	// Kept verbatim for display and download. Baseten never builds from it, so
	// it keeps comments and formatting the parsed configuration has lost.
	options.RawConfig = string(raw)
	options.Archive.Dir = dir

	if externalDirs, ok := options.Config["external_package_dirs"].([]any); ok {
		for _, entry := range externalDirs {
			if asString, ok := entry.(string); ok {
				options.Archive.ExternalPackageDirs = append(options.Archive.ExternalPackageDirs, asString)
			}
		}
		// The external contents are inlined under the bundled packages dir, so
		// the configuration the build reads must not still name them. Baseten
		// rejects relative paths the extracted archive lacks.
		delete(options.Config, "external_package_dirs")
		cleared, err := yaml.Marshal(options.Config)
		if err != nil {
			return fmt.Errorf("re-serialize %s without external_package_dirs: %w", configPath, err)
		}
		options.Archive.ConfigYAMLOverride = cleared
	}
	if bundled, ok := options.Config["bundled_packages_dir"].(string); ok && bundled != "" {
		options.Archive.BundledPackagesDir = bundled
	} else {
		options.Archive.BundledPackagesDir = modelPushDefaultBundledPkgDir
	}
	return nil
}

// modelPushDynamicToConfig converts an inline configuration for the wire. The
// stored value stays exactly as it was planned; this is one-way, since
// round-tripping through Go collections changes the Terraform type and fails the
// apply with an inconsistent-result error.
func modelPushDynamicToConfig(ctx context.Context, config types.Dynamic) (map[string]any, diag.Diagnostics) {
	var diags diag.Diagnostics

	raw, err := config.UnderlyingValue().ToTerraformValue(ctx)
	if err != nil {
		diags.AddAttributeError(modelPushPath("config"), "Unable to read the model configuration", err.Error())
		return nil, diags
	}
	converted, err := modelPushValueToGo(raw, tftypes.NewAttributePath())
	if err != nil {
		diags.AddAttributeError(modelPushPath("config"), "Unable to convert the model configuration", err.Error())
		return nil, diags
	}
	fields, ok := converted.(map[string]any)
	if !ok {
		diags.AddAttributeError(
			modelPushPath("config"),
			"Model configuration must be an object",
			fmt.Sprintf("push.config has to be an object of configuration fields, got %T.", converted),
		)
		return nil, diags
	}
	return fields, diags
}

// modelPushValueToGo lowers a Terraform value to the plain Go value the API
// takes, following the same shape as terraform-provider-kubernetes's
// payload.FromTFValue. Going through tftypes rather than the framework's types
// means collections collapse into two cases instead of five, and the attribute
// path makes an error name the offending key.
//
// Unknown values are rejected: a push only happens once everything is resolved.
func modelPushValueToGo(value tftypes.Value, at *tftypes.AttributePath) (any, error) {
	if !value.IsKnown() {
		return nil, at.NewErrorf("value is not known yet")
	}
	if value.IsNull() {
		return nil, nil
	}

	valueType := value.Type()
	switch {
	case valueType.Is(tftypes.Bool):
		var converted bool
		err := value.As(&converted)
		return converted, err

	case valueType.Is(tftypes.String):
		var converted string
		err := value.As(&converted)
		return converted, err

	// Whole numbers stay whole, so a replica count serializes as 3 rather than
	// 3.0 and the configuration Baseten sees matches what was written.
	case valueType.Is(tftypes.Number):
		var number *big.Float
		if err := value.As(&number); err != nil {
			return nil, err
		}
		if number.IsInt() {
			if whole, accuracy := number.Int64(); accuracy == big.Exact {
				return whole, nil
			}
		}
		converted, _ := number.Float64()
		return converted, nil

	case valueType.Is(tftypes.List{}), valueType.Is(tftypes.Set{}), valueType.Is(tftypes.Tuple{}):
		var elements []tftypes.Value
		if err := value.As(&elements); err != nil {
			return nil, err
		}
		converted := make([]any, 0, len(elements))
		for index, element := range elements {
			lowered, err := modelPushValueToGo(element, at.WithElementKeyInt(index))
			if err != nil {
				return nil, err
			}
			converted = append(converted, lowered)
		}
		return converted, nil

	case valueType.Is(tftypes.Map{}), valueType.Is(tftypes.Object{}):
		var elements map[string]tftypes.Value
		if err := value.As(&elements); err != nil {
			return nil, err
		}
		converted := make(map[string]any, len(elements))
		for name, element := range elements {
			lowered, err := modelPushValueToGo(element, at.WithElementKeyString(name))
			if err != nil {
				return nil, err
			}
			converted[name] = lowered
		}
		return converted, nil

	default:
		return nil, at.NewErrorf("cannot convert a %s to a configuration value", valueType)
	}
}
