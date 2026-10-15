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

// Serve avvia il plugin e resta in ascolto finché manent non lo arresta. Va chiamata dal main
// del binario. Lo stdout è riservato all'handshake di go-plugin: il plugin deve loggare solo
// tramite il logger ricevuto dalla Factory, che scrive su stderr nel formato inoltrato a manent.
func Serve(f plugin.Factory) {
	goplugin.Serve(&goplugin.ServeConfig{
		HandshakeConfig: Handshake,
		Plugins:         PluginSet(f),
		GRPCServer:      goplugin.DefaultGRPCServer,
	})
}

// logOutput è lo stderr originale del processo, catturato prima di Serve: go-plugin sostituisce
// os.Stderr con un canale gRPC che manent non inoltra al log.
var logOutput = os.Stderr

// Logger restituisce un logger che scrive su stderr in JSON compatibile con hclog: manent
// lo inoltra al proprio log applicativo e quindi anche al SIEM.
func Logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(logOutput, &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) > 0 {
				return a
			}
			switch a.Key {
			case slog.TimeKey:
				// Formato atteso da go-plugin per @timestamp.
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

// server è il lato plugin del protocollo: delega al Deployer costruito dalla Factory.
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
			return nil, status.Errorf(codes.InvalidArgument, "configurazione non valida: %v", err)
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

// get restituisce il deployer configurato.
func (s *server) get() (plugin.Deployer, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.deployer == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin non configurato")
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
		return nil, status.Error(codes.Unimplemented, "release in esercizio non leggibile dal plugin")
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

// slotCall esegue un'operazione dei rilasci per percentuale se il deployer implementa
// l'interfaccia T, altrimenti restituisce UNIMPLEMENTED.
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

// call collega il resolver dei segreti servito da manent ed esegue fn.
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

// secretsClient implementa plugin.SecretResolver chiamando manent.
type secretsClient struct {
	rpc pb.SecretsClient
}

// secretTimeout limita la lettura di un segreto, che non deve bloccare il deploy.
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
