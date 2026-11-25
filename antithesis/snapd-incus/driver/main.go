// Command driver is the Antithesis workload for snapd.
//
// Each target is one snapd running in an Incus system container (see
// ../host). The driver talks to snapd's REST API with snapd's own client
// package, sideloading generated snaps and driving them through install,
// remove, revert, enable/disable, configuration and change aborts, while
// snapd is crashed and its container restarted underneath it. It records
// what each snap looked like before each change, and judges the outcome:
// a change that fails leaves its snaps untouched, one that succeeds has its
// effect, and when nothing is in flight the filesystem, mounts and mount
// units agree with snapd's state.
package main

import (
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"
	"github.com/antithesishq/antithesis-sdk-go/lifecycle"
	"hegel.dev/go/hegel"

	"github.com/snapcore/snapd/client"
)

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	// The snap CLI waits up to 120s per request; a rule must not.
	client.MockDoTimings(250*time.Millisecond, 15*time.Second)

	hosts := strings.Split(envOr("HOSTS", "snapd1"), ",")
	var targets []*target
	for i, h := range hosts {
		targets = append(targets, newTarget(i, h))
	}

	for _, t := range targets {
		t.waitUp()
	}
	store := newStoreModel(envOr("STORE_DIR", "/srv/store"))
	for {
		err := store.setup()
		if err == nil {
			break
		}
		log.Printf("setup: store: %v", err)
		time.Sleep(time.Second)
	}
	for _, t := range targets {
		t.adminSetup()
	}
	// Reproductions of the findings, on their own host (see repro.go).
	var rp *repro
	if h := os.Getenv("REPRO_HOST"); h != "" {
		rp = newRepro(h, newStoreModel(envOr("STORE_DIR", "/srv/store")))
		rp.setup()
	}
	lifecycle.SetupComplete(map[string]any{"hosts": hosts, "repro": os.Getenv("REPRO_HOST")})
	log.Printf("setup complete")

	waitFile("/workload-active")
	assert.Reachable("the workload starts", nil)

	w := newWorkload(targets)
	w.store = store
	if s := os.Getenv("EVENTUALLY_AFTER_SECONDS"); s != "" {
		n, _ := strconv.Atoi(s)
		go func() {
			time.Sleep(time.Duration(n) * time.Second)
			w.eventually()
		}()
	}
	go w.reportLoop()
	if rp != nil {
		go rp.run()
	}

	hegel.Workload(func(tc hegel.TestCase) {
		hegel.RunStateful(tc, w,
			hegel.WithConcurrency(),
			hegel.WithStatefulStepCount(math.MaxInt),
		)
	}, hegel.WithPhases(hegel.PhaseGenerate), hegel.SuppressHealthCheck(hegel.TooSlow))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func waitFile(path string) {
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// agentGet calls the host agent, which runs commands in the container.
func agentGet(base, path string, timeout time.Duration) (string, error) {
	c := http.Client{Timeout: timeout}
	rsp, err := c.Get(base + path)
	if err != nil {
		return "", err
	}
	defer rsp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 64<<10)
	for {
		n, err := rsp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	if rsp.StatusCode != 200 {
		return b.String(), fmt.Errorf("agent %s: status %d: %s", path, rsp.StatusCode, strings.TrimSpace(b.String()))
	}
	return b.String(), nil
}
