// Package external lets a deploy plugin run as a separate process, shipped as a binary
// independent of manent. The transport is gRPC through github.com/hashicorp/go-plugin.
//
// On the plugin side a main that passes to Serve the same plugin.Factory used for a plugin
// compiled into manent is enough:
//
//	func main() { external.Serve(myplugin.Factory) }
//
// On the manent side the process is started and managed by internal/extplugin.
package external

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joeg-ita/manent/pkg/plugin"
	pb "github.com/joeg-ita/manent/pkg/plugin/external/proto"
)

// ProtocolVersion is the host-plugin protocol version. Increment it at every incompatible
// change of deployer.proto: go-plugin rejects plugins with a different version.
const ProtocolVersion = 1

// Handshake is shared by host and plugin. The cookie prevents running the plugin binary
// directly by mistake: it is not a security measure.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "MANENT_PLUGIN",
	MagicCookieValue: "7d1f0c5e-deployer",
}

// PluginName is the name the deployer is exposed with in the go-plugin PluginSet.
const PluginName = "deployer"

// GRPCPlugin implements goplugin.GRPCPlugin for both sides: Factory is used by the plugin,
// while the host gets a *Client.
type GRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Factory plugin.Factory
}

// GRPCServer registers the Deployer service in the plugin process.
func (p *GRPCPlugin) GRPCServer(broker *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterDeployerServer(s, &server{factory: p.Factory, broker: broker})
	return nil
}

// GRPCClient returns the client used by manent.
func (p *GRPCPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &Client{rpc: pb.NewDeployerClient(c), broker: broker}, nil
}

// PluginSet returns the plugin set to pass to go-plugin.
func PluginSet(f plugin.Factory) goplugin.PluginSet {
	return goplugin.PluginSet{PluginName: &GRPCPlugin{Factory: f}}
}

// Optional capabilities declared by the plugin in Configure.
const (
	CapabilityDryRun         = "dry_run"
	CapabilityCurrentRelease = "current_release"
	CapabilityRollback       = "rollback"
	CapabilitySlots          = "slots"
	CapabilityTrafficRouter  = "traffic_router"
	CapabilityTesters        = "testers"
	CapabilityScale          = "scale"
)

// capabilities returns the optional interfaces implemented by the deployer.
func capabilities(d plugin.Deployer) []string {
	var caps []string
	if _, ok := d.(plugin.DryRunner); ok {
		caps = append(caps, CapabilityDryRun)
	}
	if _, ok := d.(plugin.ReleaseInspector); ok {
		caps = append(caps, CapabilityCurrentRelease)
	}
	if _, ok := d.(plugin.Rollbacker); ok {
		caps = append(caps, CapabilityRollback)
	}
	if _, ok := d.(plugin.SlotDeployer); ok {
		caps = append(caps, CapabilitySlots)
	}
	if _, ok := d.(plugin.TrafficRouter); ok {
		caps = append(caps, CapabilityTrafficRouter)
	}
	if _, ok := d.(plugin.TesterRouter); ok {
		caps = append(caps, CapabilityTesters)
	}
	if _, ok := d.(plugin.SlotScaler); ok {
		caps = append(caps, CapabilityScale)
	}
	return caps
}

// Client is the host side of the protocol: it exposes the plugin process as a plugin.Deployer.
type Client struct {
	rpc    pb.DeployerClient
	broker *goplugin.GRPCBroker
	name   string
	caps   []string
}

// Configure sends the configuration to the plugin and records its name and capabilities.
func (c *Client) Configure(ctx context.Context, configJSON []byte) (string, error) {
	resp, err := c.rpc.Configure(ctx, &pb.ConfigureRequest{ConfigJson: configJSON})
	if err != nil {
		return "", fromStatus(err)
	}
	c.name, c.caps = resp.GetName(), resp.GetCapabilities()
	return c.name, nil
}

// Has reports whether the plugin declared the given capability.
func (c *Client) Has(capability string) bool { return slices.Contains(c.caps, capability) }

// CurrentRelease reads the release in service through the plugin; it returns an error if the
// plugin does not implement plugin.ReleaseInspector.
func (c *Client) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	var rel *plugin.Release
	_, err := c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.CurrentRelease(ctx, r)
		if status.Code(err) == codes.Unimplemented {
			return nil, fmt.Errorf("plugin %q cannot read the release in service: use another locator", c.name)
		}
		if err != nil {
			return nil, err
		}
		rel = fromProtoRelease(resp.GetRelease())
		return &pb.DeployResponse{}, nil
	})
	return rel, err
}

// Rollback brings the targets back to the given release; it returns
// plugin.ErrRollbackUnsupported if the plugin does not implement plugin.Rollbacker.
func (c *Client) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.Rollback(ctx, &pb.RollbackRequest{Request: r, Previous: toProtoRelease(&previous)})
		if status.Code(err) == codes.Unimplemented {
			return nil, plugin.ErrRollbackUnsupported
		}
		return resp, err
	})
}

// TrafficSupported implements plugin.TrafficChecker with the capabilities declared in Configure.
func (c *Client) TrafficSupported(context.Context) (plugin.TrafficSupport, error) {
	return plugin.TrafficSupport{Slots: c.Has(CapabilitySlots), Router: c.Has(CapabilityTrafficRouter),
		Testers: c.Has(CapabilityTesters), Scale: c.Has(CapabilityScale)}, nil
}

// unsupported maps UNIMPLEMENTED to plugin.ErrTrafficUnsupported.
func unsupported(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return plugin.ErrTrafficUnsupported
	}
	return err
}

// empty runs a call without a result with the secrets served.
func (c *Client) empty(req *plugin.Request, call func(*pb.DeployRequest) error) error {
	_, err := c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		return &pb.DeployResponse{}, unsupported(call(r))
	})
	return err
}

// PrepareSlots implements plugin.SlotDeployer.
func (c *Client) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	var state map[string]string
	err := c.empty(req, func(r *pb.DeployRequest) error {
		resp, err := c.rpc.PrepareSlots(ctx, r)
		state = resp.GetState()
		return err
	})
	return state, err
}

// DeployCandidate implements plugin.SlotDeployer.
func (c *Client) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.DeployCandidate(ctx, r)
		return resp, unsupported(err)
	})
}

// PromoteCandidate implements plugin.SlotDeployer.
func (c *Client) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	return c.empty(req, func(r *pb.DeployRequest) error { _, err := c.rpc.PromoteCandidate(ctx, r); return err })
}

// RevertCandidate implements plugin.SlotDeployer.
func (c *Client) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	return c.empty(req, func(r *pb.DeployRequest) error { _, err := c.rpc.RevertCandidate(ctx, r); return err })
}

// RetirePrevious implements plugin.SlotDeployer.
func (c *Client) RetirePrevious(ctx context.Context, req *plugin.Request) error {
	return c.empty(req, func(r *pb.DeployRequest) error { _, err := c.rpc.RetirePrevious(ctx, r); return err })
}

// SetCandidateWeight implements plugin.TrafficRouter.
func (c *Client) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	return c.empty(req, func(r *pb.DeployRequest) error {
		_, err := c.rpc.SetCandidateWeight(ctx, &pb.WeightRequest{Request: r, Percent: int32(percent)})
		return err
	})
}

// CandidateWeight implements plugin.TrafficRouter.
func (c *Client) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	w := 0
	err := c.empty(req, func(r *pb.DeployRequest) error {
		resp, err := c.rpc.CandidateWeight(ctx, r)
		w = int(resp.GetPercent())
		return err
	})
	return w, err
}

// RouteTesters implements plugin.TesterRouter.
func (c *Client) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	var details map[string]string
	err := c.empty(req, func(r *pb.DeployRequest) error {
		resp, err := c.rpc.RouteTesters(ctx, r)
		details = resp.GetDetails()
		return err
	})
	return details, err
}

// ScaleCandidate implements plugin.SlotScaler.
func (c *Client) ScaleCandidate(ctx context.Context, req *plugin.Request, percent int) error {
	return c.empty(req, func(r *pb.DeployRequest) error {
		_, err := c.rpc.ScaleCandidate(ctx, &pb.WeightRequest{Request: r, Percent: int32(percent)})
		return err
	})
}

// Name returns the name declared by the plugin in Configure.
func (c *Client) Name() string { return c.name }

// shortCallTimeout limits calls without a caller context (Supports, Validate), which must have
// no effects and not last long.
const shortCallTimeout = 30 * time.Second

// Supports asks the plugin; a communication error counts as "not supported".
func (c *Client) Supports(artifactType string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := c.rpc.Supports(ctx, &pb.SupportsRequest{ArtifactType: artifactType})
	return err == nil && resp.GetSupported()
}

// Validate validates the request in the plugin, without the secret resolver.
func (c *Client) Validate(req *plugin.Request) error {
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	_, err := c.rpc.Validate(ctx, toProtoRequest(req, 0))
	return fromStatus(err)
}

// Deploy runs the deploy in the plugin, serving the secrets for the duration of the call.
func (c *Client) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		return c.rpc.Deploy(ctx, r)
	})
}

// DryRun runs the dry-run in the plugin; it returns plugin.ErrDryRunUnsupported if the plugin
// does not implement plugin.DryRunner.
func (c *Client) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.DryRun(ctx, r)
		if status.Code(err) == codes.Unimplemented {
			return nil, plugin.ErrDryRunUnsupported
		}
		return resp, err
	})
}

// withSecrets serves the secret resolver on the go-plugin broker, runs the call and stops the
// server: the plugin can read secrets only during this call.
func (c *Client) withSecrets(req *plugin.Request, call func(*pb.DeployRequest) (*pb.DeployResponse, error)) (*plugin.Result, error) {
	var id uint32
	if req.Secrets != nil {
		id = c.broker.NextId()
		var (
			mu     sync.Mutex
			srv    *grpc.Server
			closed bool
		)
		go c.broker.AcceptAndServe(id, func(opts []grpc.ServerOption) *grpc.Server {
			s := grpc.NewServer(opts...)
			pb.RegisterSecretsServer(s, &secretsServer{resolver: req.Secrets})
			mu.Lock()
			defer mu.Unlock()
			if closed {
				// The call has already ended: the server must not keep listening.
				go s.Stop()
			}
			srv = s
			return s
		})
		defer func() {
			mu.Lock()
			defer mu.Unlock()
			closed = true
			if srv != nil {
				srv.Stop()
			}
		}()
	}
	resp, err := call(toProtoRequest(req, id))
	if errors.Is(err, plugin.ErrTrafficUnsupported) {
		return nil, err
	}
	if err != nil {
		return nil, fromStatus(err)
	}
	return fromProtoResult(resp), nil
}

// secretsServer exposes the request's SecretResolver to the plugin process.
type secretsServer struct {
	pb.UnimplementedSecretsServer
	resolver plugin.SecretResolver
}

func (s *secretsServer) Secret(ctx context.Context, r *pb.SecretRequest) (*pb.SecretResponse, error) {
	v, err := s.resolver.Secret(ctx, r.GetName())
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &pb.SecretResponse{Value: v}, nil
}

// fromStatus converts a gRPC error into a Go error with only the plugin's message.
func fromStatus(err error) error {
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	return errors.New(st.Message())
}

func toProtoRequest(req *plugin.Request, secretsID uint32) *pb.DeployRequest {
	r := &pb.DeployRequest{
		DeploymentId: req.DeploymentID,
		Application:  req.Application,
		Environment:  req.Environment,
		Artifact: &pb.Artifact{
			Type:      req.Artifact.Type,
			Name:      req.Artifact.Name,
			Version:   req.Artifact.Version,
			LocalPath: req.Artifact.LocalPath,
			Sha256:    req.Artifact.SHA256,
		},
		Params:          req.Params,
		SecretsBrokerId: secretsID,
	}
	if t := req.Traffic; t != nil {
		r.Traffic = &pb.TrafficRequest{State: t.State, TesterHeader: t.TesterHeader, TesterTag: t.TesterTag, ProportionalReplicas: t.ProportionalReplicas}
	}
	for _, t := range req.Targets {
		r.Targets = append(r.Targets, &pb.Target{Host: t.Host, Port: int32(t.Port), User: t.User})
	}
	return r
}

func fromProtoRequest(r *pb.DeployRequest) *plugin.Request {
	a := r.GetArtifact()
	req := &plugin.Request{
		DeploymentID: r.GetDeploymentId(),
		Application:  r.GetApplication(),
		Environment:  r.GetEnvironment(),
		Artifact: plugin.Artifact{
			Type:      a.GetType(),
			Name:      a.GetName(),
			Version:   a.GetVersion(),
			LocalPath: a.GetLocalPath(),
			SHA256:    a.GetSha256(),
		},
		Params: r.GetParams(),
	}
	if t := r.GetTraffic(); t != nil {
		req.Traffic = &plugin.TrafficRequest{State: t.GetState(), TesterHeader: t.GetTesterHeader(), TesterTag: t.GetTesterTag(),
			ProportionalReplicas: t.GetProportionalReplicas()}
	}
	for _, t := range r.GetTargets() {
		req.Targets = append(req.Targets, plugin.Target{Host: t.GetHost(), Port: int(t.GetPort()), User: t.GetUser()})
	}
	return req
}

func toProtoRelease(r *plugin.Release) *pb.Release {
	if r == nil {
		return nil
	}
	return &pb.Release{Version: r.Version, Details: r.Details}
}

func fromProtoRelease(r *pb.Release) *plugin.Release {
	if r == nil {
		return nil
	}
	return &plugin.Release{Version: r.GetVersion(), Details: r.GetDetails()}
}

func toProtoResult(res *plugin.Result) *pb.DeployResponse {
	if res == nil {
		return &pb.DeployResponse{}
	}
	out := &pb.DeployResponse{Plugin: res.Plugin}
	for _, t := range res.Targets {
		out.Targets = append(out.Targets, &pb.TargetResult{Host: t.Host, Status: t.Status, Details: t.Details})
	}
	return out
}

func fromProtoResult(r *pb.DeployResponse) *plugin.Result {
	res := &plugin.Result{Plugin: r.GetPlugin()}
	for _, t := range r.GetTargets() {
		res.Targets = append(res.Targets, plugin.TargetResult{Host: t.GetHost(), Status: t.GetStatus(), Details: t.GetDetails()})
	}
	return res
}
