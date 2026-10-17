// Command manent-uc-windows emulates, for the use cases, what a Windows host offers to the
// windows plugin besides SSH: a WinRM endpoint (WS-Management over HTTPS with basic
// authentication) that runs the commands with a real PowerShell, and the IIS web server that
// serves an application from its physicalPath while its application pool is started. The IIS
// configuration and the services are kept by the PowerShell modules of the image in
// /srv/win/state (iis.json, services.json).
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	nsShell = "http://schemas.microsoft.com/wbem/wsman/1/windows/shell"
	done    = nsShell + "/CommandState/Done"
	running = nsShell + "/CommandState/Running"
)

func main() {
	user, pass := env("WINRM_USER", "deploy"), os.Getenv("WINRM_PASSWORD")
	if pass == "" {
		log.Fatal("WINRM_PASSWORD obbligatoria")
	}
	w := &endpoint{user: user, pass: pass, shells: map[string]*shell{}}
	go func() {
		srv := &http.Server{Addr: ":5986", Handler: w, TLSConfig: &tls.Config{Certificates: []tls.Certificate{selfSigned()}}}
		log.Fatal(srv.ListenAndServeTLS("", ""))
	}()
	log.Fatal(http.ListenAndServe(":8080", iis{state: env("STATE_DIR", "/srv/win/state")}))
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// selfSigned creates the certificate of the WinRM HTTPS listener (the plugin uses
// winrm_insecure_skip_verify in the lab).
func selfSigned() tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "win1"}, DNSNames: []string{"win1"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour)}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// endpoint is the WS-Management shell endpoint: Create, Command, Send, Receive, Signal, Delete.
type endpoint struct {
	user, pass string
	mu         sync.Mutex
	shells     map[string]*shell
	seq        int
}

type shell struct {
	commands map[string]*command
}

// command is a running process with the output not yet received by the client.
type command struct {
	mu     sync.Mutex
	stdin  io.WriteCloser
	out    bytes.Buffer
	errOut bytes.Buffer
	exited bool
	code   int
	cmd    *exec.Cmd
	wake   chan struct{}
}

func (c *command) notify() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

type lockedWriter struct {
	c   *command
	buf *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.c.mu.Lock()
	n, err := w.buf.Write(p)
	w.c.mu.Unlock()
	w.c.notify()
	return n, err
}

// request is what the endpoint reads from a WS-Management message.
type request struct {
	action, shellID, commandID, command string
	stream                              string
	end                                 bool
}

func parse(body []byte) (request, error) {
	var r request
	dec := xml.NewDecoder(bytes.NewReader(body))
	var path []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return r, nil
		}
		if err != nil {
			return r, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			path = append(path, t.Name.Local)
			for _, a := range t.Attr {
				switch {
				case a.Name.Local == "CommandId":
					r.commandID = a.Value
				case a.Name.Local == "End" && a.Value == "true":
					r.end = true
				}
			}
			if t.Name.Local == "Selector" {
				for _, a := range t.Attr {
					if a.Name.Local == "Name" && a.Value == "ShellId" {
						var v string
						if err := dec.DecodeElement(&v, &t); err != nil {
							return r, err
						}
						r.shellID = strings.TrimSpace(v)
						path = path[:len(path)-1]
					}
				}
			}
		case xml.EndElement:
			path = path[:len(path)-1]
		case xml.CharData:
			if len(path) == 0 {
				continue
			}
			switch path[len(path)-1] {
			case "Action":
				r.action += strings.TrimSpace(string(t))
			case "Command":
				r.command += string(t)
			case "Stream":
				r.stream += strings.TrimSpace(string(t))
			}
		}
	}
}

func (w *endpoint) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if u, p, ok := req.BasicAuth(); !ok || u != w.user || p != w.pass {
		rw.Header().Set("WWW-Authenticate", `Basic realm="WSMAN"`)
		http.Error(rw, "unauthorized", http.StatusUnauthorized)
		return
	}
	body, _ := io.ReadAll(req.Body)
	r, err := parse(body)
	if err != nil {
		http.Error(rw, err.Error(), http.StatusBadRequest)
		return
	}
	var action, content string
	switch {
	case strings.HasSuffix(r.action, "/transfer/Create"):
		w.mu.Lock()
		w.seq++
		id := fmt.Sprintf("shell-%d", w.seq)
		w.shells[id] = &shell{commands: map[string]*command{}}
		w.mu.Unlock()
		action, content = "http://schemas.xmlsoap.org/ws/2004/09/transfer/CreateResponse", "<rsp:Shell><rsp:ShellId>"+id+"</rsp:ShellId></rsp:Shell>"
	case strings.HasSuffix(r.action, "/shell/Command"):
		id, err := w.start(r.shellID, strings.TrimSpace(r.command))
		if err != nil {
			http.Error(rw, err.Error(), http.StatusInternalServerError)
			return
		}
		action, content = nsShell+"/CommandResponse", "<rsp:CommandResponse><rsp:CommandId>"+id+"</rsp:CommandId></rsp:CommandResponse>"
	case strings.HasSuffix(r.action, "/shell/Send"):
		if c := w.find(r.shellID, r.commandID); c != nil {
			if data, err := base64.StdEncoding.DecodeString(r.stream); err == nil && len(data) > 0 {
				_, _ = c.stdin.Write(data)
			}
			if r.end {
				_ = c.stdin.Close()
			}
		}
		action, content = nsShell+"/SendResponse", "<rsp:SendResponse/>"
	case strings.HasSuffix(r.action, "/shell/Receive"):
		c := w.find(r.shellID, r.commandID)
		if c == nil {
			http.Error(rw, "comando sconosciuto", http.StatusBadRequest)
			return
		}
		action, content = nsShell+"/ReceiveResponse", c.receive(r.commandID)
	case strings.HasSuffix(r.action, "/shell/Signal"):
		if c := w.find(r.shellID, r.commandID); c != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		action, content = nsShell+"/SignalResponse", "<rsp:SignalResponse/>"
	case strings.HasSuffix(r.action, "/transfer/Delete"):
		w.mu.Lock()
		delete(w.shells, r.shellID)
		w.mu.Unlock()
		action, content = "http://schemas.xmlsoap.org/ws/2004/09/transfer/DeleteResponse", ""
	default:
		http.Error(rw, "azione non supportata: "+r.action, http.StatusBadRequest)
		return
	}
	rw.Header().Set("Content-Type", "application/soap+xml;charset=UTF-8")
	fmt.Fprintf(rw, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:rsp="%s">`+
		`<s:Header><a:Action>%s</a:Action></s:Header><s:Body>%s</s:Body></s:Envelope>`, nsShell, action, content)
}

// start runs the command line with the shell of the container, as cmd.exe would on Windows.
func (w *endpoint) start(shellID, line string) (string, error) {
	w.mu.Lock()
	sh, ok := w.shells[shellID]
	if !ok {
		w.mu.Unlock()
		return "", fmt.Errorf("shell %s sconosciuta", shellID)
	}
	w.seq++
	id := fmt.Sprintf("cmd-%d", w.seq)
	c := &command{wake: make(chan struct{}, 1)}
	sh.commands[id] = c
	w.mu.Unlock()
	c.cmd = exec.Command("/bin/sh", "-c", line)
	c.cmd.Stdout, c.cmd.Stderr = lockedWriter{c, &c.out}, lockedWriter{c, &c.errOut}
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	c.stdin = stdin
	if err := c.cmd.Start(); err != nil {
		return "", err
	}
	go func() {
		err := c.cmd.Wait()
		c.mu.Lock()
		c.exited, c.code = true, c.cmd.ProcessState.ExitCode()
		if err != nil && c.code == 0 {
			c.code = 1
		}
		c.mu.Unlock()
		c.notify()
	}()
	return id, nil
}

func (w *endpoint) find(shellID, commandID string) *command {
	w.mu.Lock()
	defer w.mu.Unlock()
	if sh, ok := w.shells[shellID]; ok {
		return sh.commands[commandID]
	}
	return nil
}

// receive returns the output produced so far, waiting up to one second for some, and the end
// of the command with its exit code once everything has been read.
func (c *command) receive(id string) string {
	deadline := time.After(time.Second)
	for {
		c.mu.Lock()
		if c.out.Len() > 0 || c.errOut.Len() > 0 || c.exited {
			break
		}
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-deadline:
			c.mu.Lock()
			goto out
		}
	}
out:
	defer c.mu.Unlock()
	var b strings.Builder
	b.WriteString("<rsp:ReceiveResponse>")
	for name, buf := range map[string]*bytes.Buffer{"stdout": &c.out, "stderr": &c.errOut} {
		if buf.Len() > 0 {
			fmt.Fprintf(&b, `<rsp:Stream Name="%s" CommandId="%s">%s</rsp:Stream>`, name, id, base64.StdEncoding.EncodeToString(buf.Bytes()))
			buf.Reset()
		}
	}
	if c.exited {
		fmt.Fprintf(&b, `<rsp:CommandState CommandId="%s" State="%s"><rsp:ExitCode>%d</rsp:ExitCode></rsp:CommandState>`, id, done, c.code)
	} else {
		fmt.Fprintf(&b, `<rsp:CommandState CommandId="%s" State="%s"/>`, id, running)
	}
	b.WriteString("</rsp:ReceiveResponse>")
	return b.String()
}

// iis serves /<application>/<file> from the physicalPath of the application of the
// "Default Web Site", 503 if its application pool is not started.
type iis struct{ state string }

type iisState struct {
	Sites map[string]map[string]struct {
		Path string `json:"path"`
		Pool string `json:"pool"`
	} `json:"sites"`
	Pools map[string]struct {
		State string `json:"state"`
	} `json:"pools"`
}

func (s iis) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	raw, err := os.ReadFile(filepath.Join(s.state, "iis.json"))
	var st iisState
	if err != nil || json.Unmarshal(raw, &st) != nil {
		http.Error(rw, "configurazione IIS non leggibile", http.StatusInternalServerError)
		return
	}
	first, rest, _ := strings.Cut(strings.TrimPrefix(req.URL.Path, "/"), "/")
	app, ok := st.Sites["Default Web Site"]["/"+first]
	if !ok {
		http.NotFound(rw, req)
		return
	}
	if st.Pools[app.Pool].State != "Started" {
		http.Error(rw, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	http.ServeFile(rw, req, filepath.Join(app.Path, filepath.Clean("/"+rest)))
}
