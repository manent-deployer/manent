// Package ecs releases a container image to an Amazon ECS service (Fargate or EC2): it
// registers a new revision of the task definition in service with only the image changed,
// updates the service and waits for the ECS deployment to complete.
//
// The artifact is a registry image (source oci://registry/repository:tag), always run pinned
// to the digest. Service, cluster, network, roles and the rest of the task definition belong to
// whoever manages the infrastructure (Terraform, CloudFormation): the plugin changes only the
// image and writes version and deploy in the revision tags, from which it reads the release in
// service and goes back to the previous one. Every target is a region, with the cluster.
package ecs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/internal/awsauth"
	"github.com/manent-deployer/manent/plugins/internal/pluginutil"
)

// Name is the name the plugin registers with.
const Name = "ecs"

// Descriptor parameters (deploy.params) read by the plugin, besides the awsauth credential ones.
const (
	ParamCluster        = "cluster"         // ECS cluster (default "default"); the target can give it as region/cluster
	ParamService        = "service"         // ECS service (default: application name)
	ParamContainer      = "container"       // container whose image changes (default: the only one, or the one named after the application)
	ParamWaitTimeout    = "wait_timeout"    // deployment wait (default 15m)
	ParamSearchRevision = "search_revision" // revisions examined to find a version's one (default 100)
	defaultCluster      = "default"
	defaultWaitTimeout  = 15 * time.Minute
	defaultSearch       = 100
)

// Tags written on the task definition revision.
const (
	TagApplication = "manent.application"
	TagEnvironment = "manent.environment"
	TagVersion     = "manent.version"
	TagDeployment  = "manent.deployment"
)

// DetailTaskDefinition is the prefix of the Release.Details keys with the release task
// definition on each target ("task_definition@<target>").
const DetailTaskDefinition = "task_definition"

// ErrNotFound means a service or task definition that does not exist.
var ErrNotFound = errors.New("not found")

// Service is the state of an ECS service.
type Service struct {
	Name           string
	Status         string
	TaskDefinition string
	DesiredCount   int32
	RunningCount   int32
	// Controller is the deployment controller type: ECS, CODE_DEPLOY or EXTERNAL.
	Controller  string
	Deployments []Deployment
	// Strategy is the native strategy of the ECS controller: ROLLING (default), BLUE_GREEN,
	// LINEAR or CANARY. With the last three ECS shifts traffic by itself (delegated mode).
	Strategy string
	// Native are the native strategy times.
	Native NativeStrategy
}

// NativeStrategy are the times of an ECS native strategy, to estimate its duration.
type NativeStrategy struct {
	// BakeMinutes is the wait after all traffic has moved to the new revision.
	BakeMinutes int32
	// StepPercent and StepBakeMinutes are step and wait of the LINEAR strategy.
	StepPercent     float64
	StepBakeMinutes int32
	// CanaryPercent and CanaryBakeMinutes are share and wait of the CANARY strategy.
	CanaryPercent     float64
	CanaryBakeMinutes int32
}

// nativeDuration estimates how long a native strategy takes to shift traffic.
func (s *Service) nativeDuration() time.Duration {
	n := s.Native
	minutes := float64(n.BakeMinutes)
	switch s.Strategy {
	case "LINEAR":
		if n.StepPercent > 0 {
			minutes += math.Ceil(100/n.StepPercent) * float64(n.StepBakeMinutes)
		}
	case "CANARY":
		minutes += float64(n.CanaryBakeMinutes)
	}
	return time.Duration(minutes * float64(time.Minute))
}

// delegated reports a native strategy that shifts traffic (blue/green, linear, canary).
func (s *Service) delegated() bool {
	return s.Strategy == "BLUE_GREEN" || s.Strategy == "LINEAR" || s.Strategy == "CANARY"
}

// Deployment is a running or ended ECS deployment on the service.
type Deployment struct {
	ID             string
	Status         string // PRIMARY, ACTIVE, INACTIVE
	TaskDefinition string
	RolloutState   string // IN_PROGRESS, COMPLETED, FAILED
	RolloutReason  string
	Desired        int32
	Running        int32
	Pending        int32
	Failed         int32
}

// TaskDefinition is a task definition revision. Base keeps the full definition read from the
// API, from which the client registers the next revision.
type TaskDefinition struct {
	ARN        string
	Family     string
	Revision   int32
	Containers []Container
	Tags       map[string]string
	Base       any
}

// Container is a container of the task definition.
type Container struct {
	Name  string
	Image string
}

// API are the ECS operations used by the plugin, in a region.
type API interface {
	DescribeService(ctx context.Context, cluster, service string) (*Service, error)
	DescribeTaskDefinition(ctx context.Context, arn string) (*TaskDefinition, error)
	// RegisterRevision registers a new revision of base with the container image changed and the
	// tags added to the existing ones.
	RegisterRevision(ctx context.Context, base *TaskDefinition, container, image string, tags map[string]string) (*TaskDefinition, error)
	UpdateService(ctx context.Context, cluster, service, taskDefinition string) error
	// ListRevisions returns the ARNs of the family's active revisions, most recent first.
	ListRevisions(ctx context.Context, family string, limit int) ([]string, error)
}

// Connector opens access to ECS in a region with the request's credentials.
type Connector interface {
	Connect(ctx context.Context, region string, req *plugin.Request) (API, error)
}

// Plugin implements plugin.Deployer, DryRunner, ReleaseInspector and Rollbacker.
type Plugin struct {
	connector Connector
	types     []string
	logger    *slog.Logger
	poll      time.Duration
}

// New creates the plugin with the given connector and supported artifact types.
func New(c Connector, types []string, logger *slog.Logger) *Plugin {
	return &Plugin{connector: c, types: types, logger: logger, poll: 5 * time.Second}
}

// Factory builds the plugin from the application configuration:
//   - endpoint: alternative ECS API endpoint (e.g. LocalStack)
//   - artifact_types: supported types (default ["ecs"])
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	types, err := pluginutil.ArtifactTypes(cfg, Name)
	if err != nil {
		return nil, err
	}
	endpoint, err := awsauth.Endpoint(cfg)
	if err != nil {
		return nil, err
	}
	return New(SDKConnector{Endpoint: endpoint}, types, logger), nil
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return Name }

// Supports reports whether the artifact type is handled.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// target is a parsed target: region and cluster.
type target struct {
	host    string
	region  string
	cluster string
}

// parseTarget parses the target host: "region" or "region/cluster".
func parseTarget(t plugin.Target, req *plugin.Request) (target, error) {
	region, cluster, ok := strings.Cut(t.Host, "/")
	if !ok || cluster == "" {
		cluster = req.Params[ParamCluster]
	}
	if cluster == "" {
		cluster = defaultCluster
	}
	if !awsauth.ValidRegion(region) {
		return target{}, fmt.Errorf("target %q: expected region or region/cluster (e.g. eu-west-1/prod)", t.Host)
	}
	return target{host: t.Host, region: region, cluster: cluster}, nil
}

func serviceName(req *plugin.Request) string {
	if s := req.Params[ParamService]; s != "" {
		return s
	}
	return req.Application
}

// Validate checks parameters and targets before any connection.
func (p *Plugin) Validate(req *plugin.Request) error {
	var errs []error
	if !pluginutil.VersionRe.MatchString(req.Artifact.Version) {
		errs = append(errs, errors.New("artifact.version is required and becomes a tag of the task definition: letters, digits, '.', '_', '+', '-' are allowed"))
	}
	if req.Artifact.LocalPath != "" && !strings.HasSuffix(req.Artifact.LocalPath, plugin.ImageRefExt) {
		errs = append(errs, errors.New("the artifact is not a registry image: provide artifact.source oci://registry/repository:tag"))
	}
	if serviceName(req) == "" {
		errs = append(errs, fmt.Errorf("%s is required", ParamService))
	}
	if _, err := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout); err != nil {
		errs = append(errs, err)
	}
	if _, err := searchLimit(req); err != nil {
		errs = append(errs, err)
	}
	if err := awsauth.Validate(req.Params); err != nil {
		errs = append(errs, err)
	}
	if len(req.Targets) == 0 {
		errs = append(errs, errors.New("no region among the targets"))
	}
	for _, t := range req.Targets {
		if _, err := parseTarget(t, req); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func searchLimit(req *plugin.Request) (int, error) {
	raw := req.Params[ParamSearchRevision]
	if raw == "" {
		return defaultSearch, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", ParamSearchRevision)
	}
	return n, nil
}

// chooseContainer chooses the container whose image changes.
func chooseContainer(td *TaskDefinition, req *plugin.Request) (string, error) {
	names := make([]string, len(td.Containers))
	for i, c := range td.Containers {
		names[i] = c.Name
	}
	want := req.Params[ParamContainer]
	switch {
	case want != "":
	case len(names) == 1:
		return names[0], nil
	default:
		want = req.Application
	}
	if slices.Contains(names, want) {
		return want, nil
	}
	return "", fmt.Errorf("container %q missing from task definition %s (containers: %s): provide %q", want, td.ARN, strings.Join(names, ", "), ParamContainer)
}

func (td *TaskDefinition) image(container string) string {
	for _, c := range td.Containers {
		if c.Name == container {
			return c.Image
		}
	}
	return ""
}

// state is the state of a target read before deploy, dry-run and rollback.
type state struct {
	target    target
	api       API
	service   *Service
	current   *TaskDefinition
	container string
}

// read opens access to the region and reads service and task definition in service.
func (p *Plugin) read(ctx context.Context, t plugin.Target, req *plugin.Request) (*state, error) {
	tg, err := parseTarget(t, req)
	if err != nil {
		return nil, err
	}
	api, err := p.connector.Connect(ctx, tg.region, req)
	if err != nil {
		return nil, err
	}
	svc, err := api.DescribeService(ctx, tg.cluster, serviceName(req))
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("service %s missing from cluster %s: it must be created with the infrastructure (e.g. Terraform), manent updates its image", serviceName(req), tg.cluster)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the service: %w", err)
	}
	if svc.Status != "" && svc.Status != "ACTIVE" {
		return nil, fmt.Errorf("service %s in state %s", svc.Name, svc.Status)
	}
	if svc.Controller != "" && svc.Controller != "ECS" {
		return nil, fmt.Errorf("service %s with deployment controller %s: the plugin handles only the ECS controller", svc.Name, svc.Controller)
	}
	td, err := api.DescribeTaskDefinition(ctx, svc.TaskDefinition)
	if err != nil {
		return nil, fmt.Errorf("reading task definition %s: %w", svc.TaskDefinition, err)
	}
	container, err := chooseContainer(td, req)
	if err != nil {
		return nil, err
	}
	return &state{target: tg, api: api, service: svc, current: td, container: container}, nil
}

func (s *state) details(req *plugin.Request) map[string]string {
	d := map[string]string{"region": s.target.region, "cluster": s.target.cluster, "service": serviceName(req), "container": s.container}
	if v := s.current.Tags[TagVersion]; v != "" {
		d["previous"] = v
	}
	return d
}

// Deploy releases the image to every target, req.Parallelism at a time; it stops at the first
// error. It is idempotent: if the service already uses the revision registered by this deploy,
// it only waits for completion.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	image, err := plugin.ImageReference(req.Artifact)
	if err != nil {
		return nil, err
	}
	targets, err := plugin.ForEachTarget(ctx, req, func(ctx context.Context, t plugin.Target) (plugin.TargetResult, error) {
		details, err := p.deployTarget(ctx, t, req, image)
		if err != nil {
			return plugin.TargetResult{Status: plugin.StatusFailed, Details: details}, fmt.Errorf("target %s: %w", t.Host, err)
		}
		return plugin.TargetResult{Status: plugin.StatusDeployed, Details: details}, nil
	})
	return &plugin.Result{Plugin: Name, Targets: targets}, err
}

// ParallelTargets implements plugin.ParallelDeployer: every target (region and cluster) has its
// own client and service, so most of the deploy is the wait for the ECS rollout.
func (p *Plugin) ParallelTargets() bool { return true }

func (p *Plugin) deployTarget(ctx context.Context, t plugin.Target, req *plugin.Request, image string) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.target.region, "cluster", s.target.cluster, "service", serviceName(req))
	td := s.current
	if td.Tags[TagDeployment] != req.DeploymentID || td.image(s.container) != image {
		tags := map[string]string{
			TagApplication: req.Application, TagEnvironment: req.Environment,
			TagVersion: req.Artifact.Version, TagDeployment: req.DeploymentID,
		}
		if td, err = s.api.RegisterRevision(ctx, s.current, s.container, image, tags); err != nil {
			return details, fmt.Errorf("registering the task definition: %w", err)
		}
		log.InfoContext(ctx, "task definition registered", "task_definition", td.ARN, "image", image)
		if err := s.api.UpdateService(ctx, s.target.cluster, serviceName(req), td.ARN); err != nil {
			return details, fmt.Errorf("updating the service: %w", err)
		}
	}
	details[DetailTaskDefinition] = td.ARN
	if s.service.delegated() {
		// Delegated mode: ECS handles steps, bake and the return on CloudWatch alarms. manent waits
		// for the end with a time covering the expected duration.
		details["strategy"] = s.service.Strategy
		log.InfoContext(ctx, "native ECS strategy: the traffic shift is delegated to the service", "strategy", s.service.Strategy, "expected", s.service.nativeDuration())
	}
	if err := p.wait(ctx, s, td.ARN, req, log); err != nil {
		return details, err
	}
	log.InfoContext(ctx, "active release", "version", req.Artifact.Version, "task_definition", td.ARN)
	return details, nil
}

// wait waits until the service deployment with task definition arn completes.
// A FAILED deployment (ECS circuit breaker) is an immediate error.
func (p *Plugin) wait(ctx context.Context, s *state, arn string, req *plugin.Request, log *slog.Logger) error {
	timeout, _ := pluginutil.Duration(req.Params, ParamWaitTimeout, defaultWaitTimeout)
	if s.service != nil && s.service.delegated() {
		timeout += s.service.nativeDuration()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	last := ""
	for {
		svc, err := s.api.DescribeService(ctx, s.target.cluster, serviceName(req))
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("deployment not completed within %s (%s)", timeout, last)
			}
			return fmt.Errorf("reading the service: %w", err)
		}
		done, st, err := completed(svc, arn)
		if err != nil {
			return err
		}
		if done {
			log.InfoContext(ctx, "deployment completed", "task_definition", arn, "running", svc.RunningCount)
			return nil
		}
		if st != last {
			log.InfoContext(ctx, "deployment in progress", "state", st)
			last = st
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("deployment not completed within %s (%s)", timeout, last)
		case <-time.After(p.poll):
		}
	}
}

// completed says whether the deployment of task definition arn ended successfully.
func completed(svc *Service, arn string) (bool, string, error) {
	var d *Deployment
	for i := range svc.Deployments {
		if svc.Deployments[i].TaskDefinition == arn && svc.Deployments[i].Status == "PRIMARY" {
			d = &svc.Deployments[i]
		}
	}
	if d == nil {
		if svc.TaskDefinition != arn {
			return false, "", fmt.Errorf("the service now uses %s: deployment replaced by another update", svc.TaskDefinition)
		}
		return false, "waiting for the deployment", nil
	}
	progress := fmt.Sprintf("%d/%d tasks running, %d starting, %d failed", d.Running, d.Desired, d.Pending, d.Failed)
	switch d.RolloutState {
	case "FAILED":
		return false, "", fmt.Errorf("deployment failed: %s (%s)", d.RolloutReason, progress)
	case "COMPLETED":
		return true, progress, nil
	case "":
		// Services without rollout state: completed when only the primary deployment remains with
		// all tasks running.
		if len(svc.Deployments) == 1 && d.Running == d.Desired {
			return true, progress, nil
		}
	}
	if old := len(svc.Deployments) - 1; old > 0 {
		progress += fmt.Sprintf(", %d previous deployments stopping", old)
	}
	return false, progress, nil
}

// CurrentRelease reads on every target the version from the tags of the task definition in
// service. A service whose task definition lacks the tag (never released by manent) has no
// release; different versions across targets are an error.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	found := map[string][]string{}
	details := map[string]string{"source": "task definition tags"}
	for _, t := range req.Targets {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return nil, fmt.Errorf("target %s: %w", t.Host, err)
		}
		v := s.current.Tags[TagVersion]
		if v == "" {
			continue
		}
		found[v] = append(found[v], t.Host)
		details[DetailTaskDefinition+"@"+t.Host] = s.current.ARN
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v := range found {
			return &plugin.Release{Version: v, Details: details}, nil
		}
	}
	var desc []string
	for v, hosts := range found {
		desc = append(desc, v+" on "+strings.Join(hosts, ","))
	}
	sort.Strings(desc)
	return nil, fmt.Errorf("different versions on the targets: %s", strings.Join(desc, "; "))
}

// Rollback brings every service back to the task definition of the given release: the one
// recorded in the release details or, failing that, the family's most recent one with the
// version tag. It continues even if a target fails, req.Parallelism targets at a time. It is
// idempotent.
func (p *Plugin) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	if !pluginutil.VersionRe.MatchString(previous.Version) {
		return nil, fmt.Errorf("invalid version %q", previous.Version)
	}
	targets, err := plugin.ForAllTargets(ctx, req, func(ctx context.Context, t plugin.Target) (plugin.TargetResult, error) {
		details, err := p.rollbackTarget(ctx, t, req, previous)
		if err != nil {
			return plugin.TargetResult{Status: plugin.StatusFailed, Details: details}, fmt.Errorf("target %s: %w", t.Host, err)
		}
		return plugin.TargetResult{Status: plugin.StatusRolledBack, Details: details}, nil
	})
	return &plugin.Result{Plugin: Name, Targets: targets}, err
}

func (p *Plugin) rollbackTarget(ctx context.Context, t plugin.Target, req *plugin.Request, previous plugin.Release) (map[string]string, error) {
	s, err := p.read(ctx, t, req)
	if err != nil {
		return nil, err
	}
	details := s.details(req)
	log := p.logger.With("deployment", req.DeploymentID, "region", s.target.region, "cluster", s.target.cluster, "service", serviceName(req), "rollback", true)
	arn := previous.Details[DetailTaskDefinition+"@"+t.Host]
	if arn == "" {
		if arn, err = p.findRevision(ctx, s, req, previous.Version); err != nil {
			return details, err
		}
	}
	details[DetailTaskDefinition] = arn
	if s.service.TaskDefinition != arn {
		if err := s.api.UpdateService(ctx, s.target.cluster, serviceName(req), arn); err != nil {
			return details, fmt.Errorf("updating the service: %w", err)
		}
	}
	if err := p.wait(ctx, s, arn, req, log); err != nil {
		return details, err
	}
	log.WarnContext(ctx, "previous release restored", "version", previous.Version, "task_definition", arn)
	return details, nil
}

// findRevision looks, among the family's most recent revisions, for the one with the version tag.
func (p *Plugin) findRevision(ctx context.Context, s *state, req *plugin.Request, version string) (string, error) {
	limit, _ := searchLimit(req)
	arns, err := s.api.ListRevisions(ctx, s.current.Family, limit)
	if err != nil {
		return "", fmt.Errorf("listing the revisions of %s: %w", s.current.Family, err)
	}
	for _, arn := range arns {
		td, err := s.api.DescribeTaskDefinition(ctx, arn)
		if err != nil {
			return "", fmt.Errorf("reading task definition %s: %w", arn, err)
		}
		if td.Tags[TagVersion] == version && td.Tags[TagApplication] == req.Application && td.Tags[TagEnvironment] == req.Environment {
			return arn, nil
		}
	}
	return "", fmt.Errorf("no revision of %s with %s=%s among the last %d", s.current.Family, TagVersion, version, limit)
}

// DryRun checks on every target, without changing it, that service, task definition and
// container exist and that the credentials can read them.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	targets, err := plugin.ForAllTargets(ctx, req, func(ctx context.Context, t plugin.Target) (plugin.TargetResult, error) {
		s, err := p.read(ctx, t, req)
		if err != nil {
			return plugin.TargetResult{Status: plugin.StatusFailed}, fmt.Errorf("target %s: %w", t.Host, err)
		}
		details := s.details(req)
		details[DetailTaskDefinition] = s.current.ARN
		if image, err := plugin.ImageReference(req.Artifact); err == nil {
			details["image"] = image
		}
		details["running"] = fmt.Sprintf("%d/%d", s.service.RunningCount, s.service.DesiredCount)
		p.logger.InfoContext(ctx, "service ready for the deploy", "deployment", req.DeploymentID, "region", s.target.region, "service", serviceName(req), "dry_run", true)
		return plugin.TargetResult{Status: plugin.StatusReady, Details: details}, nil
	})
	return &plugin.Result{Plugin: Name, Targets: targets}, err
}
