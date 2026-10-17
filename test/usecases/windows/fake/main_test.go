package main

import (
	"bytes"
	"context"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/masterzen/winrm"
)

// The real WinRM client of the windows plugin against the endpoint: stdin, output, exit code.
func TestWinRMConIlClientDelPlugin(t *testing.T) {
	srv := httptest.NewTLSServer(&endpoint{user: "deploy", pass: "pw", shells: map[string]*shell{}})
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	p, _ := strconv.Atoi(port)
	ep := winrm.NewEndpoint(host, p, true, true, nil, nil, nil, 0)
	c, err := winrm.NewClientWithParameters(ep, "deploy", "pw", winrm.NewParameters("PT60S", "en-US", 153600))
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	data := strings.Repeat("riga di prova\n", 20000)
	code, err := c.RunWithContextWithInput(context.Background(), "wc -c; echo fine >&2; exit 3", &out, &errOut, strings.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 || strings.TrimSpace(out.String()) != strconv.Itoa(len(data)) || strings.TrimSpace(errOut.String()) != "fine" {
		t.Errorf("exit %d, stdout %q, stderr %q", code, out.String(), errOut.String())
	}
	bad, _ := winrm.NewClientWithParameters(ep, "deploy", "sbagliata", winrm.NewParameters("PT60S", "en-US", 153600))
	if _, err := bad.RunWithContextWithInput(context.Background(), "true", &out, &errOut, nil); err == nil {
		t.Error("password errata accettata")
	}
}
