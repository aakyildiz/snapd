package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/snapcore/snapd/client"
)

// snapNames is the pool of snaps the workload manages on each target. A small
// pool makes operations on the same snap overlap often.
var snapNames = []string{"tst-a", "tst-b", "tst-c"}

// allSnapNames: the sideloaded snaps and the store snaps (store.go).
var allSnapNames = append(append([]string(nil), snapNames...), storeSnapNames...)

// target is one snapd instance and the driver's model of it.
type target struct {
	idx   int
	host  string
	agent string
	cli   *client.Client

	// gate: every request that may start a change holds it for reading;
	// the consistency check holds it for writing, so it sees snapd quiescent.
	gate sync.RWMutex

	snaps map[string]*snapModel

	lastCrash time.Time
	crashMu   sync.Mutex

	// system configuration: each accepted (or maybe-accepted) set bumps
	// sysGen; a set is judged only if no later one was sent.
	sysMu           sync.Mutex
	sysGen          int64
	sysOverlapGen   int64 // sets with gen <= this overlapped another
	sysPending      int
	sysTaintedUntil time.Time
}

// snapState is what snapd reports for one snap.
type snapState struct {
	Installed bool
	Current   string
	Active    bool
	Revs      []string // sorted
	Tracking  string   // tracked channel; empty for sideloaded snaps
	Held      bool     // general refreshes held by the user
}

func (s snapState) String() string {
	if !s.Installed {
		return "absent"
	}
	if s.Tracking != "" {
		return fmt.Sprintf("cur=%s active=%v revs=%v tracking=%s held=%v", s.Current, s.Active, s.Revs, s.Tracking, s.Held)
	}
	return fmt.Sprintf("cur=%s active=%v revs=%v", s.Current, s.Active, s.Revs)
}

func (s snapState) equal(o snapState) bool {
	if s.Installed != o.Installed {
		return false
	}
	if !s.Installed {
		return true
	}
	return s.Current == o.Current && s.Active == o.Active && s.Tracking == o.Tracking && s.Held == o.Held &&
		strings.Join(s.Revs, ",") == strings.Join(o.Revs, ",")
}

func (s snapState) hasRev(r string) bool {
	for _, x := range s.Revs {
		if x == r {
			return true
		}
	}
	return false
}

// snapModel is the driver's knowledge of one snap.
type snapModel struct {
	mu sync.Mutex
	// known: state holds what snapd reported at the last quiescent moment.
	known bool
	state snapState
	// pending counts changes in flight (or possibly in flight) on this snap.
	pending int
	// tainted: since state was observed, something happened whose effect the
	// driver can't attribute (overlapping changes, a lost response).
	tainted bool
}

func newTarget(idx int, host string) *target {
	t := &target{
		idx:   idx,
		host:  host,
		agent: fmt.Sprintf("http://%s:7001", host),
		snaps: map[string]*snapModel{},
	}
	t.cli = client.New(&client.Config{BaseURL: fmt.Sprintf("http://%s:7000", host), DisableAuth: true})
	hc := &http.Client{Timeout: 20 * time.Second}
	t.cli.Hijack(hc.Do)
	for _, n := range allSnapNames {
		t.snaps[n] = &snapModel{}
	}
	return t
}

func (t *target) logf(format string, args ...any) {
	log.Printf("[%s] "+format, append([]any{t.host}, args...)...)
}

// waitUp waits until snapd answers a request.
func (t *target) waitUp() {
	var last string
	for {
		_, err := t.cli.SysInfo()
		if err == nil {
			t.logf("snapd is up")
			return
		}
		if msg := err.Error(); msg != last {
			t.logf("waiting for snapd: %v", err)
			last = msg
		}
		time.Sleep(time.Second)
	}
}

// adminSetup installs the minimal "snapd" snap that classic systems require
// before any other snap can be installed (otherwise snapd fetches it from the
// store, which doesn't exist here), and records the starting state.
func (t *target) adminSetup() {
	var last string
	report := func(msg string) {
		if msg != last {
			t.logf("setup: %s", msg)
			last = msg
		}
	}
	for {
		s, _, err := t.cli.Snap("snapd")
		if err == nil && s.Status == client.StatusActive {
			break
		}
		path, err := buildSnapdSnap()
		if err != nil {
			report(fmt.Sprintf("build snapd snap: %v", err))
			time.Sleep(time.Second)
			continue
		}
		id, err := t.cli.InstallPath(path, "", &client.SnapOptions{Dangerous: true})
		if err != nil {
			report(fmt.Sprintf("install snapd snap: %v", err))
			time.Sleep(time.Second)
			continue
		}
		chg, err := t.waitChange(id, 5*time.Minute)
		if err != nil {
			report(fmt.Sprintf("wait for change %s: %v", id, err))
		} else if chg.Status != "Done" {
			report(fmt.Sprintf("snapd snap change %s ended %s: %s", id, chg.Status, chg.Err))
		}
	}
	// The pool snaps may linger from an earlier attempt; record them as-is.
	for name, m := range t.snaps {
		for {
			st, err := t.readState(name)
			if err == nil {
				m.state, m.known = st, true
				break
			}
			report(fmt.Sprintf("read %s: %v", name, err))
			time.Sleep(time.Second)
		}
	}
	// Auto-refresh would change store snaps behind the driver's back.
	for {
		id, err := t.cli.SetConf("system", map[string]any{"refresh.hold": "forever"})
		if err == nil {
			if _, err = t.waitChange(id, 2*time.Minute); err == nil {
				break
			}
		}
		report(fmt.Sprintf("hold auto-refresh: %v", err))
		time.Sleep(time.Second)
	}
	t.logf("setup done")
}

func (t *target) waitChange(id string, d time.Duration) (*client.Change, error) {
	deadline := time.Now().Add(d)
	for {
		chg, err := t.cli.Change(id)
		if err == nil && chg.Ready {
			return chg, nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("change %s still %s", id, chg.Status)
			}
			return chg, err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// readState asks snapd for one snap's revisions, current revision and status.
func (t *target) readState(name string) (snapState, error) {
	var st snapState
	s, _, err := t.cli.Snap(name)
	if err != nil {
		var cerr *client.Error
		if errors.As(err, &cerr) && cerr.Kind == client.ErrorKindSnapNotFound {
			return st, nil
		}
		return st, err
	}
	st.Installed = true
	st.Current = s.Revision.String()
	st.Tracking = s.TrackingChannel
	st.Held = s.Hold != nil && s.Hold.After(time.Now())
	st.Active = s.Status == client.StatusActive
	all, err := t.cli.List([]string{name}, &client.ListOptions{All: true})
	if err != nil {
		return st, err
	}
	for _, r := range all {
		st.Revs = append(st.Revs, r.Revision.String())
	}
	sort.Strings(st.Revs)
	return st, nil
}

// inProgress lists the in-progress changes that touch name ("" for all).
// Device registration ("become-operational") retries against the store
// forever, which doesn't exist here, so it never counts.
func (t *target) inProgress(name string) ([]*client.Change, error) {
	all, err := t.cli.Changes(&client.ChangesOptions{SnapName: name, Selector: client.ChangesInProgress})
	if err != nil {
		return nil, err
	}
	var out []*client.Change
	for _, c := range all {
		if c.Kind != "become-operational" {
			out = append(out, c)
		}
	}
	return out, nil
}

// buildSnapdSnap packs a "snapd" snap that holds no binaries or units, so
// installing it satisfies the classic prerequisite while the system keeps
// running the instrumented snapd from the deb location.
func buildSnapdSnap() (string, error) {
	dir, err := os.MkdirTemp("", "snapd-snap")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		return "", err
	}
	files := map[string]string{
		"meta/snap.yaml":     "name: snapd\nversion: \"2.99+antithesis\"\ntype: snapd\nsummary: s\ndescription: s\n",
		"usr/lib/snapd/info": "VERSION=2.99+antithesis\n",
	}
	for p, c := range files {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0644); err != nil {
			return "", err
		}
	}
	out := "/tmp/snapd.snap"
	return out, mksquashfs(dir, out)
}
