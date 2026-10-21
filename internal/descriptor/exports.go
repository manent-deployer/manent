package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/joeg-ita/manent/internal/labels"
)

// Formats of an export.
const (
	// ExportFormatString is a single value checked against the pattern (default).
	ExportFormatString = "string"
	// ExportFormatTargets is a JSON list of targets ({host, port, user, labels} or plain host
	// strings) that the importing deploys can use as deploy.targetsFrom.
	ExportFormatTargets = "targets"
)

// Limits of exports and imports.
const (
	maxExports      = 32
	maxImports      = 10
	maxImportRefs   = 32
	maxExportTarget = 500
	// MaxExportBytes limits the value of an export.
	MaxExportBytes = 64 << 10
	// MaxExportsBytes limits all the exports of a deploy: they are recorded among the workflow
	// attributes, read by listings and inventory.
	MaxExportsBytes = 256 << 10
)

// Import declares a deploy whose exports this deploy reads, resolved when the deploy starts
// (config step). Exactly one of Deployment and Current.
type Import struct {
	// Deployment is the ID of a deploy, or the name of another deploy of the same release: it
	// must be among the dependencies (after), so it has succeeded when this deploy starts.
	Deployment string `json:"deployment,omitempty" yaml:"deployment,omitempty"`
	// Current is the release in service (inventory) of an application in an environment.
	Current *ImportCurrent `json:"current,omitempty" yaml:"current,omitempty"`
}

// ImportCurrent identifies the release in service to read the exports from. The tenant is the
// importing deploy's.
type ImportCurrent struct {
	Application string `json:"application" yaml:"application"`
	// Environment defaults to the importing deploy's: a promotion then reads the exports of
	// the target environment.
	Environment string `json:"environment,omitempty" yaml:"environment,omitempty"`
	// Artifact chooses the artifact of the application; required
	// when the environment has more than one.
	Artifact string `json:"artifact,omitempty" yaml:"artifact,omitempty"`
}

// ExportRef points to an export of an import.
type ExportRef struct {
	Import string `json:"import" yaml:"import"`
	Export string `json:"export" yaml:"export"`
}

// Export declares a value produced by the deploy that other deploys can import. Only declared
// outputs become exports, checked at the end of the deploy: an invalid value fails it.
type Export struct {
	// From is "<step>.<key>": an output of the plugin (deploy step, plugin.Result.Outputs) or of
	// a custom step.
	From string `json:"from" yaml:"from"`
	// Format is string (default) or targets.
	Format string `json:"format,omitempty" yaml:"format,omitempty"`
	// Pattern is the regular expression (anchored) the value must match: required for string,
	// applied to each host for targets. It keeps secrets and unexpected values out of exports.
	Pattern string `json:"pattern,omitempty" yaml:"pattern,omitempty"`
}

// exportNameRe limits the names of imports, exports and parameters taken from an export.
var exportNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// exportKeyRe limits the output keys an export reads (same rule as the plugin outputs).
var exportKeyRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// Source returns step and output key of the export.
func (e Export) Source() (step, key string) {
	step, key, _ = strings.Cut(e.From, ".")
	return step, key
}

// IsTargets reports whether the export is a list of targets.
func (e Export) IsTargets() bool { return e.Format == ExportFormatTargets }

// Validate checks source, format and pattern of the export.
func (e Export) Validate(name string) error {
	var errs []error
	if !exportNameRe.MatchString(name) {
		errs = append(errs, fmt.Errorf("exports: invalid name %q (letters, digits, '_', '-', max 64, initial letter)", name))
	}
	if step, key := e.Source(); step == "" || !exportKeyRe.MatchString(key) {
		errs = append(errs, fmt.Errorf("exports.%s.from: %q must be <step>.<key>", name, e.From))
	}
	switch e.Format {
	case "", ExportFormatString:
		if e.Pattern == "" {
			errs = append(errs, fmt.Errorf("exports.%s.pattern is required with format string", name))
		}
	case ExportFormatTargets:
	default:
		errs = append(errs, fmt.Errorf("exports.%s.format %q is invalid (string|targets)", name, e.Format))
	}
	if e.Pattern != "" {
		if _, err := regexp.Compile(e.Pattern); err != nil {
			errs = append(errs, fmt.Errorf("exports.%s.pattern: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// Check verifies a value of the export: size, format and pattern.
func (e Export) Check(value string) error {
	if len(value) > MaxExportBytes || strings.ContainsRune(value, 0) {
		return fmt.Errorf("value too long (max %d bytes) or with NUL characters", MaxExportBytes)
	}
	var re *regexp.Regexp
	if e.Pattern != "" {
		var err error
		if re, err = regexp.Compile(`^(?:` + e.Pattern + `)$`); err != nil {
			return err
		}
	}
	if !e.IsTargets() {
		if re != nil && !re.MatchString(value) {
			return fmt.Errorf("value does not match the pattern %q", e.Pattern)
		}
		return nil
	}
	ts, err := ParseTargets(value)
	if err != nil {
		return err
	}
	for i, t := range ts {
		if re != nil && !re.MatchString(t.Host) {
			return fmt.Errorf("target %d: host %q does not match the pattern %q", i, t.Host, e.Pattern)
		}
	}
	return nil
}

// ParseTargets decodes a targets export: a JSON list of objects {host, port, user, labels} or
// of host strings. The targets are validated like those of the descriptor.
func ParseTargets(value string) ([]Target, error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(value), &raw); err != nil {
		return nil, fmt.Errorf("targets: a JSON list is expected: %w", err)
	}
	if len(raw) > maxExportTarget {
		return nil, fmt.Errorf("targets: at most %d", maxExportTarget)
	}
	out := make([]Target, 0, len(raw))
	for i, r := range raw {
		var t Target
		if trimmed := bytes.TrimSpace(r); len(trimmed) > 0 && trimmed[0] == '"' {
			if err := json.Unmarshal(r, &t.Host); err != nil {
				return nil, fmt.Errorf("targets[%d]: %w", i, err)
			}
		} else {
			dec := json.NewDecoder(bytes.NewReader(r))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&t); err != nil {
				return nil, fmt.Errorf("targets[%d]: %w", i, err)
			}
		}
		if t.Host == "" || strings.ContainsAny(t.Host, " \t\r\n/") || t.Port < 0 || t.Port > 65535 {
			return nil, fmt.Errorf("targets[%d]: invalid host or port", i)
		}
		if err := labels.ValidLabels(t.Labels); err != nil {
			return nil, fmt.Errorf("targets[%d]: %w", i, err)
		}
		out = append(out, t)
	}
	return out, nil
}

// HasImports reports whether the deploy reads exports of other deploys.
func (d *Descriptor) HasImports() bool {
	return len(d.Imports) > 0 || len(d.Deploy.TargetsFrom) > 0 || len(d.Deploy.ParamsFrom) > 0
}

// DropDeploymentImports removes the imports of a given deploy and their references: a
// promotion or a manual rollback no longer has the dependencies (after) of the source deploy,
// and its targets come from its own environment.
func (d *Descriptor) DropDeploymentImports() {
	dropped := map[string]bool{}
	for name, imp := range d.Imports {
		if imp.Deployment != "" {
			dropped[name] = true
		}
	}
	if len(dropped) == 0 {
		return
	}
	d.Imports = maps.Clone(d.Imports)
	maps.DeleteFunc(d.Imports, func(name string, _ Import) bool { return dropped[name] })
	if len(d.Imports) == 0 {
		d.Imports = nil
	}
	d.Deploy.TargetsFrom = slices.DeleteFunc(slices.Clone(d.Deploy.TargetsFrom), func(r ExportRef) bool { return dropped[r.Import] })
	if len(d.Deploy.TargetsFrom) == 0 {
		d.Deploy.TargetsFrom = nil
	}
	d.Deploy.ParamsFrom = maps.Clone(d.Deploy.ParamsFrom)
	maps.DeleteFunc(d.Deploy.ParamsFrom, func(_ string, r ExportRef) bool { return dropped[r.Import] })
	if len(d.Deploy.ParamsFrom) == 0 {
		d.Deploy.ParamsFrom = nil
	}
}

// ValidateImports checks imports and references: declared imports, each used, deploys among
// the dependencies (after). field prefixes the messages ("" for the descriptor). params are the
// parameters already set, which a parameter taken from an export cannot repeat. self are
// application and environment of the deploy: a deploy cannot import its own release in service.
func ValidateImports(imports map[string]Import, targetsFrom []ExportRef, paramsFrom map[string]ExportRef,
	params map[string]string, after []string, application, artifact, environment string) error {
	var errs []error
	if len(imports) > maxImports {
		errs = append(errs, fmt.Errorf("imports: at most %d", maxImports))
	}
	for _, name := range slices.Sorted(maps.Keys(imports)) {
		imp := imports[name]
		if !exportNameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("imports: invalid name %q (letters, digits, '_', '-', max 64, initial letter)", name))
		}
		switch {
		case (imp.Deployment == "") == (imp.Current == nil):
			errs = append(errs, fmt.Errorf("imports.%s: provide deployment or current", name))
		case imp.Deployment != "":
			if !validName(imp.Deployment) {
				errs = append(errs, fmt.Errorf("imports.%s.deployment %q: %s", name, imp.Deployment, nameRule))
			} else if !slices.Contains(after, imp.Deployment) {
				errs = append(errs, fmt.Errorf("imports.%s: deploy %s must be among the dependencies (after)", name, imp.Deployment))
			}
		default:
			c := imp.Current
			env := c.Environment
			if env == "" {
				env = environment
			}
			if !validName(c.Application) || (c.Environment != "" && !validName(c.Environment)) || (c.Artifact != "" && !validName(c.Artifact)) {
				errs = append(errs, fmt.Errorf("imports.%s.current: invalid application, environment or artifact", name))
			} else if c.Application == application && env == environment && (c.Artifact == "" || c.Artifact == artifact) {
				errs = append(errs, fmt.Errorf("imports.%s.current: a deploy cannot import its own release in service", name))
			}
		}
	}
	used := map[string]bool{}
	check := func(field string, r ExportRef) {
		if _, ok := imports[r.Import]; !ok {
			errs = append(errs, fmt.Errorf("%s: import %q is not declared in imports", field, r.Import))
		}
		if !exportNameRe.MatchString(r.Export) {
			errs = append(errs, fmt.Errorf("%s: invalid export %q", field, r.Export))
		}
		used[r.Import] = true
	}
	if len(targetsFrom)+len(paramsFrom) > maxImportRefs {
		errs = append(errs, fmt.Errorf("targetsFrom and paramsFrom: at most %d references", maxImportRefs))
	}
	for i, r := range targetsFrom {
		check(fmt.Sprintf("targetsFrom[%d]", i), r)
	}
	for _, p := range slices.Sorted(maps.Keys(paramsFrom)) {
		if p == "" || strings.ContainsRune(p, 0) {
			errs = append(errs, errors.New("paramsFrom: empty parameter name"))
		}
		if _, ok := params[p]; ok {
			errs = append(errs, fmt.Errorf("paramsFrom.%s: the parameter is also set in params", p))
		}
		check("paramsFrom."+p, paramsFrom[p])
	}
	for _, name := range slices.Sorted(maps.Keys(imports)) {
		if !used[name] {
			errs = append(errs, fmt.Errorf("imports.%s is not used by targetsFrom or paramsFrom", name))
		}
	}
	return errors.Join(errs...)
}

// ValidateExports checks the declared exports.
func ValidateExports(exports map[string]Export) error {
	var errs []error
	if len(exports) > maxExports {
		errs = append(errs, fmt.Errorf("exports: at most %d", maxExports))
	}
	for _, name := range slices.Sorted(maps.Keys(exports)) {
		errs = append(errs, exports[name].Validate(name))
	}
	return errors.Join(errs...)
}
