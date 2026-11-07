package customstep

import (
	"errors"

	"github.com/manent-deployer/manent/internal/cmdout"
	"github.com/manent-deployer/manent/internal/pipeline"
)

// Limit of command output in the deploy log, per run across all targets.
const maxLogBytes = 1 << 20

// err redacts the error message keeping its nature (permanent or temporary).
func redactErr(r *cmdout.Redactor, e error) error {
	msg := r.Apply(e.Error())
	if msg == e.Error() {
		return e
	}
	out := errors.New(msg)
	if errors.Is(e, pipeline.ErrPermanent) {
		return pipeline.Permanent(out)
	}
	return out
}
