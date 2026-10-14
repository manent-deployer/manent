package customspec

import (
	"strings"
	"testing"
	"time"
)

func commandSpec() Spec {
	s := Spec{
		Type:    TypeCommand,
		Secrets: []string{"db-admin"},
		Params:  map[string]Param{"database": {Pattern: "[a-z][a-z0-9_]{0,62}", Required: true}},
		Outputs: map[string]Output{"applied": {Pattern: "[0-9]{1,6}"}},
		SSH:     &SSH{KeySecret: "ssh-key"},
		Run: &Action{
			RunOnce: true,
			Command: []string{"/opt/tools/migrate", "up", "--to", "{{ .Version }}", "--db", "{{ .Params.database }}"},
		},
		DryRun: &Action{Command: []string{"/opt/tools/migrate", "plan"}, Secrets: []string{"db-readonly"}},
	}
	s.ApplyDefaults()
	return s
}

func httpSpec() Spec {
	s := Spec{
		Type:    TypeHTTP,
		Secrets: []string{"cdn-token"},
		Params:  map[string]Param{"zone": {Pattern: "[a-f0-9]{32}"}},
		Outputs: map[string]Output{"purge_id": {Pattern: "[a-z0-9-]{1,64}", Path: "$.result.id"}},
		Run: &Action{
			URL:  "https://api.cdn.example.com/zones/{{ .Params.zone }}/purge",
			Auth: &Auth{Type: "bearer", Secret: "cdn-token"},
			JSON: map[string]any{"tags": []any{"{{ .Application }}"}},
			Poll: &Poll{URL: "https://api.cdn.example.com/purges/{{ .Response.result.id }}", Until: Condition{Path: "$.status", In: []string{"done"}}},
		},
	}
	s.ApplyDefaults()
	return s
}

func TestValidazione(t *testing.T) {
	if err := commandSpec().Validate("db-migrate", false); err != nil {
		t.Fatalf("specifica command valida: %v", err)
	}
	h := httpSpec()
	if err := h.Validate("cdn-purge", false); err != nil {
		t.Fatalf("specifica http valida: %v", err)
	}
	if h.Run.Method != "POST" || h.Timeout != DefaultTimeout || h.Run.Poll.Interval != DefaultPollInterval || h.Run.Poll.Timeout != DefaultTimeout {
		t.Errorf("default non applicati: %+v", h.Run)
	}
	if err := (Spec{}).Validate("deploy", false); err != nil {
		t.Errorf("step standard: %v", err)
	}

	cases := map[string]struct {
		edit func(*Spec)
		want string
	}{
		"campi senza type":         {func(s *Spec) { s.Type = "" }, "richiedono type"},
		"binario relativo":         {func(s *Spec) { s.Run.Command[0] = "migrate" }, "percorso assoluto"},
		"binario da segnaposto":    {func(s *Spec) { s.Run.Command[0] = "/opt/{{ .Params.database }}" }, "percorso assoluto"},
		"radice sconosciuta":       {func(s *Spec) { s.Run.Command[3] = "{{ .Env.HOME }}" }, "segnaposto .Env sconosciuto"},
		"parametro non dichiarato": {func(s *Spec) { s.Run.Command[3] = "{{ .Params.other }}" }, `parametro "other" non dichiarato`},
		"sintassi":                 {func(s *Spec) { s.Run.Command[3] = "{{ .Version " }, "command[3]"},
		"pattern mancante":         {func(s *Spec) { s.Params["x"] = Param{} }, "params.x.pattern obbligatorio"},
		"default non conforme":     {func(s *Spec) { s.Params["x"] = Param{Pattern: "[0-9]+", Default: "a"} }, "default non rispetta"},
		"output senza run_once":    {func(s *Spec) { s.Run.RunOnce = false }, "run_once"},
		"senza credenziali":        {func(s *Spec) { s.SSH = nil }, "ssh.key_secret"},
		"campi http":               {func(s *Spec) { s.Run.URL = "https://x/" }, "campi http"},
		"segreti di run":           {func(s *Spec) { s.Run.Secrets = []string{"x"} }, "dentro dry_run"},
		"timeout":                  {func(s *Spec) { s.Timeout = 7 * time.Hour }, "timeout"},
		"nome segreto":             {func(s *Spec) { s.Secrets = []string{"a b"} }, "non valido"},
	}
	for name, c := range cases {
		s := commandSpec()
		c.edit(&s)
		err := s.Validate("db-migrate", false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: errore %v, atteso %q", name, err, c.want)
		}
	}

	httpCases := map[string]struct {
		edit func(*Spec)
		want string
	}{
		"host da segnaposto":   {func(s *Spec) { s.Run.URL = "https://{{ .Params.zone }}.example.com/" }, "senza segnaposto"},
		"host senza barra":     {func(s *Spec) { s.Run.URL = "https://api.example.com{{ .Params.zone }}" }, "seguito da '/'"},
		"http in chiaro":       {func(s *Spec) { s.Run.URL = "http://api.example.com/" }, `schema "http" non ammesso`},
		"header riservato":     {func(s *Spec) { s.Run.Headers = map[string]string{"Host": "x"} }, "nome non ammesso"},
		"authorization doppia": {func(s *Spec) { s.Run.Headers = map[string]string{"Authorization": "x"} }, "già impostato da auth"},
		"auth non dichiarata":  {func(s *Spec) { s.Secrets = nil }, "run.auth.secret"},
		"output senza path":    {func(s *Spec) { s.Outputs["x"] = Output{Pattern: ".*"} }, "path obbligatorio"},
		"path non valido":      {func(s *Spec) { s.Outputs["x"] = Output{Pattern: ".*", Path: "a.b"} }, "inizia con $"},
		"response fuori poll":  {func(s *Spec) { s.Run.URL = "https://api.cdn.example.com/{{ .Response.id }}" }, ".Response sconosciuto"},
		"json con GET":         {func(s *Spec) { s.Run.Method = "GET" }, "json non ammesso"},
		"poll oltre timeout":   {func(s *Spec) { s.Run.Poll.Timeout = time.Hour }, "poll.timeout"},
		"ssh su http":          {func(s *Spec) { s.SSH = &SSH{KeySecret: "k"} }, "ssh si usa solo"},
	}
	for name, c := range httpCases {
		s := httpSpec()
		c.edit(&s)
		err := s.Validate("cdn-purge", false)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: errore %v, atteso %q", name, err, c.want)
		}
	}
	s := httpSpec()
	s.Run.URL, s.Run.Poll = "http://api.example.com/x", nil
	if err := s.Validate("cdn-purge", true); err != nil {
		t.Errorf("http ammesso da allow_http: %v", err)
	}
}

func TestFirmaERevoca(t *testing.T) {
	doc, err := Encode("db-migrate", commandSpec())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Encode("db-migrate", commandSpec())
	if doc != again || len(Hash(doc)) != 64 {
		t.Fatalf("forma canonica non deterministica")
	}
	back, err := Decode(doc)
	if err != nil || back.Name != "db-migrate" || back.Run.Command[0] != "/opt/tools/migrate" || back.Timeout != DefaultTimeout {
		t.Fatalf("decodifica: %+v, %v", back, err)
	}
	if redo, _ := Encode(back.Name, back.Spec); redo != doc {
		t.Errorf("la specifica decodificata non ha la stessa forma canonica")
	}

	old := []byte(strings.Repeat("o", 32))
	cur := []byte(strings.Repeat("k", 32))
	sig := Keys{old}.Sign(doc)
	keys := Keys{cur, old}
	if err := keys.Verify(doc, sig); err != nil {
		t.Errorf("firma con la chiave precedente rifiutata: %v", err)
	}
	if err := keys.Verify(doc, keys.Sign(doc)); err != nil {
		t.Errorf("firma corrente rifiutata: %v", err)
	}
	if err := keys.Verify(strings.Replace(doc, "migrate", "evil", 1), sig); err == nil {
		t.Error("specifica alterata accettata")
	}
	if err := keys.Verify(doc, ""); err == nil {
		t.Error("specifica non firmata accettata con chiavi configurate")
	}
	if err := (Keys{}).Verify(doc, ""); err != nil || (Keys{}).Sign(doc) != "" {
		t.Errorf("senza chiavi: %v", err)
	}
}

func TestParametri(t *testing.T) {
	s := commandSpec()
	s.Params["mode"] = Param{Pattern: "fast|safe", Default: "safe"}
	p, err := s.EffectiveParams(map[string]string{"database": "shop"}, true)
	if err != nil || p["database"] != "shop" || p["mode"] != "safe" {
		t.Fatalf("parametri: %v, %v", p, err)
	}
	if _, err := s.EffectiveParams(nil, false); err != nil {
		t.Errorf("obbligatorio mancante prima della configurazione di ambiente: %v", err)
	}
	for _, values := range []map[string]string{nil, {"database": "Shop; rm"}, {"database": "shop", "x": "1"}, {"database": "shop", "mode": "fastest"}} {
		if _, err := s.EffectiveParams(values, true); err == nil {
			t.Errorf("parametri %v accettati", values)
		}
	}
	if got := s.DeclaredSecrets(); strings.Join(got, ",") != "db-admin,db-readonly,ssh-key" {
		t.Errorf("segreti dichiarati: %v", got)
	}
}

func TestPercorsiJSON(t *testing.T) {
	doc := map[string]any{"result": map[string]any{"id": "p-1", "items": []any{map[string]any{"n": 3.0}}, "a.b": true}}
	for path, want := range map[string]string{
		"$.result.id": "p-1", "$.result.items[0].n": "3", `$.result["a.b"]`: "true",
	} {
		if got, ok := Lookup(doc, path); !ok || got != want {
			t.Errorf("%s = %q, %v", path, got, ok)
		}
	}
	for _, path := range []string{"$.result", "$.result.items[1].n", "$.missing", "$.result.id.x"} {
		if v, ok := Lookup(doc, path); ok {
			t.Errorf("%s: valore %q inatteso", path, v)
		}
	}
	for _, bad := range []string{"", "result.id", "$", "$..id", "$.a[*]", "$.a[-1]", `$["a`} {
		if _, err := ParsePath(bad); err == nil {
			t.Errorf("percorso %q accettato", bad)
		}
	}
}
