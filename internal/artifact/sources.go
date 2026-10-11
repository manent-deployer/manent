package artifact

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/joeg-ita/manent/internal/secrets"
)

// Tipi di autenticazione verso una sorgente di artefatti.
const (
	AuthBasic  = "basic"  // Authorization: Basic <username>:<segreto>
	AuthBearer = "bearer" // Authorization: Bearer <segreto>
	AuthHeader = "header" // <header>: <segreto> (es. X-JFrog-Art-Api)
)

// SourceAuth sono le credenziali di una sorgente. SecretRef è un riferimento del secret
// provider (es. "vault:secret/data/manent/nexus#password"), risolto a ogni download.
type SourceAuth struct {
	Type      string
	Username  string
	Header    string
	SecretRef string
}

// Source è una sorgente di artefatti configurata, identificata da un prefisso dell'URL
// (es. "https://nexus.example.com/repository/releases/" o "file:///srv/artifacts/").
type Source struct {
	Prefix string
	Auth   *SourceAuth
	// CAFile è un bundle di CA aggiuntive per la sorgente.
	CAFile string
	client *http.Client
}

// Sources applica le sorgenti configurate: quali sono ammesse e con quali credenziali.
type Sources struct {
	list []Source
	// AllowUnlisted ammette anche le sorgenti che non corrispondono a nessun prefisso
	// (senza credenziali).
	AllowUnlisted bool
	Secrets       secrets.Provider
	// Client è il client HTTP delle sorgenti senza CA dedicata.
	Client *http.Client
}

// NewSources verifica le sorgenti e prepara i client con le CA dedicate.
func NewSources(list []Source, allowUnlisted bool, sp secrets.Provider, client *http.Client) (*Sources, error) {
	s := &Sources{AllowUnlisted: allowUnlisted, Secrets: sp, Client: client}
	for i, src := range list {
		if !strings.HasPrefix(src.Prefix, "https://") && !strings.HasPrefix(src.Prefix, "http://") && !strings.HasPrefix(src.Prefix, "file:///") && !IsOCI(src.Prefix) {
			return nil, fmt.Errorf("artifacts.sources[%d]: prefix deve iniziare con https://, http://, file:///, oci:// o oci+http://", i)
		}
		if a := src.Auth; a != nil {
			if strings.HasPrefix(src.Prefix, "file:") {
				return nil, fmt.Errorf("artifacts.sources[%d]: auth non applicabile a una sorgente file", i)
			}
			switch {
			case a.Type != AuthBasic && a.Type != AuthBearer && a.Type != AuthHeader:
				return nil, fmt.Errorf("artifacts.sources[%d].auth.type %q non valido (basic|bearer|header)", i, a.Type)
			case a.SecretRef == "":
				return nil, fmt.Errorf("artifacts.sources[%d].auth.secret_ref obbligatorio", i)
			case a.Type == AuthBasic && a.Username == "":
				return nil, fmt.Errorf("artifacts.sources[%d].auth.username obbligatorio con basic", i)
			case a.Type == AuthHeader && a.Header == "":
				return nil, fmt.Errorf("artifacts.sources[%d].auth.header obbligatorio con header", i)
			}
		}
		if src.CAFile != "" {
			c, err := clientWithCA(client, src.CAFile)
			if err != nil {
				return nil, fmt.Errorf("artifacts.sources[%d]: %w", i, err)
			}
			src.client = c
		}
		s.list = append(s.list, src)
	}
	return s, nil
}

// clientWithCA restituisce una copia del client che si fida anche delle CA del file indicato.
func clientWithCA(base *http.Client, caFile string) (*http.Client, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("lettura ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("ca_file non contiene certificati PEM")
	}
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	return c, nil
}

// normalize porta la sorgente nella forma confrontata con i prefissi: gli URL http(s) e le
// immagini oci restano invariati, i percorsi locali diventano "file://" più il percorso assoluto ripulito.
func normalize(source string) (string, error) {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") || IsOCI(source) {
		return source, nil
	}
	abs, err := filepath.Abs(strings.TrimPrefix(source, "file://"))
	if err != nil {
		return "", err
	}
	return "file://" + filepath.ToSlash(abs), nil
}

// match restituisce la sorgente configurata con il prefisso più lungo, o nil.
func (s *Sources) match(source string) (*Source, error) {
	norm, err := normalize(source)
	if err != nil {
		return nil, err
	}
	var best *Source
	for i := range s.list {
		if strings.HasPrefix(norm, s.list[i].Prefix) && (best == nil || len(s.list[i].Prefix) > len(best.Prefix)) {
			best = &s.list[i]
		}
	}
	return best, nil
}

// Check verifica che la sorgente sia ammessa.
func (s *Sources) Check(source string) error {
	if s == nil {
		return nil
	}
	src, err := s.match(source)
	if err != nil {
		return err
	}
	if src == nil && !s.AllowUnlisted {
		return fmt.Errorf("sorgente dell'artefatto non ammessa: non corrisponde a nessuna voce di artifacts.sources")
	}
	return nil
}

// prepare restituisce il client da usare per la richiesta e vi aggiunge le credenziali della
// sorgente, lette dal secret provider al momento del download.
func (s *Sources) prepare(ctx context.Context, req *http.Request, source string) (*http.Client, error) {
	if s == nil {
		return nil, nil
	}
	src, err := s.match(source)
	if err != nil {
		return nil, err
	}
	if src == nil {
		if !s.AllowUnlisted {
			return nil, s.Check(source)
		}
		return s.Client, nil
	}
	if a := src.Auth; a != nil {
		secret, err := s.Secrets.Resolve(ctx, a.SecretRef)
		if err != nil {
			return nil, fmt.Errorf("credenziali della sorgente %s: %w", src.Prefix, err)
		}
		switch a.Type {
		case AuthBasic:
			req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(a.Username+":"+secret)))
		case AuthBearer:
			req.Header.Set("Authorization", "Bearer "+secret)
		case AuthHeader:
			req.Header.Set(a.Header, secret)
		}
	}
	if src.client != nil {
		return src.client, nil
	}
	return s.Client, nil
}
