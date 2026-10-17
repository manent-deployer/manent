package external

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	goplugin "github.com/hashicorp/go-plugin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/joeg-ita/manent/pkg/plugin"
	pb "github.com/joeg-ita/manent/pkg/plugin/external/proto"
)

// Serve starts the plugin and keeps listening until manent stops it. Call it from the
// binary's main. Stdout is reserved to the go-plugin handshake: the plugin must log only
// through the logger received by the Factory, which writes to stderr in the format forwarded
// to manent.
func Serve(f plugin.Factory) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         PluginSet(f),
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}

// logOutput is the process's original stderr, captured before Serve: go-plugin replaces
// os.Stderr with a gRPC channel that manent does not forward to the log.
var logOutput = os.Stderr

// Logger returns a logger that writes to stderr in hclog-compatible JSON: manent forwards it
// to its application log and so to the SIEM too.
func Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				// Format go-plugin expects for @timestamp.
				return slog.String("@timestamp", a.Value.Time().Format("2006-01-02T15:04:05.000000Z07:00"))
			case slog.LevelKey:
				return slog.String("@level", strings.ToLower(a.Value.String()))
			case slog.MessageKey:
				return slog.String("@message", a.Value.String())
			}
			return a
		},
	}))
}

// server is the plugin side of the protocol: it delegates to the Deployer built by the Factory.
type server struct {
	pb.UnimplementedDeployerServer
	factory plugin.Factory
	broker  *goplugin.GRPCBroker

	mu       sync.RWMutex
	deployer plugin.Deployer
}

func (s *server) Configure(_ context.Context, r *pb.ConfigureRequest) (*pb.ConfigureResponse, error) {
	cfg := map[string]any{}
	if len(r.GetConfigJson()) > 0 {
		if err := json.Unmarshal(r.GetConfigJson(), &cfg); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid configuration: %v", err)
		}
	}
	d, err := s.factory(cfg, Logger())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	s.mu.Lock()
	s.deployer = d
	s.mu.Unlock()
	return &pb.ConfigureResponse{Name: d.Name(), Capabilities: capabilities(d)}, nil
}

// get returns the configured deployer.
func (s *server) get() (plugin.Deployer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.deployer == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin not configured")
	}
	return s.deployer, nil
}

func (s *server) Supports(_ context.Context, r *pb.SupportsRequest) (*pb.SupportsResponse, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	return &pb.SupportsResponse{Supported: d.Supports(r.GetArtifactType())}, nil
}

func (s *server) Validate(_ context.Context, r *pb.DeployRequest) (*pb.Empty, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	if err := d.Validate(fromProtoRequest(r)); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return &pb.Empty{}, nil
}

func (s *server) Deploy(ctx context.Context, r *pb.DeployRequest) (*pb.DeployResponse, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	return s.call(ctx, r, d.Deploy)
}

func (s *server) DryRun(ctx context.Context, r *pb.DeployRequest) (*pb.DeployResponse, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	dr, ok := d.(plugin.DryRunner)
	if !ok {
		return nil, status.Error(codes.Unimplemented, plugin.ErrDryRunUnsupported.Error())
	}
	return s.call(ctx, r, dr.DryRun)
}

func (s *server) CurrentRelease(ctx context.Context, r *pb.DeployRequest) (*pb.CurrentReleaseResponse, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	ins, ok := d.(plugin.ReleaseInspector)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "the plugin cannot read the release in service")
	}
	var rel *plugin.Release
	if _, err := s.call(ctx, r, func(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
		var err error
		rel, err = ins.CurrentRelease(ctx, req)
		return nil, err
	}); err != nil {
		return nil, err
	}
	return &pb.CurrentReleaseResponse{Release: toProtoRelease(rel)}, nil
}

func (s *server) Rollback(ctx context.Context, r *pb.RollbackRequest) (*pb.DeployResponse, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	rb, ok := d.(plugin.Rollbacker)
	if !ok {
		return nil, status.Error(codes.Unimplemented, plugin.ErrRollbackUnsupported.Error())
	}
	prev := fromProtoRelease(r.GetPrevious())
	if prev == nil {
		return nil, status.Error(codes.InvalidArgument, "release precedente mancante")
	}
	return s.call(ctx, r.GetRequest(), func(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
		return rb.Rollback(ctx, req, *prev)
	})
}

// slotCall runs a percentage rollout operation if the deployer implements interface T,
// otherwise it returns UNIMPLEMENTED.
func slotCall[T any](s *server, ctx context.Context, r *pb.DeployRequest, fn func(T, context.Context, *plugin.Request) error) (*pb.Empty, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	impl, ok := d.(T)
	if !ok {
		return nil, status.Error(codes.Unimplemented, plugin.ErrTrafficUnsupported.Error())
	}
	if _, err := s.call(ctx, r, func(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
		return nil, fn(impl, ctx, req)
	}); err != nil {
		return nil, err
	}
	return &pb.Empty{}, nil
}

func (s *server) PrepareSlots(ctx context.Context, r *pb.DeployRequest) (*pb.StateResponse, error) {
	var state map[string]string
	if _, err := slotCall(s, ctx, r, func(d plugin.SlotDeployer, ctx context.Context, req *plugin.Request) error {
		var err error
		state, err = d.PrepareSlots(ctx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return &pb.StateResponse{State: state}, nil
}

func (s *server) DeployCandidate(ctx context.Context, r *pb.DeployRequest) (*pb.DeployResponse, error) {
	d, err := s.get()
	if err != nil {
		return nil, err
	}
	sd, ok := d.(plugin.SlotDeployer)
	if !ok {
		return nil, status.Error(codes.Unimplemented, plugin.ErrTrafficUnsupported.Error())
	}
	return s.call(ctx, r, sd.DeployCandidate)
}

func (s *server) PromoteCandidate(ctx context.Context, r *pb.DeployRequest) (*pb.Empty, error) {
	return slotCall(s, ctx, r, plugin.SlotDeployer.PromoteCandidate)
}

func (s *server) RevertCandidate(ctx context.Context, r *pb.DeployRequest) (*pb.Empty, error) {
	return slotCall(s, ctx, r, plugin.SlotDeployer.RevertCandidate)
}

func (s *server) RetirePrevious(ctx context.Context, r *pb.DeployRequest) (*pb.Empty, error) {
	return slotCall(s, ctx, r, plugin.SlotDeployer.RetirePrevious)
}

func (s *server) SetCandidateWeight(ctx context.Context, r *pb.WeightRequest) (*pb.Empty, error) {
	return slotCall(s, ctx, r.GetRequest(), func(d plugin.TrafficRouter, ctx context.Context, req *plugin.Request) error {
		return d.SetCandidateWeight(ctx, req, int(r.GetPercent()))
	})
}

func (s *server) CandidateWeight(ctx context.Context, r *pb.DeployRequest) (*pb.WeightResponse, error) {
	w := 0
	if _, err := slotCall(s, ctx, r, func(d plugin.TrafficRouter, ctx context.Context, req *plugin.Request) error {
		var err error
		w, err = d.CandidateWeight(ctx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return &pb.WeightResponse{Percent: int32(w)}, nil
}

func (s *server) RouteTesters(ctx context.Context, r *pb.DeployRequest) (*pb.DetailsResponse, error) {
	var details map[string]string
	if _, err := slotCall(s, ctx, r, func(d plugin.TesterRouter, ctx context.Context, req *plugin.Request) error {
		var err error
		details, err = d.RouteTesters(ctx, req)
		return err
	}); err != nil {
		return nil, err
	}
	return &pb.DetailsResponse{Details: details}, nil
}

func (s *server) ScaleCandidate(ctx context.Context, r *pb.WeightRequest) (*pb.Empty, error) {
	return slotCall(s, ctx, r.GetRequest(), func(d plugin.SlotScaler, ctx context.Context, req *plugin.Request) error {
		return d.ScaleCandidate(ctx, req, int(r.GetPercent()))
	})
}

// call binds the secret resolver served by manent and runs fn.
func (s *server) call(ctx context.Context, r *pb.DeployRequest, fn func(context.Context, *plugin.Request) (*plugin.Result, error)) (*pb.DeployResponse, error) {
	req := fromProtoRequest(r)
	if id := r.GetSecretsBrokerId(); id != 0 {
		conn, err := s.broker.Dial(id)
		if err != nil {
			return nil, status.Errorf(codes.Unavailable, "connessione al resolver dei segreti: %v", err)
		}
		defer conn.Close()
		req.Secrets = secretsClient{rpc: pb.NewSecretsClient(conn)}
	}
	res, err := fn(ctx, req)
	switch {
	case errors.Is(err, plugin.ErrDryRunUnsupported), errors.Is(err, plugin.ErrRollbackUnsupported), errors.Is(err, plugin.ErrTrafficUnsupported):
		return nil, status.Error(codes.Unimplemented, err.Error())
	case err != nil:
		return nil, status.Error(codes.Unknown, err.Error())
	}
	return toProtoResult(res), nil
}

// secretsClient implements plugin.SecretResolver by calling manent.
type secretsClient struct {
	rpc pb.SecretsClient
}

// secretTimeout limits reading a secret, which must not block the deploy.
const secretTimeout = 30 * time.Second

func (c secretsClient) Secret(ctx context.Context, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, secretTimeout)
	defer cancel()
	resp, err := c.rpc.Secret(ctx, &pb.SecretRequest{Name: name})
	if err != nil {
		return "", fromStatus(err)
	}
	return resp.GetValue(), nil
}
