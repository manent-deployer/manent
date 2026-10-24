package lambda

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/manent-deployer/manent/pkg/plugin"
	"github.com/manent-deployer/manent/plugins/internal/awsauth"
)

// fakeLambda emulates the Lambda REST API for the operations used by the plugin. A code update
// stays InProgress for one read, then Successful (Failed if the image contains "broken");
// publishing unchanged code returns the last version, as Lambda does.
type fakeLambda struct {
	mu        sync.Mutex
	t         *testing.T
	functions map[string]*fakeFunction
	dryRuns   int
}

type fakeFunction struct {
	packageType string
	codeSha     string
	image       string
	updating    int
	failed      bool
	versions    []map[string]any
	aliases     map[string]map[string]any
	updates     int
}

func newFakeLambda(t *testing.T) *fakeLambda {
	return &fakeLambda{t: t, functions: map[string]*fakeFunction{
		"shop":   {packageType: PackageImage, codeSha: "init", aliases: map[string]map[string]any{}},
		"worker": {packageType: PackageZip, codeSha: "init", aliases: map[string]map[string]any{}},
	}}
}

func (f *fakeLambda) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	missing := func(what string) {
		w.Header().Set("X-Amzn-ErrorType", "ResourceNotFoundException")
		reply(http.StatusNotFound, map[string]any{"Type": "User", "Message": what + " not found"})
	}
	if !strings.Contains(r.Header.Get("Authorization"), "Credential=AKIDTEST/") {
		w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
		reply(http.StatusForbidden, map[string]any{"Message": "denied"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/2015-03-31/functions/"), "/")
	fn, ok := f.functions[parts[0]]
	if !ok {
		missing("Function")
		return
	}
	var in map[string]any
	_ = json.NewDecoder(r.Body).Decode(&in)
	config := func() map[string]any {
		c := map[string]any{"FunctionName": parts[0], "PackageType": fn.packageType, "State": "Active", "LastUpdateStatus": "Successful",
			"CodeSha256": fn.codeSha, "Version": "$LATEST"}
		if fn.updating > 0 {
			fn.updating--
			c["LastUpdateStatus"] = "InProgress"
		} else if fn.failed {
			c["LastUpdateStatus"], c["LastUpdateStatusReason"] = "Failed", "image manifest not found"
		}
		return c
	}
	switch {
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "configuration":
		q := r.URL.Query().Get("Qualifier")
		if q == "" || q == "$LATEST" {
			reply(http.StatusOK, config())
			return
		}
		for _, v := range fn.versions {
			if v["Version"] == q {
				reply(http.StatusOK, v)
				return
			}
		}
		missing("Version")
	case r.Method == http.MethodPut && len(parts) == 2 && parts[1] == "code":
		if in["DryRun"] == true {
			f.dryRuns++
			reply(http.StatusOK, config())
			return
		}
		fn.updates++
		fn.updating, fn.failed = 1, false
		if img, _ := in["ImageUri"].(string); img != "" {
			fn.image = img
			sum := sha256.Sum256([]byte(img))
			fn.codeSha = hex.EncodeToString(sum[:])
			fn.failed = strings.Contains(img, "broken")
		} else {
			zip, _ := base64.StdEncoding.DecodeString(in["ZipFile"].(string))
			sum := sha256.Sum256(zip)
			fn.codeSha = base64.StdEncoding.EncodeToString(sum[:])
		}
		reply(http.StatusOK, config())
	case r.Method == http.MethodPost && len(parts) == 2 && parts[1] == "versions":
		if in["CodeSha256"] != fn.codeSha {
			w.Header().Set("X-Amzn-ErrorType", "InvalidParameterValueException")
			reply(http.StatusBadRequest, map[string]any{"Message": "CodeSha256 does not match"})
			return
		}
		if n := len(fn.versions); n > 0 && fn.versions[n-1]["CodeSha256"] == fn.codeSha {
			reply(http.StatusCreated, fn.versions[n-1])
			return
		}
		v := map[string]any{"FunctionName": parts[0], "Version": strconv.Itoa(len(fn.versions) + 1), "CodeSha256": fn.codeSha,
			"Description": in["Description"], "State": "Active", "LastUpdateStatus": "Successful", "PackageType": fn.packageType}
		fn.versions = append(fn.versions, v)
		reply(http.StatusCreated, v)
	case r.Method == http.MethodGet && len(parts) == 2 && parts[1] == "versions":
		all := []any{config()}
		for _, v := range fn.versions {
			all = append(all, v)
		}
		reply(http.StatusOK, map[string]any{"Versions": all})
	case len(parts) == 3 && parts[1] == "aliases" && r.Method == http.MethodGet:
		a, ok := fn.aliases[parts[2]]
		if !ok {
			missing("Alias")
			return
		}
		reply(http.StatusOK, a)
	case len(parts) == 2 && parts[1] == "aliases" && r.Method == http.MethodPost:
		name := in["Name"].(string)
		fn.aliases[name] = map[string]any{"Name": name, "FunctionVersion": in["FunctionVersion"], "Description": in["Description"]}
		reply(http.StatusCreated, fn.aliases[name])
	case len(parts) == 3 && parts[1] == "aliases" && r.Method == http.MethodPut:
		a, ok := fn.aliases[parts[2]]
		if !ok {
			missing("Alias")
			return
		}
		a["FunctionVersion"], a["Description"] = in["FunctionVersion"], in["Description"]
		a["RoutingConfig"] = in["RoutingConfig"]
		reply(http.StatusOK, a)
	default:
		f.t.Errorf("richiesta inattesa %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadRequest)
	}
}

func (f *fakeLambda) alias(fn string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a, ok := f.functions[fn].aliases["live"]; ok {
		return a["FunctionVersion"].(string)
	}
	return ""
}

type mapSecrets map[string]string

func (m mapSecrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", fmt.Errorf("segreto %q assente", name)
	}
	return v, nil
}

func setup(t *testing.T) (*Plugin, *fakeLambda) {
	dir := t.TempDir()
	for k, v := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_EC2_METADATA_DISABLED": "true", "AWS_PROFILE": "", "AWS_ACCESS_KEY_ID": "", "AWS_SECRET_ACCESS_KEY": "",
	} {
		t.Setenv(k, v)
	}
	f := newFakeLambda(t)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	p := New(SDKConnector{Endpoint: aws.String(srv.URL)}, []string{Name}, slog.New(slog.DiscardHandler))
	p.poll = 0
	return p, f
}

const digest = "sha256:" + "cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34cd34"

func request(t *testing.T, version, file, content string) *plugin.Request {
	path := filepath.Join(t.TempDir(), file)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return &plugin.Request{
		DeploymentID: "shop-" + version, Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: Name, Name: "shop", Version: version, LocalPath: path, SHA256: hex.EncodeToString(sum[:])},
		Targets:  []plugin.Target{{Host: "eu-west-1"}},
		Params:   map[string]string{awsauth.ParamAccessKeySecret: "aws-id", awsauth.ParamSecretKeySecret: "aws-secret"},
		Secrets:  mapSecrets{"aws-id": "AKIDTEST", "aws-secret": "s3cr3t"},
	}
}

func imageRequest(t *testing.T, version, repo string) *plugin.Request {
	req := request(t, version, "shop"+plugin.ImageRefExt, "123456789012.dkr.ecr.us-east-1.amazonaws.com/"+repo+"@"+digest)
	req.Artifact.SHA256 = strings.TrimPrefix(digest, "sha256:")
	return req
}

func TestImmagineERollback(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	req := imageRequest(t, "1.0.0", "shop")
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
	// The ECR image is moved to the target's region.
	if img := res.Targets[0].Details["image"]; img != "123456789012.dkr.ecr.eu-west-1.amazonaws.com/shop@"+digest || f.functions["shop"].image != img {
		t.Errorf("immagine %q", img)
	}
	if f.alias("shop") != "1" || res.Targets[0].Details[DetailFunctionVersion] != "1" {
		t.Fatalf("alias su %q, dettagli %v", f.alias("shop"), res.Targets[0].Details)
	}
	if _, err := p.Deploy(ctx, req); err != nil || f.functions["shop"].updates != 1 {
		t.Fatalf("deploy ripetuto: %v, %d aggiornamenti", err, f.functions["shop"].updates)
	}
	rel, err := p.CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.0.0" || rel.Details[DetailFunctionVersion+"@eu-west-1"] != "1" {
		t.Fatalf("release in esercizio: %+v, %v", rel, err)
	}

	// A failed update leaves the alias where it was.
	broken := imageRequest(t, "2.0.0", "shop-broken")
	if _, err := p.Deploy(ctx, broken); err == nil || !strings.Contains(err.Error(), "image manifest not found") {
		t.Fatalf("aggiornamento fallito atteso: %v", err)
	}
	if f.alias("shop") != "1" {
		t.Fatalf("alias spostato dopo un errore: %s", f.alias("shop"))
	}

	req2 := imageRequest(t, "2.0.0", "shop-v2")
	res, err = p.Deploy(ctx, req2)
	if err != nil || f.alias("shop") != "2" || res.Targets[0].Details["previous"] != "1.0.0" {
		t.Fatalf("seconda release: %v, alias %s, %v", err, f.alias("shop"), res.Targets[0].Details)
	}
	if _, err := p.Rollback(ctx, req2, *rel); err != nil || f.alias("shop") != "1" {
		t.Fatalf("rollback: %v, alias %s", err, f.alias("shop"))
	}
	if cur, _ := p.CurrentRelease(ctx, req2); cur == nil || cur.Version != "1.0.0" {
		t.Errorf("dopo il rollback: %+v", cur)
	}
	// Without details the version is searched among the published ones.
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "2.0.0"}); err != nil || f.alias("shop") != "2" {
		t.Fatalf("rollback senza dettagli: %v, alias %s", err, f.alias("shop"))
	}
	if _, err := p.Rollback(ctx, req2, plugin.Release{Version: "0.1.0"}); err == nil || !strings.Contains(err.Error(), "no version") {
		t.Errorf("versione assente: %v", err)
	}
}

func TestPacchettoZip(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	req := request(t, "1.0.0", "worker.zip", "PK\x03\x04 contenuto")
	req.Params[ParamFunction] = "worker"
	if _, err := p.Deploy(ctx, req); err != nil {
		t.Fatal(err)
	}
	if f.alias("worker") != "1" {
		t.Fatalf("alias %q", f.alias("worker"))
	}

	// The sha256 returned by Lambda must match the artifact's.
	bad := request(t, "1.1.0", "worker.zip", "PK\x03\x04 altro")
	bad.Params[ParamFunction] = "worker"
	bad.Artifact.SHA256 = strings.Repeat("0", 64)
	if _, err := p.Deploy(ctx, bad); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("sha256 diverso: %v", err)
	}

	// Zip package on an image function.
	wrong := request(t, "1.0.0", "shop.zip", "PK")
	if _, err := p.Deploy(ctx, wrong); err == nil || !strings.Contains(err.Error(), "uses Image packages") {
		t.Errorf("tipo di pacchetto: %v", err)
	}
}

func TestDryRunEValidazione(t *testing.T) {
	p, f := setup(t)
	ctx := context.Background()
	req := imageRequest(t, "1.0.0", "shop")
	res, err := p.DryRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Targets[0].Details; res.Targets[0].Status != plugin.StatusReady || d["alias_status"] != "to be created" || d["package_type"] != PackageImage {
		t.Errorf("dry-run: %+v", res.Targets[0])
	}
	if f.dryRuns != 1 || f.functions["shop"].updates != 0 || f.alias("shop") != "" {
		t.Error("il dry-run ha modificato la funzione")
	}
	missing := imageRequest(t, "1.0.0", "shop")
	missing.Params[ParamFunction] = "cart"
	if _, err := p.DryRun(ctx, missing); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Errorf("funzione assente: %v", err)
	}

	bad := request(t, "", "app.tar.gz", "x")
	bad.Targets = []plugin.Target{{Host: "europa"}}
	bad.Params[ParamAlias] = "42"
	bad.Params[ParamFunction] = "shop:prod"
	err = p.Validate(bad)
	for _, want := range []string{"artifact.version", ".zip", "europa", ParamAlias, ParamFunction} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("validazione: manca %q in %v", want, err)
		}
	}
}

func TestDescrizioneEImmagine(t *testing.T) {
	v, d := parseDescription(description("1.2.3+build.4", "shop-1.2.3"))
	if v != "1.2.3+build.4" || d != "shop-1.2.3" {
		t.Errorf("descrizione: %q %q", v, d)
	}
	if v, _ := parseDescription("deploy manuale"); v != "" {
		t.Errorf("descrizione estranea: %q", v)
	}
	for in, want := range map[string]string{
		"123456789012.dkr.ecr.us-east-1.amazonaws.com/a@sha256:x":     "123456789012.dkr.ecr.eu-south-1.amazonaws.com/a@sha256:x",
		"123456789012.dkr.ecr.cn-north-1.amazonaws.com.cn/a@sha256:x": "123456789012.dkr.ecr.eu-south-1.amazonaws.com.cn/a@sha256:x",
		"registry.example.com/a@sha256:x":                             "registry.example.com/a@sha256:x",
	} {
		if got := regionalImage(in, "eu-south-1"); got != want {
			t.Errorf("%s → %s, atteso %s", in, got, want)
		}
	}
}
