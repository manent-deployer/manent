package config

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// RedactedValue replaces a value that may be a secret in Config.Redacted.
const RedactedValue = "[redacted]"

// secretKey matches the keys whose value may be a secret. Keys ending with _ref, _refs, _file,
// _dir, _path or _id name a reference or a location, not the value, and are kept.
var (
	secretKey  = regexp.MustCompile(`(?i)(password|passwd|secret|token|api_?key|private_?key|credentials?|sha256|signature)$`)
	keptSuffix = regexp.MustCompile(`(?i)_(ref|refs|file|files|dir|path|id|header|claim)$`)
)

// Redacted returns the configuration as a generic map (the YAML keys), ready to show: values
// that may be secrets are replaced by RedactedValue, passwords in URLs are masked. The second
// result lists the paths (dotted, with [i] for list items) of the replaced values.
//
// The configuration should hold only references to secrets: the redaction is a safety net for
// values written in the file anyway (database_url with a password, the hashes of the static
// tokens, the env of the external plugins, free-form plugin settings).
func (c *Config) Redacted() (map[string]any, []string, error) {
	data, err := yaml.Marshal(c)
	if err != nil {
		return nil, nil, fmt.Errorf("encoding the configuration: %w", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		return nil, nil, fmt.Errorf("decoding the configuration: %w", err)
	}
	r := redactor{}
	out, _ := r.walk("", "", m).(map[string]any)
	slices.Sort(r.paths)
	return out, r.paths, nil
}

type redactor struct{ paths []string }

func (r *redactor) walk(path, key string, v any) any {
	switch x := v.(type) {
	case map[string]any:
		// The values of an env map (plugins.<name>.external.env) are free-form: hide them all.
		hideAll := key == "env"
		for k, val := range x {
			p := join(path, k)
			if hideAll {
				if val != nil && val != "" {
					x[k] = RedactedValue
					r.paths = append(r.paths, p)
				}
				continue
			}
			x[k] = r.walk(p, k, val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = r.walk(path+"["+strconv.Itoa(i)+"]", key, val)
		}
		return x
	case string:
		if x == "" {
			return x
		}
		if secretKey.MatchString(key) && !keptSuffix.MatchString(key) {
			r.paths = append(r.paths, path)
			return RedactedValue
		}
		if masked, ok := maskURL(x); ok {
			r.paths = append(r.paths, path)
			return masked
		}
		return x
	default:
		return v
	}
}

// maskURL masks the password of a URL with credentials (postgres://user:pass@host/db).
func maskURL(s string) (string, bool) {
	if !strings.Contains(s, "://") || !strings.Contains(s, "@") {
		return "", false
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return "", false
	}
	if _, has := u.User.Password(); !has {
		return "", false
	}
	return u.Redacted(), true
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
