package descriptor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// KindRelease identifies the descriptor of a composite release.
const KindRelease = "Release"

// Policies of a release when a deploy fails.
const (
	// ReleaseOnFailureStop does not start the deploys not yet started; running ones finish.
	ReleaseOnFailureStop = "stop"
	// ReleaseOnFailureContinue goes on with the deploys that do not depend on the failed one.
	ReleaseOnFailureContinue = "continue"
	// ReleaseOnFailureRollback stops like stop, then brings the successful deploys back to the
	// previous version, in reverse dependency order.
	ReleaseOnFailureRollback = "rollback"
)

// maxReleaseDeploys limits the deploys of a release.
const maxReleaseDeploys = 50

// Release releases several interdependent deploys together: independent ones in parallel,
// the others after the deploys they depend on succeed.
type Release struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	// ID identifies the release and is the idempotency key: required.
	ID string `json:"id" yaml:"id"`
	// OnFailure is stop (default), continue or rollback.
	OnFailure string          `json:"onFailure,omitempty" yaml:"onFailure,omitempty"`
	Deploys   []ReleaseDeploy `json:"deploys" yaml:"deploys"`
}

// ReleaseDeploy is a deploy of the release.
type ReleaseDeploy struct {
	// Name identifies the deploy in the release and in the dependencies (after).
	Name string `json:"name" yaml:"name"`
	// File is the YAML descriptor, relative to the release file: CLI only, which reads it and
	// sends it as Descriptor.
	File       string      `json:"file,omitempty" yaml:"file,omitempty"`
	Descriptor *Descriptor `json:"descriptor,omitempty" yaml:"descriptor,omitempty"`
	// After are the names of the release deploys that must succeed before this one.
	After []string `json:"after,omitempty" yaml:"after,omitempty"`
}

// ParseReleaseJSON reads a JSON release (API): the descriptors must be inlined.
func ParseReleaseJSON(data []byte) (*Release, error) {
	var r Release
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("invalid JSON release: %w", err)
	}
	return &r, r.Validate()
}

// LoadReleaseYAMLFile reads a YAML release and the descriptors it refers to (file), with
// paths relative to the release directory.
func LoadReleaseYAMLFile(path string) (*Release, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Release
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("invalid YAML release: %w", err)
	}
	for i := range r.Deploys {
		rd := &r.Deploys[i]
		if rd.File == "" {
			continue
		}
		if rd.Descriptor != nil {
			return nil, fmt.Errorf("deploys[%d]: file and descriptor are alternatives", i)
		}
		f := rd.File
		if !filepath.IsAbs(f) {
			f = filepath.Join(filepath.Dir(path), f)
		}
		d, err := LoadYAMLFile(f)
		if err != nil {
			return nil, fmt.Errorf("deploys[%d] (%s): %w", i, rd.File, err)
		}
		rd.Descriptor, rd.File = d, ""
	}
	return &r, r.Validate()
}

// Validate checks the release: distinct names and IDs, existing and acyclic dependencies,
// valid descriptors without schedule or dry-run.
func (r *Release) Validate() error {
	var errs []error
	if r.APIVersion != APIVersion {
		errs = append(errs, fmt.Errorf("apiVersion must be %q", APIVersion))
	}
	if r.Kind != KindRelease {
		errs = append(errs, fmt.Errorf("kind must be %q", KindRelease))
	}
	if !validName(r.ID) {
		errs = append(errs, fmt.Errorf("invalid id %q (required): %s", r.ID, nameRule))
	}
	if !slices.Contains([]string{"", ReleaseOnFailureStop, ReleaseOnFailureContinue, ReleaseOnFailureRollback}, r.OnFailure) {
		errs = append(errs, fmt.Errorf("invalid onFailure %q: stop, continue or rollback", r.OnFailure))
	}
	if len(r.Deploys) == 0 || len(r.Deploys) > maxReleaseDeploys {
		errs = append(errs, fmt.Errorf("deploys: from 1 to %d", maxReleaseDeploys))
	}
	names, ids := map[string]bool{}, map[string]bool{}
	for i, rd := range r.Deploys {
		if !validName(rd.Name) || names[rd.Name] {
			errs = append(errs, fmt.Errorf("deploys[%d]: invalid or duplicate name %q", i, rd.Name))
		}
		names[rd.Name] = true
		if rd.File != "" {
			errs = append(errs, fmt.Errorf("deploys[%d]: file not resolved, provide descriptor", i))
			continue
		}
		d := rd.Descriptor
		if d == nil {
			errs = append(errs, fmt.Errorf("deploys[%d]: descriptor is required", i))
			continue
		}
		if err := d.Validate(); err != nil {
			errs = append(errs, fmt.Errorf("deploys[%d] (%s): %w", i, rd.Name, err))
		}
		if d.Schedule != nil || d.DryRun {
			errs = append(errs, fmt.Errorf("deploys[%d] (%s): schedule and dryRun not allowed in a release", i, rd.Name))
		}
		if d.Tenants != nil {
			errs = append(errs, fmt.Errorf("deploys[%d] (%s): tenants not allowed in a release: give one tenant per deploy", i, rd.Name))
		}
		id := r.DeploymentID(rd)
		if !validName(id) || ids[id] {
			errs = append(errs, fmt.Errorf("deploys[%d]: invalid or duplicate deploy id %q", i, id))
		}
		ids[id] = true
	}
	for i, rd := range r.Deploys {
		for _, a := range rd.After {
			if !names[a] || a == rd.Name {
				errs = append(errs, fmt.Errorf("deploys[%d] (%s): after %q is not another deploy of the release", i, rd.Name, a))
			}
		}
	}
	if len(errs) == 0 {
		if _, err := r.Order(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// DeploymentID is the ID of the deploy in the release: the descriptor's or "<release>.<name>".
func (r *Release) DeploymentID(rd ReleaseDeploy) string {
	if rd.Descriptor != nil && rd.Descriptor.ID != "" {
		return rd.Descriptor.ID
	}
	return r.ID + "." + rd.Name
}

// FailurePolicy returns onFailure with its default.
func (r *Release) FailurePolicy() string {
	if r.OnFailure == "" {
		return ReleaseOnFailureStop
	}
	return r.OnFailure
}

// Order returns the deploys in topological order: each after those it depends on, ties in
// file order. Error if the dependencies form a cycle.
func (r *Release) Order() ([]ReleaseDeploy, error) {
	done := map[string]bool{}
	out := make([]ReleaseDeploy, 0, len(r.Deploys))
	for len(out) < len(r.Deploys) {
		progress := false
		for _, rd := range r.Deploys {
			if done[rd.Name] {
				continue
			}
			ready := true
			for _, a := range rd.After {
				ready = ready && done[a]
			}
			if ready {
				done[rd.Name], progress = true, true
				out = append(out, rd)
			}
		}
		if !progress {
			var cycle []string
			for _, rd := range r.Deploys {
				if !done[rd.Name] {
					cycle = append(cycle, rd.Name)
				}
			}
			return nil, fmt.Errorf("deploys: circular dependencies among %v", cycle)
		}
	}
	return out, nil
}
