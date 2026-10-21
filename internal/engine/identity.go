package engine

import (
	"fmt"
	"slices"
	"strings"
)

// artifactSep separates application and artifact in the queue partition key.
const artifactSep = "+"

// releaseKey identifies the release in service of an inventory entry: every artifact of an
// application is a release of its own.
func releaseKey(application, artifact, environment string) string {
	return application + "/" + artifact + "/" + environment
}

// sameRelease reports whether a deploy belongs to the release of application, artifact and
// environment. An empty artifact matches every artifact (already resolved by resolveArtifact,
// or not chosen).
func (e *Engine) sameRelease(s Status, application, artifact, environment string) bool {
	return s.Application == application && s.Environment == environment && (artifact == "" || s.Artifact == artifact)
}

// resolveArtifact chooses the artifact of a promotion or rollback: the given one or, if not
// given, the only one released in the environment; with more than one it is an error that
// lists them.
func (e *Engine) resolveArtifact(application, environment, given string) (string, error) {
	if given != "" {
		return given, nil
	}
	entries, err := e.inventory(InventoryFilter{Application: application, Environment: environment})
	if err != nil {
		return "", err
	}
	var names []string
	for _, ie := range entries {
		if !slices.Contains(names, ie.Artifact) {
			names = append(names, ie.Artifact)
		}
	}
	switch len(names) {
	case 0:
		return "", nil
	case 1:
		return names[0], nil
	}
	slices.Sort(names)
	return "", fmt.Errorf("%w: %s has %d artifacts in %s (%s): choose one with artifact", ErrInvalidRequest,
		application, len(names), environment, strings.Join(names, ", "))
}
