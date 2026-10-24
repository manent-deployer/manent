package engine

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/descriptor"
	"github.com/manent-deployer/manent/internal/testdb"
)

// A plugin with its own queue: its deploys wait until a worker that serves the plugin starts,
// the others run on the default queue.
func TestCodaDelPlugin(t *testing.T) {
	url := testdb.URL(t)
	reg, planner := testRegistry(t, artifactStep{})
	// The plugin's worker registers its queue first: DBOS keeps it in the database, and the
	// plain worker must not dequeue it (explicit listening).
	opts := workerOpts(url, "w2", reg)
	opts.PluginQueues = []QueueOptions{{Name: "ansible-pool"}}
	opts.ListenExplicitly = true
	w2 := startWorker(t, opts)
	plain := workerOpts(url, "w1", reg)
	plain.ListenExplicitly = true
	startWorker(t, plain)
	e := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		PluginQueues: map[string]string{"ansible": "ansible-pool"}, TypeQueues: map[string]string{"ansible": "ansible-pool"}})

	app := deploy(t, e, alice, ownedDescriptor("shop", "1.0.0"))
	if st, err := e.Status(alice, app); err != nil || st.Queue != "deployments" {
		t.Fatalf("an application deploy runs on the default queue: %+v %v", st, err)
	}
	var ids []string
	for i := range 6 {
		d := ownedDescriptor("os-"+strconv.Itoa(i), "1.0.0")
		if i%2 == 0 {
			d.Artifact.Type = "ansible"
		} else {
			d.Deploy.Plugin = "ansible"
		}
		sub, err := e.Submit(alice, d)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sub.ID)
	}
	for _, id := range ids {
		if _, err := e.Wait(alice, id); err != nil {
			t.Fatal(err)
		}
		st, err := e.Status(alice, id)
		if err != nil || st.State != StateSuccess || st.Queue != "ansible-pool" || st.Executor != "w2" {
			t.Errorf("only the worker that serves the plugin runs its deploys: %+v %v", st, err)
		}
	}
	qs, err := e.Queues(alice)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range qs {
		if q.Name == "ansible-pool" && q.Kind != "plugin" {
			t.Errorf("kind of the plugin queue: %+v", q)
		}
	}
}

// In a release the deploys are enqueued by a workflow on a worker that does not serve the
// plugin: they must still go to the plugin's queue, also after a dependency wait.
func TestCodaDelPluginInUnaRelease(t *testing.T) {
	url := testdb.URL(t)
	reg, planner := testRegistry(t, artifactStep{})
	routing := ServiceOptions{QueueName: "deployments", Planner: planner, Logger: discard,
		PluginQueues: map[string]string{"ansible": "ansible-pool"}, TypeQueues: map[string]string{"ansible": "ansible-pool"}}
	o := workerOpts(url, "w1", reg)
	o.ListenExplicitly = true
	w1, err := NewWorker(o)
	if err != nil {
		t.Fatal(err)
	}
	o1 := routing
	o1.Client = w1.Client()
	e := NewService(o1)
	w1.SetService(e)
	if err := w1.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w1.Shutdown(5 * time.Second) })
	opts := workerOpts(url, "w2", reg)
	opts.PluginQueues = []QueueOptions{{Name: "ansible-pool"}}
	opts.ListenExplicitly = true
	w2, err := NewWorker(opts)
	if err != nil {
		t.Fatal(err)
	}
	o2 := routing
	o2.Client = w2.Client()
	w2.SetService(NewService(o2))
	if err := w2.Launch(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w2.Shutdown(5 * time.Second) })

	os := appVersion("os", "prod", "1.0.0", "")
	os.Artifact.Type = "ansible"
	r := &descriptor.Release{APIVersion: descriptor.APIVersion, Kind: descriptor.KindRelease, ID: "rel", Deploys: []descriptor.ReleaseDeploy{
		{Name: "infra", Descriptor: appVersion("infra", "prod", "1.0.0", "")},
		{Name: "os", Descriptor: os, After: []string{"infra"}},
	}}
	ctx, cancel := context.WithTimeout(alice, 30*time.Second)
	defer cancel()
	if _, err := e.SubmitRelease(ctx, r); err != nil {
		t.Fatal(err)
	}
	for {
		rs, err := e.ReleaseStatus(ctx, "rel")
		if err != nil {
			t.Fatal(err)
		}
		if Terminal(rs.State) || ctx.Err() != nil {
			if rs.State != StateSuccess {
				t.Fatalf("release: %+v", rs)
			}
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	st, err := e.Status(ctx, "rel.os")
	if err != nil || st.Queue != "ansible-pool" || st.Executor != "w2" {
		t.Errorf("the ansible deploy of the release runs on the plugin's queue: %+v %v", st, err)
	}
}
