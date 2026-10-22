// Command gitserver serves, for the use case lab (UC27), the Git repositories under a root over
// smart HTTP, through git http-backend, to clients authenticated with a token (basic auth,
// user manent). Never use it outside the lab.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	root := flag.String("root", "/srv/git", "directory of the bare repositories")
	tokenFile := flag.String("token-file", "/keys/git-token", "file with the token of the user manent")
	flag.Parse()
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		log.Fatalf("git --exec-path: %v", err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(strings.TrimSpace(string(out)), "git-http-backend"),
		Env:  []string{"GIT_PROJECT_ROOT=" + *root, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"},
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// The token is read at every request: the case can change it.
		token, err := os.ReadFile(*tokenFile)
		if u, p, ok := r.BasicAuth(); err != nil || !ok || u != "manent" || p != strings.TrimSpace(string(token)) {
			log.Printf("%s %s: 401", r.Method, r.URL.Path)
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		log.Printf("%s %s", r.Method, r.URL.Path)
		backend.ServeHTTP(w, r)
	})
	log.Printf("server Git su %s, repository in %s", *listen, *root)
	log.Fatal(http.ListenAndServe(*listen, nil))
}
