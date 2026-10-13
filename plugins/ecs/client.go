package ecs

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ecs"
	"github.com/aws/aws-sdk-go-v2/service/ecs/types"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
)

// SDKConnector apre l'accesso a ECS con l'SDK AWS.
type SDKConnector struct {
	// Endpoint, se indicato, sostituisce quello di ECS (es. LocalStack).
	Endpoint *string
}

// Connect costruisce il client ECS per la region con le credenziali della richiesta.
func (c SDKConnector) Connect(ctx context.Context, region string, req *plugin.Request) (API, error) {
	cfg, err := awsauth.Config(ctx, region, req)
	if err != nil {
		return nil, err
	}
	return sdkAPI{ecs.NewFromConfig(cfg, func(o *ecs.Options) { o.BaseEndpoint = c.Endpoint })}, nil
}

type sdkAPI struct{ c *ecs.Client }

func (a sdkAPI) DescribeService(ctx context.Context, cluster, service string) (*Service, error) {
	out, err := a.c.DescribeServices(ctx, &ecs.DescribeServicesInput{Cluster: aws.String(cluster), Services: []string{service}})
	if err != nil {
		var nf *types.ClusterNotFoundException
		if errors.As(err, &nf) {
			return nil, fmt.Errorf("cluster %s %w", cluster, ErrNotFound)
		}
		return nil, err
	}
	for _, f := range out.Failures {
		if aws.ToString(f.Reason) == "MISSING" {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("%s: %s", aws.ToString(f.Reason), aws.ToString(f.Detail))
	}
	if len(out.Services) == 0 {
		return nil, ErrNotFound
	}
	s := out.Services[0]
	svc := &Service{
		Name: aws.ToString(s.ServiceName), Status: aws.ToString(s.Status), TaskDefinition: aws.ToString(s.TaskDefinition),
		DesiredCount: s.DesiredCount, RunningCount: s.RunningCount,
	}
	if s.DeploymentController != nil {
		svc.Controller = string(s.DeploymentController.Type)
	}
	for _, d := range s.Deployments {
		svc.Deployments = append(svc.Deployments, Deployment{
			ID: aws.ToString(d.Id), Status: aws.ToString(d.Status), TaskDefinition: aws.ToString(d.TaskDefinition),
			RolloutState: string(d.RolloutState), RolloutReason: aws.ToString(d.RolloutStateReason),
			Desired: d.DesiredCount, Running: d.RunningCount, Pending: d.PendingCount, Failed: d.FailedTasks,
		})
	}
	return svc, nil
}

func (a sdkAPI) DescribeTaskDefinition(ctx context.Context, arn string) (*TaskDefinition, error) {
	out, err := a.c.DescribeTaskDefinition(ctx, &ecs.DescribeTaskDefinitionInput{
		TaskDefinition: aws.String(arn), Include: []types.TaskDefinitionField{types.TaskDefinitionFieldTags},
	})
	if err != nil {
		return nil, err
	}
	return convert(out.TaskDefinition, out.Tags), nil
}

func convert(td *types.TaskDefinition, tags []types.Tag) *TaskDefinition {
	out := &TaskDefinition{
		ARN: aws.ToString(td.TaskDefinitionArn), Family: aws.ToString(td.Family), Revision: td.Revision,
		Tags: map[string]string{}, Base: td,
	}
	for _, c := range td.ContainerDefinitions {
		out.Containers = append(out.Containers, Container{Name: aws.ToString(c.Name), Image: aws.ToString(c.Image)})
	}
	for _, t := range tags {
		out.Tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return out
}

// RegisterRevision copia nella nuova revisione tutti i campi registrabili della base: cambia
// solo l'immagine del container indicato.
func (a sdkAPI) RegisterRevision(ctx context.Context, base *TaskDefinition, container, image string, tags map[string]string) (*TaskDefinition, error) {
	td, ok := base.Base.(*types.TaskDefinition)
	if !ok {
		return nil, errors.New("task definition di base non letta dall'API")
	}
	defs := slices.Clone(td.ContainerDefinitions)
	for i := range defs {
		if aws.ToString(defs[i].Name) == container {
			defs[i].Image = aws.String(image)
		}
	}
	all := maps.Clone(base.Tags)
	maps.Copy(all, tags)
	var tagList []types.Tag
	for _, k := range slices.Sorted(maps.Keys(all)) {
		if strings.HasPrefix(k, "aws:") {
			// I tag riservati di AWS non si possono scrivere.
			continue
		}
		tagList = append(tagList, types.Tag{Key: aws.String(k), Value: aws.String(all[k])})
	}
	out, err := a.c.RegisterTaskDefinition(ctx, &ecs.RegisterTaskDefinitionInput{
		Family:                  td.Family,
		ContainerDefinitions:    defs,
		Cpu:                     td.Cpu,
		Memory:                  td.Memory,
		EnableFaultInjection:    td.EnableFaultInjection,
		EphemeralStorage:        td.EphemeralStorage,
		ExecutionRoleArn:        td.ExecutionRoleArn,
		TaskRoleArn:             td.TaskRoleArn,
		InferenceAccelerators:   td.InferenceAccelerators,
		IpcMode:                 td.IpcMode,
		PidMode:                 td.PidMode,
		NetworkMode:             td.NetworkMode,
		PlacementConstraints:    td.PlacementConstraints,
		ProxyConfiguration:      td.ProxyConfiguration,
		RequiresCompatibilities: td.RequiresCompatibilities,
		RuntimePlatform:         td.RuntimePlatform,
		Volumes:                 td.Volumes,
		Tags:                    tagList,
	})
	if err != nil {
		return nil, err
	}
	return convert(out.TaskDefinition, out.Tags), nil
}

func (a sdkAPI) UpdateService(ctx context.Context, cluster, service, taskDefinition string) error {
	_, err := a.c.UpdateService(ctx, &ecs.UpdateServiceInput{
		Cluster: aws.String(cluster), Service: aws.String(service), TaskDefinition: aws.String(taskDefinition),
	})
	return err
}

func (a sdkAPI) ListRevisions(ctx context.Context, family string, limit int) ([]string, error) {
	var arns []string
	p := ecs.NewListTaskDefinitionsPaginator(a.c, &ecs.ListTaskDefinitionsInput{
		FamilyPrefix: aws.String(family), Sort: types.SortOrderDesc, Status: types.TaskDefinitionStatusActive,
	})
	for p.HasMorePages() && len(arns) < limit {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, arn := range out.TaskDefinitionArns {
			// FamilyPrefix è un prefisso: "shop" troverebbe anche "shop-api".
			if strings.HasSuffix(arn[:strings.LastIndexByte(arn, ':')], "/"+family) && len(arns) < limit {
				arns = append(arns, arn)
			}
		}
	}
	return arns, nil
}
