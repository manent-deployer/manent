// Package external permette di eseguire un plugin di deploy come processo separato, distribuito
// come binario indipendente da manent. Il trasporto è gRPC tramite github.com/hashicorp/go-plugin.
//
// Lato plugin basta un main che passa a Serve la stessa plugin.Factory usata per un plugin
// compilato dentro manent:
//
//	func main() { external.Serve(mioplugin.Factory) }
//
// Lato manent il processo viene avviato e gestito da internal/extplugin.
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

// ProtocolVersion è la versione del protocollo host-plugin. Va incrementata a ogni modifica
// incompatibile di deployer.proto: go-plugin rifiuta i plugin con una versione diversa.
const ProtocolVersion = 1

// Handshake è condiviso da host e plugin. Il cookie evita che il binario del plugin venga
// eseguito direttamente per errore: non è una misura di sicurezza.
var Handshake = goplugin.HandshakeConfig{
	ProtocolVersion:  ProtocolVersion,
	MagicCookieKey:   "MANENT_PLUGIN",
	MagicCookieValue: "7d1f0c5e-deployer",
}

// PluginName è il nome con cui il deployer è esposto nel PluginSet di go-plugin.
const PluginName = "deployer"

// GRPCPlugin implementa goplugin.GRPCPlugin per entrambi i lati: Factory è usata dal plugin,
// mentre l'host ottiene un *Client.
type GRPCPlugin struct {
	goplugin.NetRPCUnsupportedPlugin
	Factory plugin.Factory
}

// GRPCServer registra il servizio Deployer nel processo del plugin.
func (p *GRPCPlugin) GRPCServer(broker *goplugin.GRPCBroker, s *grpc.Server) error {
	pb.RegisterDeployerServer(s, &server{factory: p.Factory, broker: broker})
	return nil
}

// GRPCClient restituisce il client usato da manent.
func (p *GRPCPlugin) GRPCClient(_ context.Context, broker *goplugin.GRPCBroker, c *grpc.ClientConn) (any, error) {
	return &Client{rpc: pb.NewDeployerClient(c), broker: broker}, nil
}

// PluginSet restituisce l'insieme di plugin da passare a go-plugin.
func PluginSet(f plugin.Factory) goplugin.PluginSet {
	return goplugin.PluginSet{PluginName: &GRPCPlugin{Factory: f}}
}

// Capacità opzionali dichiarate dal plugin in Configure.
const (
	CapabilityDryRun         = "dry_run"
	CapabilityCurrentRelease = "current_release"
	CapabilityRollback       = "rollback"
	CapabilitySlots          = "slots"
	CapabilityTrafficRouter  = "traffic_router"
	CapabilityTesters        = "testers"
	CapabilityScale          = "scale"
)

// capabilities restituisce le interfacce opzionali implementate dal deployer.
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

// Client è il lato host del protocollo: espone il processo del plugin come plugin.Deployer.
type Client struct {
	rpc    pb.DeployerClient
	broker *goplugin.GRPCBroker
	name   string
	caps   []string
}

// Configure invia la configurazione al plugin e ne registra nome e capacità.
func (c *Client) Configure(ctx context.Context, configJSON []byte) (string, error) {
	resp, err := c.rpc.Configure(ctx, &pb.ConfigureRequest{ConfigJson: configJSON})
	if err != nil {
		return "", fromStatus(err)
	}
	c.name, c.caps = resp.GetName(), resp.GetCapabilities()
	return c.name, nil
}

// Has indica se il plugin ha dichiarato la capacità indicata.
func (c *Client) Has(capability string) bool { return slices.Contains(c.caps, capability) }

// CurrentRelease legge la release in esercizio tramite il plugin; restituisce un errore se il
// plugin non implementa plugin.ReleaseInspector.
func (c *Client) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	var rel *plugin.Release
	_, err := c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.CurrentRelease(ctx, r)
		if status.Code(err) == codes.Unimplemented {
			return nil, fmt.Errorf("il plugin %q non sa leggere la release in esercizio: usare un altro locator", c.name)
		}
		if err != nil {
			return nil, err
		}
		rel = fromProtoRelease(resp.GetRelease())
		return &pb.DeployResponse{}, nil
	})
	return rel, err
}

// Rollback riporta i target alla release indicata; restituisce plugin.ErrRollbackUnsupported
// se il plugin non implementa plugin.Rollbacker.
func (c *Client) Rollback(ctx context.Context, req *plugin.Request, previous plugin.Release) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.Rollback(ctx, &pb.RollbackRequest{Request: r, Previous: toProtoRelease(&previous)})
		if status.Code(err) == codes.Unimplemented {
			return nil, plugin.ErrRollbackUnsupported
		}
		return resp, err
	})
}

// TrafficSupported implementa plugin.TrafficChecker con le capacità dichiarate in Configure.
func (c *Client) TrafficSupported(context.Context) (plugin.TrafficSupport, error) {
	return plugin.TrafficSupport{Slots: c.Has(CapabilitySlots), Router: c.Has(CapabilityTrafficRouter),
		Testers: c.Has(CapabilityTesters), Scale: c.Has(CapabilityScale)}, nil
}

// unsupported traduce UNIMPLEMENTED in plugin.ErrTrafficUnsupported.
func unsupported(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return plugin.ErrTrafficUnsupported
	}
	return err
}

// empty esegue una chiamata senza risultato con i segreti serviti.
func (c *Client) empty(req *plugin.Request, call func(*pb.DeployRequest) error) error {
	_, err := c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		return &pb.DeployResponse{}, unsupported(call(r))
	})
	return err
}

// PrepareSlots implementa plugin.SlotDeployer.
func (c *Client) PrepareSlots(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	var state map[string]string
	err := c.empty(req, func(r *pb.DeployRequest) error {
		resp, err := c.rpc.PrepareSlots(ctx, r)
		state = resp.GetState()
		return err
	})
	return state, err
}

// DeployCandidate implementa plugin.SlotDeployer.
func (c *Client) DeployCandidate(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.DeployCandidate(ctx, r)
		return resp, unsupported(err)
	})
}

// PromoteCandidate implementa plugin.SlotDeployer.
func (c *Client) PromoteCandidate(ctx context.Context, req *plugin.Request) error {
	return c.empty(req, func(r *pb.DeployRequest) error { _, err := c.rpc.PromoteCandidate(ctx, r); return err })
}

// RevertCandidate implementa plugin.SlotDeployer.
func (c *Client) RevertCandidate(ctx context.Context, req *plugin.Request) error {
	return c.empty(req, func(r *pb.DeployRequest) error { _, err := c.rpc.RevertCandidate(ctx, r); return err })
}

// RetirePrevious implementa plugin.SlotDeployer.
func (c *Client) RetirePrevious(ctx context.Context, req *plugin.Request) error {
	return c.empty(req, func(r *pb.DeployRequest) error { _, err := c.rpc.RetirePrevious(ctx, r); return err })
}

// SetCandidateWeight implementa plugin.TrafficRouter.
func (c *Client) SetCandidateWeight(ctx context.Context, req *plugin.Request, percent int) error {
	return c.empty(req, func(r *pb.DeployRequest) error {
		_, err := c.rpc.SetCandidateWeight(ctx, &pb.WeightRequest{Request: r, Percent: int32(percent)})
		return err
	})
}

// CandidateWeight implementa plugin.TrafficRouter.
func (c *Client) CandidateWeight(ctx context.Context, req *plugin.Request) (int, error) {
	w := 0
	err := c.empty(req, func(r *pb.DeployRequest) error {
		resp, err := c.rpc.CandidateWeight(ctx, r)
		w = int(resp.GetPercent())
		return err
	})
	return w, err
}

// RouteTesters implementa plugin.TesterRouter.
func (c *Client) RouteTesters(ctx context.Context, req *plugin.Request) (map[string]string, error) {
	var details map[string]string
	err := c.empty(req, func(r *pb.DeployRequest) error {
		resp, err := c.rpc.RouteTesters(ctx, r)
		details = resp.GetDetails()
		return err
	})
	return details, err
}

// ScaleCandidate implementa plugin.SlotScaler.
func (c *Client) ScaleCandidate(ctx context.Context, req *plugin.Request, percent int) error {
	return c.empty(req, func(r *pb.DeployRequest) error {
		_, err := c.rpc.ScaleCandidate(ctx, &pb.WeightRequest{Request: r, Percent: int32(percent)})
		return err
	})
}

// Name restituisce il nome dichiarato dal plugin in Configure.
func (c *Client) Name() string { return c.name }

// shortCallTimeout limita le chiamate senza contesto del chiamante (Supports, Validate),
// che non devono avere effetti né durare a lungo.
const shortCallTimeout = 30 * time.Second

// Supports interroga il plugin; un errore di comunicazione vale come "non supportato".
func (c *Client) Supports(artifactType string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	resp, err := c.rpc.Supports(ctx, &pb.SupportsRequest{ArtifactType: artifactType})
	return err == nil && resp.GetSupported()
}

// Validate valida la richiesta nel plugin, senza resolver dei segreti.
func (c *Client) Validate(req *plugin.Request) error {
	ctx, cancel := context.WithTimeout(context.Background(), shortCallTimeout)
	defer cancel()
	_, err := c.rpc.Validate(ctx, toProtoRequest(req, 0))
	return fromStatus(err)
}

// Deploy esegue il deploy nel plugin servendo i segreti per la durata della chiamata.
func (c *Client) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		return c.rpc.Deploy(ctx, r)
	})
}

// DryRun esegue il dry-run nel plugin; restituisce plugin.ErrDryRunUnsupported se il plugin
// non implementa plugin.DryRunner.
func (c *Client) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return c.withSecrets(req, func(r *pb.DeployRequest) (*pb.DeployResponse, error) {
		resp, err := c.rpc.DryRun(ctx, r)
		if status.Code(err) == codes.Unimplemented {
			return nil, plugin.ErrDryRunUnsupported
		}
		return resp, err
	})
}

// withSecrets serve il resolver dei segreti sul broker di go-plugin, esegue la chiamata e
// arresta il server: il plugin può leggere i segreti solo durante questa chiamata.
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
				// La chiamata è già terminata: il server non deve restare in ascolto.
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

// secretsServer espone il SecretResolver della richiesta al processo del plugin.
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

// fromStatus converte un errore gRPC in un errore Go con il solo messaggio del plugin.
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
		r.Traffic = &pb.TrafficRequest{State: t.State, TesterHeader: t.TesterHeader, TesterTag: t.TesterTag}
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
		req.Traffic = &plugin.TrafficRequest{State: t.GetState(), TesterHeader: t.GetTesterHeader(), TesterTag: t.GetTesterTag()}
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
