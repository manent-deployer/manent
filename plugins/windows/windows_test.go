package windows

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/joeg-ita/manent/internal/traffic"
	"github.com/joeg-ita/manent/pkg/plugin"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeHost simulates a Windows host: it recognizes the scripts by their output keys and keeps
// the path in service and the extracted releases.
type fakeHost struct {
	path     string // physicalPath or PathName
	releases map[string]bool
	uploaded []byte
	scripts  []string
	failOn   string
}

type fakeTransport struct{ h *fakeHost }

func (f fakeTransport) Run(_ context.Context, script string, data io.Reader) (string, error) {
	h := f.h
	h.scripts = append(h.scripts, script)
	if h.failOn != "" && strings.Contains(script, h.failOn) {
		return "", errors.New("uscita 1: errore simulato")
	}
	switch {
	case strings.Contains(script, "MANENT:found"):
		return "MANENT:found=true\r\nMANENT:path=" + h.path + "\r\n", nil
	case strings.Contains(script, "MANENT:present"):
		v := between(script, "$version = '", "'")
		present := "false"
		if h.releases[v] {
			present = "true"
		}
		return "MANENT:powershell=5.1.17763\nMANENT:present=" + present + "\n", nil
	case strings.Contains(script, "FromBase64String($l)"):
		// The same stdin as the real transports: the script line, then the data as base64 lines
		// ended by an empty line.
		sc := bufio.NewScanner(stdin(script, data))
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		sc.Scan()
		for sc.Scan() {
			if sc.Text() == "" {
				break
			}
			b, err := base64.StdEncoding.DecodeString(sc.Text())
			if err != nil {
				return "", err
			}
			h.uploaded = append(h.uploaded, b...)
		}
		h.releases[between(script, "$version = '", "'")] = true
		return "MANENT:dir=x\n", nil
	case strings.Contains(script, "MANENT:active"):
		v := between(script, "$version = '", "'")
		root := between(script, "$root = '", "'")
		switch {
		case strings.Contains(script, "Set-ScheduledTask"):
			h.path = root + `\` + v + `\bin\job.exe`
		case strings.Contains(script, "Invoke-Msiexec"):
			h.path = root + `\` + v
		case strings.Contains(script, "sc.exe"):
			h.path = `"` + root + `\` + v + `\bin\svc.exe" --port 8080`
		default:
			h.path = root + `\` + v
		}
		return "MANENT:active=" + v + "\n", nil
	}
	return "", nil
}

func (fakeTransport) Close() error { return nil }

func between(s, a, b string) string {
	_, rest, _ := strings.Cut(s, a)
	v, _, _ := strings.Cut(rest, b)
	return v
}

type fakeConnector struct {
	hosts map[string]*fakeHost
	conns []Conn
}

func (f *fakeConnector) Connect(_ context.Context, t plugin.Target, c Conn) (Transport, error) {
	f.conns = append(f.conns, c)
	return fakeTransport{h: f.hosts[t.Host]}, nil
}

type secrets map[string]string

func (s secrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", errors.New("segreto assente: " + name)
	}
	return v, nil
}

func request(t *testing.T, params map[string]string, version string) *plugin.Request {
	t.Helper()
	art := filepath.Join(t.TempDir(), "shop.zip")
	if err := os.WriteFile(art, []byte(strings.Repeat("zip!", 30000)), 0o600); err != nil {
		t.Fatal(err)
	}
	return &plugin.Request{
		DeploymentID: "d1", Application: "shop", Environment: "prod",
		Artifact: plugin.Artifact{Type: "windows", Name: "shop", Version: version, LocalPath: art, SHA256: "abc"},
		Targets:  []plugin.Target{{Host: "win-1", User: `CONTOSO\deploy`}},
		Params:   params, Secrets: secrets{"win-pass": "s3cret"},
	}
}

func iisParams() map[string]string {
	return map[string]string{ParamReleasesDir: `C:\apps\shop\releases`, ParamSite: "Default Web Site", ParamApp: "/shop", ParamAppPool: "shop", ParamPassword: "win-pass"}
}

func TestDeployIIS(t *testing.T) {
	host := &fakeHost{path: `%SystemDrive%\inetpub\wwwroot`, releases: map[string]bool{}}
	c := &fakeConnector{hosts: map[string]*fakeHost{"win-1": host}}
	p := New(c, []string{"windows"}, discard)
	req := request(t, iisParams(), "1.4.0")
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	res, err := p.Deploy(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Targets[0].Status != plugin.StatusDeployed || host.path != `C:\apps\shop\releases\1.4.0` {
		t.Errorf("deploy: %+v, path %s", res, host.path)
	}
	raw, _ := os.ReadFile(req.Artifact.LocalPath)
	if string(host.uploaded) != string(raw) {
		t.Errorf("artefatto copiato: %d byte su %d", len(host.uploaded), len(raw))
	}
	// Default connection: WinRM over HTTPS with NTLM, password from the secret.
	if cn := c.conns[0]; cn.Transport != TransportWinRM || !cn.HTTPS || cn.Port != 5986 || cn.Auth != AuthNTLM || cn.Password != "s3cret" {
		t.Errorf("connessione: %+v", cn)
	}
	act := host.scripts[len(host.scripts)-2]
	for _, want := range []string{"Set-WebConfigurationProperty", "$site = 'Default Web Site'", "$app = '/shop'", "Restart-WebAppPool"} {
		if !strings.Contains(act, want) {
			t.Errorf("script di attivazione senza %q", want)
		}
	}
	// The cleanup never removes the release in service and the previous one (none: not manent's).
	if cl := host.scripts[len(host.scripts)-1]; !strings.Contains(cl, "$protect = @('1.4.0', '')") {
		t.Errorf("pulizia: %s", cl)
	}

	// Same version again: already extracted, no copy.
	host.uploaded, host.scripts = nil, nil
	if _, err := p.Deploy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if host.uploaded != nil {
		t.Error("release già presente copiata di nuovo")
	}

	// The release in service is read from the IIS application.
	rel, err := p.CurrentRelease(context.Background(), req)
	if err != nil || rel == nil || rel.Version != "1.4.0" {
		t.Fatalf("release in esercizio: %+v %v", rel, err)
	}
	// Rollback to a kept release.
	if _, err := p.Rollback(context.Background(), req, plugin.Release{Version: "1.3.0"}); err != nil {
		t.Fatal(err)
	}
	if host.path != `C:\apps\shop\releases\1.3.0` {
		t.Errorf("dopo il rollback: %s", host.path)
	}
}

func TestDeployServizioSSH(t *testing.T) {
	host := &fakeHost{path: `C:\old\svc.exe`, releases: map[string]bool{}}
	c := &fakeConnector{hosts: map[string]*fakeHost{"win-1": host}}
	p := New(c, []string{"windows"}, discard)
	params := map[string]string{ParamMode: ModeService, ParamReleasesDir: `C:\apps\svc\releases`, ParamService: "ShopWorker",
		ParamBinary: `bin\svc.exe`, ParamArguments: "--port 8080", ParamTransport: TransportSSH, ParamSSHKey: "key", ParamShell: "pwsh"}
	req := request(t, params, "2.0.0")
	req.Secrets = secrets{"key": "PEM"}
	if _, err := p.Deploy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if cn := c.conns[0]; cn.Transport != TransportSSH || cn.PrivateKey != "PEM" || cn.Shell != "pwsh" {
		t.Errorf("connessione: %+v", cn)
	}
	if !strings.Contains(host.scripts[len(host.scripts)-2], "& sc.exe config $name binPath= $bin") {
		t.Error("attivazione del servizio senza sc.exe config")
	}
	rel, err := p.CurrentRelease(context.Background(), req)
	if err != nil || rel == nil || rel.Version != "2.0.0" {
		t.Fatalf("release dal PathName del servizio: %+v %v", rel, err)
	}

	// A failed activation fails the deploy on that target.
	host.failOn = "MANENT:active"
	req.Artifact.Version = "2.1.0"
	res, err := p.Deploy(context.Background(), req)
	if err == nil || res.Targets[0].Status != plugin.StatusFailed || !strings.Contains(err.Error(), "activation") {
		t.Errorf("attivazione fallita: %+v %v", res, err)
	}
}

func TestParametriNonValidi(t *testing.T) {
	for want, params := range map[string]map[string]string{
		"releases_dir":       {ParamSite: "s", ParamPassword: "p"},
		"site":               {ParamReleasesDir: `C:\r`, ParamSite: "x'; Remove-Item C:\\", ParamPassword: "p"},
		"binary":             {ParamMode: ModeService, ParamReleasesDir: `C:\r`, ParamService: "svc", ParamBinary: `..\evil.exe`, ParamPassword: "p"},
		"password_secret":    {ParamReleasesDir: `C:\r`, ParamSite: "s"},
		"winrm_https":        {ParamReleasesDir: `C:\r`, ParamSite: "s", ParamPassword: "p", ParamAuth: AuthBasic, ParamHTTPS: "false"},
		"keep_releases":      {ParamReleasesDir: `C:\r`, ParamSite: "s", ParamPassword: "p", ParamKeep: "1"},
		"mode":               {ParamReleasesDir: `C:\r`, ParamMode: "msi", ParamPassword: "p"},
		"ssh_key_secret":     {ParamReleasesDir: `C:\r`, ParamSite: "s", ParamTransport: TransportSSH},
		"powershell or pwsh": {ParamReleasesDir: `C:\r`, ParamSite: "s", ParamPassword: "p", ParamShell: "bash"},
		"task_path":          {ParamMode: ModeTask, ParamReleasesDir: `C:\r`, ParamTask: "job", ParamBinary: "j.exe", ParamTaskPath: `Shop`, ParamPassword: "p"},
		"task_user":          {ParamMode: ModeTask, ParamReleasesDir: `C:\r`, ParamTask: "job", ParamBinary: "j.exe", ParamTaskUser: `CONTOSO\svc`, ParamPassword: "p"},
		"product_name":       {ParamMode: ModeMSI, ParamReleasesDir: `C:\r`, ParamPassword: "p"},
		"msi_properties":     {ParamMode: ModeMSI, ParamReleasesDir: `C:\r`, ParamProduct: "Shop", ParamMSIProps: `INSTALLDIR=C:\x lower=1`, ParamPassword: "p"},
	} {
		if _, err := parse(params); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("atteso errore su %q, ricevuto %v", want, err)
		}
	}
}

func TestReleaseOf(t *testing.T) {
	cases := map[string]string{
		`C:\apps\shop\releases\1.4.0`:     "1.4.0",
		`c:/APPS/shop/releases/1.4.0/web`: "1.4.0",
		`C:\apps\shop\releases`:           "",
		`C:\inetpub\wwwroot`:              "",
		`C:\apps\shop\releases\..\x`:      "",
	}
	for p, want := range cases {
		if got := releaseOf(`C:\apps\shop\releases\`, p); got != want {
			t.Errorf("%s: %q, atteso %q", p, got, want)
		}
	}
	if got := executable(`"C:\a b\svc.exe" --x`); got != `C:\a b\svc.exe` {
		t.Errorf("eseguibile tra virgolette: %q", got)
	}
	if got := executable(`C:\a\svc.exe --x`); got != `C:\a\svc.exe` {
		t.Errorf("eseguibile: %q", got)
	}
}

func TestBootstrapEStdin(t *testing.T) {
	// The command line stays within the cmd.exe limit, whatever the script.
	if n := len(bootstrap("powershell")); n > 8191 {
		t.Errorf("riga di comando di %d caratteri", n)
	}
	data := strings.Repeat("x", chunk+10)
	raw, _ := io.ReadAll(stdin("Write-Output 1", strings.NewReader(data)))
	lines := strings.Split(string(raw), "\n")
	if s, _ := base64.StdEncoding.DecodeString(lines[0]); string(s) != "Write-Output 1" {
		t.Errorf("prima riga: %q", lines[0])
	}
	var got []byte
	for _, l := range lines[1:] {
		if l == "" {
			break
		}
		b, _ := base64.StdEncoding.DecodeString(l)
		got = append(got, b...)
	}
	if string(got) != data || len(lines) != 5 {
		t.Errorf("dati: %d byte in %d righe", len(got), len(lines))
	}
	if psQuote("it's") != "'it''s'" {
		t.Error("psQuote")
	}
	if r := results("rumore\r\nMANENT:a=1\nMANENT:b=x=y\n"); r["a"] != "1" || r["b"] != "x=y" {
		t.Errorf("risultati: %v", r)
	}
}

func TestDeployTaskEMSI(t *testing.T) {
	host := &fakeHost{path: `C:\old\job.exe`, releases: map[string]bool{}}
	c := &fakeConnector{hosts: map[string]*fakeHost{"win-1": host}}
	p := New(c, []string{"windows"}, discard)
	params := map[string]string{ParamMode: ModeTask, ParamReleasesDir: `C:\apps\job\releases`, ParamTask: "Shop Export", ParamTaskPath: `\Shop\`,
		ParamBinary: `bin\job.exe`, ParamArguments: "--all", ParamTaskUser: `CONTOSO\svc-job`, ParamTaskSecret: "job-pass", ParamStopTask: "true", ParamPassword: "win-pass"}
	req := request(t, params, "3.0.0")
	req.Secrets = secrets{"win-pass": "s3cret", "job-pass": "j0b"}
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	act := host.scripts[len(host.scripts)-2]
	for _, want := range []string{"$name = 'Shop Export'", `$tpath = '\Shop\'`, "Stop-ScheduledTask", "-User $user -Password $password", "$password = 'j0b'", "$arguments = '--all'"} {
		if !strings.Contains(act, want) {
			t.Errorf("attivazione dell'attività senza %q", want)
		}
	}
	if rel, err := p.CurrentRelease(context.Background(), req); err != nil || rel == nil || rel.Version != "3.0.0" {
		t.Fatalf("release dall'azione dell'attività: %+v %v", rel, err)
	}

	// MSI: the package is kept, not extracted; uninstall only on rollback (or uninstall_previous).
	host = &fakeHost{releases: map[string]bool{}}
	c.hosts["win-1"] = host
	params = map[string]string{ParamMode: ModeMSI, ParamReleasesDir: `C:\pkg\shop`, ParamProduct: "Shop Agent", ParamMSIProps: "PORT=8080 ENV=prod", ParamPassword: "win-pass"}
	req = request(t, params, "4.1.0")
	req.Artifact.LocalPath = strings.TrimSuffix(req.Artifact.LocalPath, ".zip") + ".msi"
	if err := os.WriteFile(req.Artifact.LocalPath, []byte("msi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(req); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Deploy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var upload string
	for _, sc := range host.scripts {
		if strings.Contains(sc, "FromBase64String($l)") {
			upload = sc
		}
	}
	if !strings.Contains(upload, "$msi = 'true'") {
		t.Error("pacchetto MSI estratto come uno zip")
	}
	act = host.scripts[len(host.scripts)-2]
	for _, want := range []string{"$product = 'Shop Agent'", "$props = @('PORT=8080', 'ENV=prod')", "$uninstall = 'false'", "Win32_InstalledWin32Program"} {
		if !strings.Contains(act, want) {
			t.Errorf("installazione MSI senza %q", want)
		}
	}
	if rel, err := p.CurrentRelease(context.Background(), req); err != nil || rel == nil || rel.Version != "4.1.0" {
		t.Fatalf("release MSI: %+v %v", rel, err)
	}
	if _, err := p.Rollback(context.Background(), req, plugin.Release{Version: "4.0.0"}); err != nil {
		t.Fatal(err)
	}
	if last := host.scripts[len(host.scripts)-1]; !strings.Contains(last, "$uninstall = 'true'") || host.path != `C:\pkg\shop\4.0.0` {
		t.Errorf("rollback MSI senza disinstallazione: %s", host.path)
	}
	bad := request(t, params, "4.1.0")
	if err := p.Validate(bad); err == nil || !strings.Contains(err.Error(), ".msi") {
		t.Errorf("zip accettato in mode msi: %v", err)
	}
}

// poolTransport answers the pool scripts with the given members and records the scripts.
type poolTransport struct {
	members string
	scripts *[]string
}

func (f poolTransport) Run(_ context.Context, script string, _ io.Reader) (string, error) {
	*f.scripts = append(*f.scripts, script)
	return "MANENT:members=" + f.members + "\r\n", nil
}

func (poolTransport) Close() error { return nil }

func TestPool(t *testing.T) {
	var scripts []string
	var conns []Conn
	members := `[{"name":"web1","state":"0","healthy":true,"enabled":true,"requests":3},{"name":"web2","state":"1","healthy":true,"enabled":true,"requests":0}]`
	connect := func(_ context.Context, _ map[string]string, tg plugin.Target, c Conn) (Transport, error) {
		if tg.Host != "arr1" || tg.User != `CONTOSO\lb` {
			t.Errorf("host di gestione: %+v", tg)
		}
		conns = append(conns, c)
		return poolTransport{members: members, scripts: &scripts}, nil
	}
	settings := map[string]string{SettingHost: "arr1", SettingUser: `CONTOSO\lb`, SettingFarm: "shop-farm", ParamPassword: "lb-pass"}
	if err := ValidateARR(map[string]string{SettingHost: "arr1"}); err == nil || !strings.Contains(err.Error(), "farm") {
		t.Errorf("farm mancante: %v", err)
	}
	pl, err := NewPoolFactory(PoolARR, connect)(context.Background(), traffic.Env{Settings: settings, Secrets: secrets{"lb-pass": "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	ms, err := pl.Members(context.Background())
	if err != nil || len(ms) != 2 || !ms[0].InService() || ms[1].State != traffic.StateDraining {
		t.Fatalf("membri: %+v %v", ms, err)
	}
	if conns[0].Password != "pw" || !strings.Contains(scripts[0], "$farmName = 'shop-farm'") {
		t.Errorf("connessione o farm: %+v", conns[0])
	}
	if err := pl.Drain(context.Background(), []string{"web1"}); err != nil {
		t.Fatal(err)
	}
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "$members = @('web1')") || !strings.Contains(last, "Set-State $members 1") {
		t.Errorf("svuotamento ARR: %s", last)
	}
	if n, err := pl.Connections(context.Background(), []string{"web1"}); err != nil || n["web1"] != 3 {
		t.Errorf("richieste in corso: %v %v", n, err)
	}
	if _, err := pl.Connections(context.Background(), []string{"web9"}); err == nil {
		t.Error("membro assente accettato")
	}
	if err := pl.Drain(context.Background(), []string{"web1'; Remove-Item C:\\"}); err == nil {
		t.Error("nome di membro non valido accettato")
	}

	members = `[{"name":"NODE1","state":"Converged","healthy":true,"enabled":true},{"name":"node2","state":"Draining","healthy":false,"enabled":true}]`
	nlb, err := NewPoolFactory(PoolNLB, connect)(context.Background(), traffic.Env{Settings: map[string]string{SettingHost: "arr1", SettingUser: `CONTOSO\lb`,
		SettingDrainTimeout: "90s", ParamPassword: "lb-pass"}, Secrets: secrets{"lb-pass": "pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := nlb.Drain(context.Background(), []string{"node2"}); err != nil {
		t.Fatal(err)
	}
	if last := scripts[len(scripts)-1]; !strings.Contains(last, "Stop-NlbClusterNode -HostName $m -Drain -Timeout 2") {
		t.Errorf("svuotamento NLB: %s", last)
	}
	if n, _ := nlb.Connections(context.Background(), []string{"node2", "node1"}); n["node2"] != 1 || n["node1"] != 0 {
		t.Errorf("connessioni NLB: %v", n)
	}
	if h, _ := nlb.Healthy(context.Background(), []string{"node1", "node2"}); !h["node1"] || h["node2"] {
		t.Errorf("salute NLB: %v", h)
	}
}
