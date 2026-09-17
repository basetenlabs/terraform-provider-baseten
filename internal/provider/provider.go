package provider

import (
	"context"
	"net/http"
	"os"

	"github.com/basetenlabs/baseten-go/client"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

const (
	apiKeyEnvVar    = "BASETEN_API_KEY"
	remoteURLEnvVar = "BASETEN_REMOTE_URL"
)

var _ provider.Provider = &basetenProvider{}

// basetenProvider implements the Baseten Terraform provider.
type basetenProvider struct {
	version string
}

// basetenProviderModel is the provider configuration block.
type basetenProviderModel struct {
	APIKey    types.String `tfsdk:"api_key"`
	RemoteURL types.String `tfsdk:"remote_url"`
}

// New returns a function that constructs the provider for the given version.
func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &basetenProvider{version: version}
	}
}

func (p *basetenProvider) Metadata(ctx context.Context, req provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "baseten"
	resp.Version = p.version
}

func (p *basetenProvider) Schema(ctx context.Context, req provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages Baseten models, deployments, secrets, and API keys.\n\n" +
			"Set the API key with the `BASETEN_API_KEY` environment variable, or from a variable your " +
			"secret manager supplies. A key written into a Terraform configuration ends up in version " +
			"control, and one passed as an argument is also recorded in plan files.",
		Attributes: map[string]schema.Attribute{
			"api_key": schema.StringAttribute{
				MarkdownDescription: "Baseten API key. Defaults to the `BASETEN_API_KEY` environment variable.",
				Optional:            true,
				Sensitive:           true,
			},
			"remote_url": schema.StringAttribute{
				MarkdownDescription: "Base URL of the Baseten API. Defaults to the `BASETEN_REMOTE_URL` environment " +
					"variable, then to `https://api.baseten.co`.",
				Optional: true,
			},
		},
	}
}

func (p *basetenProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config basetenProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The client is built once here, so a value only known after another
	// resource applies cannot be resolved.
	if config.APIKey.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Unknown API key",
			"The provider cannot be configured with an api_key that is not known until apply.",
		)
	}
	if config.RemoteURL.IsUnknown() {
		resp.Diagnostics.AddAttributeError(
			path.Root("remote_url"),
			"Unknown remote URL",
			"The provider cannot be configured with a remote_url that is not known until apply.",
		)
	}
	if resp.Diagnostics.HasError() {
		return
	}

	apiKey := config.APIKey.ValueString()
	if apiKey == "" {
		apiKey = os.Getenv(apiKeyEnvVar)
	}
	if apiKey == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Missing API key",
			"Set the api_key provider attribute or the "+apiKeyEnvVar+" environment variable.",
		)
		return
	}

	remoteURL := config.RemoteURL.ValueString()
	if remoteURL == "" {
		remoteURL = os.Getenv(remoteURLEnvVar)
	}

	headers := http.Header{}
	headers.Set("User-Agent", "terraform-provider-baseten/"+p.version)

	managementClient, err := client.NewManagementClient(client.ManagementClientOptions{
		APIKey:  apiKey,
		BaseURL: remoteURL,
		Headers: headers,
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to create Baseten API client", err.Error())
		return
	}

	resp.ResourceData = managementClient
	resp.DataSourceData = managementClient
}

func (p *basetenProvider) Resources(ctx context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		newAPIKeyResource,
		newModelResource,
		newSecretResource,
	}
}

func (p *basetenProvider) DataSources(ctx context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		newModelDataSource,
	}
}
