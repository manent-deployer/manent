// Package policy evaluates the governance policy written in Rego with OPA embedded in the
// process, and keeps the policy in force up to date from a source (manent's store, a local
// directory, an OPA bundle, Consul) without restarting: Runtime implements engine.Governance
// and engine.PolicyAdmin.
//
// A revision is an immutable set of files: Rego modules (*.rego, tests in *_test.rego) and
// data (data.json, data.yaml in any directory, placed in the data tree at that path). It is
// identified by the sha256 of its content.
package policy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// MaxRevisionSize limits the total size of a revision's files.
const MaxRevisionSize = 4 << 20

// Files maps the relative path of every file of a revision to its content.
type Files map[string]string

// Hash is the sha256 of the files, in path order: the identity of a revision.
func (f Files) Hash() string {
	h := sha256.New()
	for _, p := range slices.Sorted(maps.Keys(f)) {
		fmt.Fprintf(h, "%d:%s\n%d:", len(p), p, len(f[p]))
		h.Write([]byte(f[p]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// isTest reports a test module.
func isTest(p string) bool { return strings.HasSuffix(p, "_test.rego") }

// isData reports a data file.
func isData(p string) bool {
	switch path.Base(p) {
	case "data.json", "data.yaml", "data.yml":
		return true
	}
	return false
}

// Check verifies paths, kinds and size of the files.
func (f Files) Check() error {
	var errs []error
	size := 0
	for _, p := range slices.Sorted(maps.Keys(f)) {
		size += len(f[p])
		clean := path.Clean(p)
		switch {
		case p == "" || clean != p || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "../") || p == "..":
			errs = append(errs, fmt.Errorf("invalid file path %q: it must be relative and clean", p))
		case !strings.HasSuffix(p, ".rego") && !isData(p):
			errs = append(errs, fmt.Errorf("file %q not allowed: only *.rego modules and data.json/data.yaml files", p))
		}
	}
	if size > MaxRevisionSize {
		errs = append(errs, fmt.Errorf("revision too large: %d bytes (at most %d)", size, MaxRevisionSize))
	}
	return errors.Join(errs...)
}

// Data merges the data files into one document: a file in directory a/b goes under a.b.
// Two files defining the same key are an error.
func (f Files) Data() (map[string]any, error) {
	root := map[string]any{}
	for _, p := range slices.Sorted(maps.Keys(f)) {
		if !isData(p) {
			continue
		}
		var v any
		var err error
		if strings.HasSuffix(p, ".json") {
			dec := json.NewDecoder(bytes.NewReader([]byte(f[p])))
			dec.UseNumber()
			err = dec.Decode(&v)
		} else {
			err = yaml.Unmarshal([]byte(f[p]), &v)
			v = normalizeYAML(v)
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		obj, ok := v.(map[string]any)
		if !ok {
			if v == nil {
				continue
			}
			return nil, fmt.Errorf("%s: the document must be an object", p)
		}
		dst := root
		if dir := path.Dir(p); dir != "." {
			for _, seg := range strings.Split(dir, "/") {
				next, ok := dst[seg].(map[string]any)
				if !ok {
					if _, exists := dst[seg]; exists {
						return nil, fmt.Errorf("%s: %s is already defined", p, dir)
					}
					next = map[string]any{}
					dst[seg] = next
				}
				dst = next
			}
		}
		if err := mergeInto(dst, obj, p); err != nil {
			return nil, err
		}
	}
	return root, nil
}

// mergeInto adds src to dst; objects merge, any other value defined twice is an error.
func mergeInto(dst, src map[string]any, file string) error {
	for k, v := range src {
		cur, exists := dst[k]
		if !exists {
			dst[k] = v
			continue
		}
		a, ok1 := cur.(map[string]any)
		b, ok2 := v.(map[string]any)
		if !ok1 || !ok2 {
			return fmt.Errorf("%s: key %q already defined by another data file", file, k)
		}
		if err := mergeInto(a, b, file); err != nil {
			return err
		}
	}
	return nil
}

// normalizeYAML turns the maps decoded by yaml.v3 into map[string]any, as JSON would.
func normalizeYAML(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = normalizeYAML(e)
		}
		return x
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[fmt.Sprint(k)] = normalizeYAML(e)
		}
		return m
	case []any:
		for i, e := range x {
			x[i] = normalizeYAML(e)
		}
		return x
	case time.Time:
		return x.Format(time.RFC3339Nano)
	}
	return v
}
