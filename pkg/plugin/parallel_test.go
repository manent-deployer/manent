package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func targets(n int) []Target {
	ts := make([]Target, n)
	for i := range ts {
		ts[i] = Target{Host: fmt.Sprintf("h%d", i)}
	}
	return ts
}

func TestForEachTargetLimit(t *testing.T) {
	for _, p := range []int{0, 1, 3, 10} {
		var cur, peak atomic.Int32
		req := &Request{Targets: targets(8), Parallelism: p}
		res, err := ForEachTarget(context.Background(), req, func(ctx context.Context, tg Target) (TargetResult, error) {
			c := cur.Add(1)
			for {
				old := peak.Load()
				if c <= old || peak.CompareAndSwap(old, c) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			cur.Add(-1)
			return TargetResult{Status: StatusDeployed}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		want := int32(min(max(p, 1), 8))
		if peak.Load() != want {
			t.Errorf("parallelism %d: peak %d, want %d", p, peak.Load(), want)
		}
		for i, r := range res {
			if r.Host != req.Targets[i].Host || r.Status != StatusDeployed || r.DurationMS < 10 {
				t.Errorf("parallelism %d: result %d = %+v", p, i, r)
			}
		}
	}
}

func TestForEachTargetStopsAfterError(t *testing.T) {
	boom := errors.New("boom")
	// Sequential: the targets after the failed one never start, the error is the plain one.
	var calls atomic.Int32
	res, err := ForEachTarget(context.Background(), &Request{Targets: targets(4)}, func(ctx context.Context, tg Target) (TargetResult, error) {
		calls.Add(1)
		if tg.Host == "h1" {
			return TargetResult{}, fmt.Errorf("target %s: %w", tg.Host, boom)
		}
		return TargetResult{Status: StatusDeployed}, nil
	})
	if !errors.Is(err, boom) || err.Error() != "target h1: boom" {
		t.Fatalf("err = %v", err)
	}
	want := []string{StatusDeployed, StatusFailed, StatusSkipped, StatusSkipped}
	for i, r := range res {
		if r.Status != want[i] {
			t.Errorf("target %d: %s, want %s", i, r.Status, want[i])
		}
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d", calls.Load())
	}

	// In parallel the running targets finish and their errors are joined; a permanent one
	// makes the result permanent.
	release := make(chan struct{})
	res, err = ForEachTarget(context.Background(), &Request{Targets: targets(5), Parallelism: 2}, func(ctx context.Context, tg Target) (TargetResult, error) {
		switch tg.Host {
		case "h0":
			<-release
			return TargetResult{}, Permanent(errors.New("h0 refused"))
		case "h1":
			close(release)
			time.Sleep(5 * time.Millisecond)
			return TargetResult{}, errors.New("h1 down")
		}
		return TargetResult{Status: StatusDeployed}, nil
	})
	if !errors.Is(err, ErrPermanent) || res[0].Status != StatusFailed || res[1].Status != StatusFailed {
		t.Fatalf("err = %v, results %+v", err, res)
	}
	for _, r := range res[2:] {
		if r.Status != StatusSkipped {
			t.Errorf("%s: %s, want skipped", r.Host, r.Status)
		}
	}
}

func TestForAllTargets(t *testing.T) {
	res, err := ForAllTargets(context.Background(), &Request{Targets: targets(3), Parallelism: 2}, func(ctx context.Context, tg Target) (TargetResult, error) {
		if tg.Host == "h0" {
			return TargetResult{}, errors.New("h0 down")
		}
		return TargetResult{Status: StatusRolledBack}, nil
	})
	if err == nil || res[0].Status != StatusFailed || res[1].Status != StatusRolledBack || res[2].Status != StatusRolledBack {
		t.Fatalf("err = %v, results %+v", err, res)
	}
}

func TestForEachTargetCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	res, err := ForEachTarget(ctx, &Request{Targets: targets(3)}, func(ctx context.Context, tg Target) (TargetResult, error) {
		cancel()
		return TargetResult{Status: StatusDeployed}, nil
	})
	if !errors.Is(err, context.Canceled) || res[0].Status != StatusDeployed || res[1].Status != StatusSkipped {
		t.Fatalf("err = %v, results %+v", err, res)
	}
}

type parallelStub struct{ Deployer }

func (parallelStub) ParallelTargets() bool { return true }

func TestSupportsParallel(t *testing.T) {
	if SupportsParallel(nil) || !SupportsParallel(parallelStub{}) {
		t.Fatal("SupportsParallel")
	}
}
