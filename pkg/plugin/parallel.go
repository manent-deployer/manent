package plugin

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ParallelDeployer is an optional interface of the Deployer: a plugin that returns true
// honors Request.Parallelism, usually by running its per-target work with ForEachTarget.
// manent passes a parallelism greater than 1 only to these plugins.
type ParallelDeployer interface {
	ParallelTargets() bool
}

// SupportsParallel reports whether the plugin declared it honors Request.Parallelism.
func SupportsParallel(d Deployer) bool {
	p, ok := d.(ParallelDeployer)
	return ok && p.ParallelTargets()
}

// ForEachTarget runs fn on every target of the request with at most req.Parallelism calls in
// flight (0 or 1: one after the other, in order). After the first error no new target starts
// and the running ones finish: stopping a copy or an apply halfway would leave a state harder
// to recover. The results keep the order of req.Targets, with the duration of every call;
// the targets never started are reported as StatusSkipped. fn wraps its errors with the
// target name; a failed result without a status becomes StatusFailed.
//
// The errors of the targets are joined: if one of them is permanent (Permanent), so is the
// result. A cancelled context stops the targets not yet started.
func ForEachTarget(ctx context.Context, req *Request, fn func(ctx context.Context, t Target) (TargetResult, error)) ([]TargetResult, error) {
	return forTargets(ctx, req, true, fn)
}

// ForAllTargets is ForEachTarget that does not stop at the first error: every target runs.
// It suits the operations that must reach as many targets as possible, such as a rollback,
// or that report on all of them, such as a dry-run.
func ForAllTargets(ctx context.Context, req *Request, fn func(ctx context.Context, t Target) (TargetResult, error)) ([]TargetResult, error) {
	return forTargets(ctx, req, false, fn)
}

func forTargets(ctx context.Context, req *Request, stopOnError bool, fn func(ctx context.Context, t Target) (TargetResult, error)) ([]TargetResult, error) {
	n := len(req.Targets)
	results := make([]TargetResult, n)
	errs := make([]error, n)
	sem := make(chan struct{}, max(1, min(req.Parallelism, n)))
	var (
		failed atomic.Bool
		wg     sync.WaitGroup
	)
	for i, t := range req.Targets {
		results[i] = TargetResult{Host: t.Host, Status: StatusSkipped}
	}
	for i, t := range req.Targets {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil || (stopOnError && failed.Load()) {
			// The slot, if taken, is not needed: nothing else starts.
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			r, err := fn(ctx, t)
			r.DurationMS = time.Since(start).Milliseconds()
			if r.Host == "" {
				r.Host = t.Host
			}
			if err != nil {
				if r.Status == "" || r.Status == StatusSkipped {
					r.Status = StatusFailed
				}
				errs[i] = err
				failed.Store(true)
			}
			results[i] = r
		}()
	}
	wg.Wait()
	err := errors.Join(errs...)
	if err == nil {
		for i := range results {
			if results[i].Status == StatusSkipped {
				// Not started only because the context was cancelled.
				err = ctx.Err()
				break
			}
		}
	}
	return results, single(errs, err)
}

// single returns the only error of errs as is, so a sequential run keeps the plain error of
// the failed target; otherwise joined.
func single(errs []error, joined error) error {
	var only error
	for _, e := range errs {
		if e != nil {
			if only != nil {
				return joined
			}
			only = e
		}
	}
	if only != nil {
		return only
	}
	return joined
}
