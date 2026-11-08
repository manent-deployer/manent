package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/manent-deployer/manent/internal/testdb"
)

// A worker with a different app.name on the same database cannot take the queues of the
// application that owns them: the error says that app.name must match.
func TestCodaDiAltraApplicazione(t *testing.T) {
	url := testdb.URL(t)
	reg, _ := testRegistry(t, artifactStep{})
	startWorker(t, workerOpts(url, "w1", reg))
	other := workerOpts(url, "w2", reg)
	other.AppName = "manent-other"
	w, err := NewWorker(other)
	if err == nil {
		_ = w.Shutdown(5 * time.Second)
		t.Fatal("a worker of another application must not register the same queue")
	}
	for _, want := range []string{`registering queue "deployments"`, "must have the same app.name", `"manent-other"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in: %v", want, err)
		}
	}
}

// A queue of the queues section: the matching deploys run only on the worker that serves it,
// which with Exclusive leaves the others to the default queue; a plugin's queue wins.
func TestCodaPerArtefatto(t *testing.T) {
	url := testdb.URL(t)
	reg, planner := testRegistry(t, artifactStep{})
	opts := workerOpts(url, "w2", reg)
	opts.RouteQueues = []QueueOptions{{Name: "gpu", GlobalConcurrency: 3}}
	opts.ListenExplicitly, opts.Exclusive = true, true
	w2 := startWorker(t, opts)
	plain := workerOpts(url, "w1", reg)
	plain.ListenExplicitly = true
	startWorker(t, plain)
	e := NewService(ServiceOptions{Client: w2.Client(), QueueName: "deployments", Planner: planner, Logger: discard,
		QueueRoutes:  []QueueRoute{{Queue: "gpu", Artifacts: []string{"ml-*"}, Environments: []string{"prod"}, Concurrency: 3}},
		PluginQueues: map[string]string{"ansible": "ansible-pool"}, TypeQueues: map[string]string{"ansible": "ansible-pool"}})

	for _, app := range []string{"ml-trainer", "ml-scorer"} {
		id := deploy(t, e, alice, ownedDescriptor(app, "1.0.0"))
		if st, err := e.Status(alice, id); err != nil || st.Queue != "gpu" || st.Executor != "w2" {
			t.Errorf("%s runs on the routed queue, on the worker that serves it: %+v %v", app, st, err)
		}
	}
	other := ownedDescriptor("shop", "1.0.0")
	id := deploy(t, e, alice, other)
	if st, err := e.Status(alice, id); err != nil || st.Queue != "deployments" || st.Executor != "w1" {
		t.Errorf("an exclusive worker leaves the default queue to the others: %+v %v", st, err)
	}
	test := ownedDescriptor("ml-trainer", "1.0.1")
	test.Environment = "test"
	id = deploy(t, e, alice, test)
	if st, err := e.Status(alice, id); err != nil || st.Queue != "deployments" {
		t.Errorf("the route matches the environment too: %+v %v", st, err)
	}

	withPlugin := ownedDescriptor("ml-infra", "1.0.0")
	withPlugin.Deploy.Plugin = "ansible"
	sub, err := e.Submit(alice, withPlugin)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := e.Status(alice, sub.ID); err != nil || st.Queue != "ansible-pool" {
		t.Errorf("the plugin's queue wins over the route: %+v %v", st, err)
	}
	if err := e.Cancel(alice, sub.ID); err != nil {
		t.Fatal(err)
	}

	qs, err := e.Queues(alice)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range qs {
		if q.Name == "gpu" {
			found = true
			if q.Kind != "routed" || q.GlobalConcurrency != 3 {
				t.Errorf("routed queue: %+v", q)
			}
		}
	}
	if !found {
		t.Errorf("a routed queue is listed also when empty: %+v", qs)
	}
}
