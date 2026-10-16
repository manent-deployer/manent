package kubernetes

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Object is an untyped Kubernetes object.
type Object map[string]any

func (o Object) str(keys ...string) string {
	var cur any = map[string]any(o)
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[k]
	}
	s, _ := cur.(string)
	return s
}

// APIVersion, Kind, Name and Namespace read the identity fields of the object.
func (o Object) APIVersion() string { return o.str("apiVersion") }
func (o Object) Kind() string       { return o.str("kind") }
func (o Object) Name() string       { return o.str("metadata", "name") }
func (o Object) Namespace() string  { return o.str("metadata", "namespace") }

// Ref describes the object in messages: Kind/namespace/name.
func (o Object) Ref() string {
	if ns := o.Namespace(); ns != "" {
		return o.Kind() + "/" + ns + "/" + o.Name()
	}
	return o.Kind() + "/" + o.Name()
}

// metadata returns (creating it) the metadata map.
func (o Object) metadata() map[string]any {
	m, ok := o["metadata"].(map[string]any)
	if !ok {
		m = map[string]any{}
		o["metadata"] = m
	}
	return m
}

// setMap sets key/value in metadata.<field> (labels or annotations).
func (o Object) setMap(field, key, value string) {
	md := o.metadata()
	m, ok := md[field].(map[string]any)
	if !ok {
		m = map[string]any{}
		md[field] = m
	}
	m[key] = value
}

// ObjectRef identifies an applied object, for the cleanup of removed objects.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

func (r ObjectRef) String() string {
	return strings.Trim(r.Kind+"/"+r.Namespace+"/"+r.Name, "/")
}

func refOf(o Object) ObjectRef {
	return ObjectRef{APIVersion: o.APIVersion(), Kind: o.Kind(), Namespace: o.Namespace(), Name: o.Name()}
}

// loadManifests reads the artifact manifests: a YAML or JSON file (also multi-document) or a
// .tar.gz/.tgz archive of .yaml, .yml and .json files, read in name order.
func loadManifests(file string) ([]byte, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("lettura dei manifest: %w", err)
	}
	lower := strings.ToLower(file)
	switch {
	case strings.HasSuffix(lower, ".yaml"), strings.HasSuffix(lower, ".yml"), strings.HasSuffix(lower, ".json"):
		return data, nil
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return fromArchive(data)
	}
	return nil, fmt.Errorf("artefatto %s non supportato: atteso un file .yaml, .yml, .json o un archivio .tar.gz di manifest", path.Base(file))
}

func fromArchive(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("archivio dei manifest: %w", err)
	}
	tr := tar.NewReader(zr)
	files := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("archivio dei manifest: %w", err)
		}
		name := strings.ToLower(h.Name)
		if h.Typeflag != tar.TypeReg || strings.HasPrefix(path.Base(h.Name), ".") ||
			!(strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".json")) {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, 8<<20))
		if err != nil {
			return nil, err
		}
		files[h.Name] = b
	}
	if len(files) == 0 {
		return nil, errors.New("l'archivio non contiene manifest .yaml, .yml o .json")
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	var out bytes.Buffer
	for _, n := range names {
		out.WriteString("\n---\n")
		out.Write(files[n])
	}
	return out.Bytes(), nil
}

// render replaces the known ${NAME} variables (manent's and the var.NAME parameters); the
// others stay unchanged, because manifests can contain scripts with their own variables.
func render(src []byte, vars map[string]string) []byte {
	pairs := make([]string, 0, len(vars)*2)
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		pairs = append(pairs, "${"+k+"}", vars[k])
	}
	return []byte(strings.NewReplacer(pairs...).Replace(string(src)))
}

// parseObjects splits the YAML documents into objects; empty documents are ignored and lists
// (kind: List) expanded.
func parseObjects(data []byte) ([]Object, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var out []Object
	for i := 1; ; i++ {
		var doc map[string]any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("documento %d dei manifest: %w", i, err)
		}
		if len(doc) == 0 {
			continue
		}
		o := Object(doc)
		if o.Kind() == "List" || strings.HasSuffix(o.Kind(), "List") && o["items"] != nil {
			items, _ := o["items"].([]any)
			for _, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("documento %d: elemento della lista non valido", i)
				}
				out = append(out, Object(m))
			}
			continue
		}
		out = append(out, o)
	}
	for _, o := range out {
		if o.APIVersion() == "" || o.Kind() == "" || o.Name() == "" {
			return nil, fmt.Errorf("oggetto senza apiVersion, kind o metadata.name: %s", o.Ref())
		}
	}
	if len(out) == 0 {
		return nil, errors.New("nessun oggetto nei manifest")
	}
	return out, nil
}

// kindOrder applies first the objects others depend on (namespaces, CRDs, permissions,
// configuration), then the workloads.
var kindOrder = []string{
	"Namespace", "CustomResourceDefinition", "PriorityClass", "StorageClass",
	"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding",
	"ConfigMap", "Secret", "PersistentVolume", "PersistentVolumeClaim",
	"Service", "Deployment", "StatefulSet", "DaemonSet", "Job", "CronJob",
}

func order(kind string) int {
	if i := slices.Index(kindOrder, kind); i >= 0 {
		return i
	}
	return len(kindOrder)
}

func sortObjects(objs []Object) {
	sort.SliceStable(objs, func(i, j int) bool { return order(objs[i].Kind()) < order(objs[j].Kind()) })
}

// podSpec returns the pod spec of a workload, or nil.
func podSpec(o Object) map[string]any {
	spec, _ := o["spec"].(map[string]any)
	switch o.Kind() {
	case "Pod":
		return spec
	case "CronJob":
		jt, _ := spec["jobTemplate"].(map[string]any)
		js, _ := jt["spec"].(map[string]any)
		spec = js
	}
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	return ps
}

// images returns the container images of a workload.
func images(o Object) []string {
	ps := podSpec(o)
	var out []string
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		list, _ := ps[field].([]any)
		for _, c := range list {
			m, _ := c.(map[string]any)
			if img, _ := m["image"].(string); img != "" {
				out = append(out, img)
			}
		}
	}
	return out
}

// workload reports the objects whose rollout must be waited for.
func workload(o Object) bool {
	if !strings.HasPrefix(o.APIVersion(), "apps/") {
		return false
	}
	switch o.Kind() {
	case "Deployment", "StatefulSet", "DaemonSet":
		return true
	}
	return false
}
