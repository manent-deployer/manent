package lambda

import (
	"context"
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	"github.com/aws/aws-sdk-go-v2/service/lambda/types"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
)

// SDKConnector apre l'accesso a Lambda con l'SDK AWS.
type SDKConnector struct {
	// Endpoint, se indicato, sostituisce quello di Lambda (es. LocalStack).
	Endpoint *string
}

// Connect costruisce il client Lambda per la region con le credenziali della richiesta.
func (c SDKConnector) Connect(ctx context.Context, region string, req *plugin.Request) (API, error) {
	cfg, err := awsauth.Config(ctx, region, req)
	if err != nil {
		return nil, err
	}
	return sdkAPI{lambda.NewFromConfig(cfg, func(o *lambda.Options) { o.BaseEndpoint = c.Endpoint })}, nil
}

type sdkAPI struct{ c *lambda.Client }

// notFound traduce l'errore di risorsa inesistente in ErrNotFound.
func notFound(err error) error {
	var nf *types.ResourceNotFoundException
	if errors.As(err, &nf) {
		return ErrNotFound
	}
	return err
}

func convert(c *types.FunctionConfiguration) *Function {
	return &Function{
		Name: aws.ToString(c.FunctionName), PackageType: string(c.PackageType),
		State: string(c.State), StateReason: aws.ToString(c.StateReason),
		LastUpdateStatus: string(c.LastUpdateStatus), LastUpdateReason: aws.ToString(c.LastUpdateStatusReason),
		CodeSha256: aws.ToString(c.CodeSha256), Version: aws.ToString(c.Version), Description: aws.ToString(c.Description),
	}
}

func (a sdkAPI) GetFunction(ctx context.Context, name, qualifier string) (*Function, error) {
	in := &lambda.GetFunctionConfigurationInput{FunctionName: aws.String(name)}
	if qualifier != "" {
		in.Qualifier = aws.String(qualifier)
	}
	out, err := a.c.GetFunctionConfiguration(ctx, in)
	if err != nil {
		return nil, notFound(err)
	}
	return &Function{
		Name: aws.ToString(out.FunctionName), PackageType: string(out.PackageType),
		State: string(out.State), StateReason: aws.ToString(out.StateReason),
		LastUpdateStatus: string(out.LastUpdateStatus), LastUpdateReason: aws.ToString(out.LastUpdateStatusReason),
		CodeSha256: aws.ToString(out.CodeSha256), Version: aws.ToString(out.Version), Description: aws.ToString(out.Description),
	}, nil
}

func (a sdkAPI) UpdateCode(ctx context.Context, name string, code Code, dryRun bool) (*Function, error) {
	in := &lambda.UpdateFunctionCodeInput{FunctionName: aws.String(name), DryRun: dryRun, ZipFile: code.ZipFile}
	if code.ImageURI != "" {
		in.ImageUri = aws.String(code.ImageURI)
	}
	out, err := a.c.UpdateFunctionCode(ctx, in)
	if err != nil {
		return nil, notFound(err)
	}
	return &Function{
		Name: aws.ToString(out.FunctionName), PackageType: string(out.PackageType), State: string(out.State),
		LastUpdateStatus: string(out.LastUpdateStatus), CodeSha256: aws.ToString(out.CodeSha256), Version: aws.ToString(out.Version),
	}, nil
}

func (a sdkAPI) PublishVersion(ctx context.Context, name, codeSha256, description string) (*Function, error) {
	out, err := a.c.PublishVersion(ctx, &lambda.PublishVersionInput{
		FunctionName: aws.String(name), CodeSha256: aws.String(codeSha256), Description: aws.String(description),
	})
	if err != nil {
		return nil, notFound(err)
	}
	return &Function{
		Name: aws.ToString(out.FunctionName), PackageType: string(out.PackageType), State: string(out.State),
		LastUpdateStatus: string(out.LastUpdateStatus), CodeSha256: aws.ToString(out.CodeSha256),
		Version: aws.ToString(out.Version), Description: aws.ToString(out.Description),
	}, nil
}

func (a sdkAPI) GetAlias(ctx context.Context, name, alias string) (*Alias, error) {
	out, err := a.c.GetAlias(ctx, &lambda.GetAliasInput{FunctionName: aws.String(name), Name: aws.String(alias)})
	if err != nil {
		return nil, notFound(err)
	}
	res := &Alias{Name: aws.ToString(out.Name), FunctionVersion: aws.ToString(out.FunctionVersion), Description: aws.ToString(out.Description)}
	if out.RoutingConfig != nil && len(out.RoutingConfig.AdditionalVersionWeights) > 0 {
		res.Weights = out.RoutingConfig.AdditionalVersionWeights
	}
	return res, nil
}

func (a sdkAPI) PutAlias(ctx context.Context, name, alias, version, description string, create bool) error {
	if create {
		_, err := a.c.CreateAlias(ctx, &lambda.CreateAliasInput{
			FunctionName: aws.String(name), Name: aws.String(alias), FunctionVersion: aws.String(version), Description: aws.String(description),
		})
		var exists *types.ResourceConflictException
		if err == nil || !errors.As(err, &exists) {
			return err
		}
		// Creato nel frattempo (es. da un tentativo precedente): si aggiorna.
	}
	_, err := a.c.UpdateAlias(ctx, &lambda.UpdateAliasInput{
		FunctionName: aws.String(name), Name: aws.String(alias), FunctionVersion: aws.String(version), Description: aws.String(description),
		RoutingConfig: &types.AliasRoutingConfiguration{AdditionalVersionWeights: map[string]float64{}},
	})
	return notFound(err)
}

func (a sdkAPI) RouteAlias(ctx context.Context, name, alias, version, description string, weights map[string]float64) error {
	_, err := a.c.UpdateAlias(ctx, &lambda.UpdateAliasInput{
		FunctionName: aws.String(name), Name: aws.String(alias), FunctionVersion: aws.String(version), Description: aws.String(description),
		RoutingConfig: &types.AliasRoutingConfiguration{AdditionalVersionWeights: weights},
	})
	return notFound(err)
}

func (a sdkAPI) ListVersions(ctx context.Context, name string) ([]Function, error) {
	var list []Function
	p := lambda.NewListVersionsByFunctionPaginator(a.c, &lambda.ListVersionsByFunctionInput{FunctionName: aws.String(name)})
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, notFound(err)
		}
		for i := range out.Versions {
			list = append(list, *convert(&out.Versions[i]))
		}
	}
	return list, nil
}
