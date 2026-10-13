package ecs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/joeg-ita/manent/pkg/plugin"
	"github.com/joeg-ita/manent/plugins/internal/awsauth"
)

// fakeECS emula il protocollo JSON di ECS per le operazioni usate dal plugin. Un deployment
// diventa COMPLETED alla seconda lettura del servizio, FAILED se l'immagine contiene "broken".
type fakeECS struct {
	mu        sync.Mutex
	t         *testing.T
	services  map[string]*fakeService
	taskDefs  map[string]map[string]any // ARN → task definition
	tags      map[string][]map[string]string
	revisions map[string]int
	registers int
	updates   int
}

type fakeService struct {
	taskDef     string
	deployments []map[string]any
	reads       int
}

func newFakeECS(t *testing.T) *fakeECS {
	f := &fakeECS{t: t, services: map[string]*fakeService{}, taskDefs: map[string]map[string]any{}, tags: map[string][]map[string]string{}, revisions: map[string]int{}}
	arn := f.register(map[string]any{
		"family": "shop", "cpu": "256", "memory": "512", "networkMode": "awsvpc",
		"executionRoleArn": "arn:aws:iam::123456789012:role/exec",
		"containerDefinitions": []any{
			map[string]any{"name": "shop", "image": "nginx:1.27", "essential": true, "environment": []any{map[string]any{"name": "MODE", "value": "prod"}}},
			map[string]any{"name": "log-router", "image": "fluent-bit:3"},
		},
	}, []map[string]string{{"key": "team", "value": "web"}})
	f.services["prod/shop"] = &fakeService{taskDef: arn, deployments: []map[string]any{deployment(arn, "PRIMARY", "COMPLETED")}}
	return f
}

func deployment(arn, status, rollout string) map[string]any {
	return map[string]any{"id": "ecs-svc/" + arn[len(arn)-1:], "status": status, "taskDefinition": arn, "rolloutState": rollout, "desiredCount": 2, "runningCount": 2}
}

func (f *fakeECS) register(td map[string]any, tags []map[string]string) string {
	family := td["family"].(string)
	f.revisions[family]++
	arn := fmt.Sprintf("arn:aws:ecs:eu-west-1:123456789012:task-definition/%s:%d", family, f.revisions[family])
	td["taskDefinitionArn"], td["revision"], td["status"] = arn, f.revisions[family], "ACTIVE"
	f.taskDefs[arn] = td
	f.tags[arn] = tags
	return arn
}

func (f *fakeECS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIDTEST/") {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"__type":"AccessDeniedException","message":"denied"}`))
		return
	}
	op := strings.TrimPrefix(r.Header.Get("X-Amz-Target"), "AmazonEC2ContainerServiceV20141113.")
	var in map[string]any
	body, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(body, &in)
	reply := func(v any) {
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch op {
	case "DescribeServices":
		name := in["cluster"].(string) + "/" + in["services"].([]any)[0].(string)
		s, ok := f.services[name]
		if !ok {
			reply(map[string]any{"services": []any{}, "failures": []any{map[string]any{"arn": name, "reason": "MISSING"}}})
			return
		}
		s.reads++
		if p := s.deployments[0]; p["rolloutState"] == "IN_PROGRESS" && s.reads >= 2 {
			image := f.taskDefs[p["taskDefinition"].(string)]["containerDefinitions"].([]any)[0].(map[string]any)["image"].(string)
			if strings.Contains(image, "broken") {
				p["rolloutState"], p["rolloutStateReason"], p["failedTasks"] = "FAILED", "ECS deployment circuit breaker: tasks failed to start.", 3
			} else {
				p["rolloutState"], p["runningCount"] = "COMPLETED", 2
				s.deployments = s.deployments[:1]
			}
		}
		reply(map[string]any{"services": []any{map[string]any{
			"serviceName": in["services"].([]any)[0], "status": "ACTIVE", "taskDefinition": s.taskDef,
			"desiredCount": 2, "runningCount": 2, "deploymentController": map[string]any{"type": "ECS"}, "deployments": s.deployments,
		}}})
	case "DescribeTaskDefinition":
		arn := in["taskDefinition"].(string)
		td, ok := f.taskDefs[arn]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			reply(map[string]any{"__type": "ClientException", "message": "Unable to describe task definition."})
			return
		}
		reply(map[string]any{"taskDefinition": td, "tags": f.tags[arn]})
	case "RegisterTaskDefinition":
		f.registers++
		var tags []map[string]string
		raw, _ := json.Marshal(in["tags"])
		_ = json.Unmarshal(raw, &tags)
		delete(in, "tags")
		arn := f.register(in, tags)
		reply(map[string]any{"taskDefinition": f.taskDefs[arn], "tags": tags})
	case "UpdateService":
		f.updates++
		s := f.services[in["cluster"].(string)+"/"+in["service"].(string)]
		arn := in["taskDefinition"].(string)
		s.taskDef, s.reads = arn, 0
		for _, d := range s.deployments {
			d["status"] = "ACTIVE"
		}
		d := deployment(arn, "PRIMARY", "IN_PROGRESS")
		d["runningCount"] = 0
		s.deployments = append([]map[string]any{d}, s.deployments...)
		reply(map[string]any{"service": map[string]any{"serviceName": in["service"]}})
	case "ListTaskDefinitions":
		var arns []string
		for i := f.revisions["shop"]; i >= 1; i-- {
			arns = append(arns, fmt.Sprintf("arn:aws:ecs:eu-west-1:123456789012:task-definition/shop:%d", i))
		}
		arns = append(arns, "arn:aws:ecs:eu-west-1:123456789012:task-definition/shop-api:1")
		reply(map[string]any{"taskDefinitionArns": arns})
	default:
		f.t.Errorf("operazione inattesa %q", op)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeECS) image(arn, container string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.taskDefs[arn]["containerDefinitions"].([]any) {
		if m := c.(map[string]any); m["name"] == container {
			return m["image"].(string)
		}
	}
	return ""
}

func (f *fakeECS) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services["prod/shop"].taskDef
}

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("segreto %q assente", name)
	}
	return v, nil
}

// isolateAWS evita che i test leggano configurazione e credenziali AWS dell'utente.
func isolateAWS(t *testing.T) {
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_PROFILE": "", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "",
	} {
		t.Setenv(k, v)
	}
}

func setup(t *testing.T) (*Plugin, *fakeECS) {
	isolateAWS(t)
	f := newFakeECS(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := New(SDKConnector{Endpoint: aws.String(srv.URL)}, []string{Name}, slog.New(slog.DiscardHandler))
	p.poll = 0
	return p, f
}

const digest = "sha256:" + "ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12ab12"

func request(t *testing.T, version, image string) *plugin.Request {
	path := filepath.Join(t.TempDir(), "shop"+plugin.ImageRefExt)
	if err := os.WriteFile(path, []byte(image+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &plugin.Request{
		DeploymentID: "shop-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "ecs", Name: "shop", Version: version, LocalPath: path},
		Targets:  []plugin.Target{{Host: "eu-west-1/prod"}},
		Params: map[string]string{
			awsauth.ParamAccessKeySecret: "aws-id", awsauth.ParamSecretKeySecret: "aws-secret",
		},
		Secrets: mapSecrets{"aws-id": "AKIDTEST", "aws-secret": "s3cr3t"},
	}
}

func TestDeployERollback(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	img1 := "123456789012.dkr.ecr.eu-west-1.amazonaws.com/shop@" + digest
	req := request(t, "1.0.0", img1)
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	if rel, err := p.CurrentRelease(ctx, req); err != nil || rel != nil {
		t.Fatalf("release prima del primo deploy: %+v, %v", rel, err)
	}
	res, err := p.Deploy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	first := res.Targets[0].Details[DetailTaskDefinition]
	if f.current() != first || f.image(first, "shop") != img1 || f.image(first, "log-router") != "fluent-bit:3" {
		t.Fatalf("servizio su %s, immagini %q %q", f.current(), f.image(first, "shop"), f.image(first, "log-router"))
	}
	f.mu.Lock()
	td := f.taskDefs[first]
	tags := f.tags[first]
	f.mu.Unlock()
	if td["executionRoleArn"] != "arn:aws:iam::123456789012:role/exec" || td["cpu"] != "256" {
		t.Errorf("campi della task definition non copiati: %v", td)
	}
	if env := td["containerDefinitions"].([]any)[0].(map[string]any)["environment"]; env == nil {
		t.Error("environment del container perso")
	}
	got := map[string]string{}
	for _, tg := range tags {
		got[tg["key"]] = tg["value"]
	}
	if got["team"] != "web" || got[TagVersion] != "1.0.0" || got[TagDeployment] != "shop-1.0.0" {
		t.Errorf("tag: %v", got)
	}

	// Rieseguire lo stesso deploy non registra una nuova revisione.
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.registers != 1 || f.updates != 1 {
		t.Errorf("deploy ripetuto: %d registrazioni, %d aggiornamenti", f.registers, f.updates)
	}

	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.0.0" || rel.Details[DetailTaskDefinition+"@eu-west-1/prod"] != first {
		t.Fatalf("release in esercizio: %+v, %v", rel, err)
	}

	// La 2.0.0 fallisce per il circuit breaker; il rollback torna alla revisione della 1.0.0.
	req2 := request(t, "2.0.0", "123456789012.dkr.ecr.eu-west-1.amazonaws.com/shop-broken@"+digest)
	res, err = p.Deploy(ctx, req2)
	if err == nil || !strings.Contains(err.Error(), "circuit breaker") || res.Targets[0].Status != plugin.StatusFailed {
		t.Fatalf("deploy fallito atteso: %v", err)
	}
	if _, err := p.Rollback(ctx, req2, *rel); err != nil {
		t.Fatal(err)
	}
	if f.current() != first {
		t.Fatalf("dopo il rollback il servizio usa %s, atteso %s", f.current(), first)
	}

	// Senza i dettagli la revisione si cerca per tag (la famiglia shop-api è esclusa).
	f.mu.Lock()
	f.services["prod/shop"].taskDef = "arn:aws:ecs:eu-west-1:123456789012:task-definition/shop:2"
	f.mu.Unlock()
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if f.current() != first {
		t.Fatalf("rollback senza dettagli su %s, atteso %s", f.current(), first)
	}
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "0.9.0"}); err == nil || !strings.Contains(err.Error(), "nessuna revisione") {
		t.Errorf("versione assente: %v", err)
	}
}

func TestDryRunEValidazione(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	req := request(t, "1.0.0", "registry.example.com/shop@"+digest)
	res, err := p.DryRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Targets[0].Details; res.Targets[0].Status != plugin.StatusReady || d["container"] != "shop" || d["running"] != "2/2" {
		t.Errorf("dry-run: %+v", res.Targets[0])
	}
	if f.registers != 0 || f.updates != 0 {
		t.Error("il dry-run ha modificato il servizio")
	}

	missing := request(t, "1.0.0", "registry.example.com/shop@"+digest)
	missing.Params[ParamService] = "cart"
	if _, err := p.DryRun(ctx, missing); err == nil || !strings.Contains(err.Error(), "assente dal cluster") {
		t.Errorf("servizio assente: %v", err)
	}
	wrong := request(t, "1.0.0", "registry.example.com/shop@"+digest)
	wrong.Params[ParamContainer] = "api"
	if _, err := p.DryRun(ctx, wrong); err == nil || !strings.Contains(err.Error(), "log-router") {
		t.Errorf("container assente: %v", err)
	}
	denied := request(t, "1.0.0", "registry.example.com/shop@"+digest)
	denied.Secrets = mapSecrets{"aws-id": "AKIDOTHER", "aws-secret": "x"}
	if _, err := p.DryRun(ctx, denied); err == nil {
		t.Error("credenziali rifiutate: atteso errore")
	}

	bad := request(t, "", "x")
	bad.Artifact.LocalPath = "/tmp/app.zip"
	bad.Targets = []plugin.Target{{Host: "europa"}}
	bad.Params[ParamWaitTimeout] = "presto"
	bad.Params[awsauth.ParamSecretKeySecret] = ""
	bad.Params[awsauth.ParamRoleARN] = "ruolo"
	err = p.Validate(bad)
	for _, want := range []string{"artifact.version", "immagine di un registry", "europa", ParamWaitTimeout, awsauth.ParamSecretKeySecret, awsauth.ParamRoleARN} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validazione: manca %q in %v", want, err)
		}
	}
}

func TestTarget(t *testing.T) {
	req := &plugin.Request{Params: map[string]string{ParamCluster: "main"}}
	for host, want := range map[string]target{
		"eu-west-1":         {host: "eu-west-1", region: "eu-west-1", cluster: "main"},
		"us-east-2/default": {host: "us-east-2/default", region: "us-east-2", cluster: "default"},
	} {
		got, err := parseTarget(plugin.Target{Host: host}, req)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v", host, got, err)
		}
	}
	if got, _ := parseTarget(plugin.Target{Host: "eu-west-1"}, &plugin.Request{}); got.cluster != defaultCluster {
		t.Errorf("cluster di default: %q", got.cluster)
	}
}
