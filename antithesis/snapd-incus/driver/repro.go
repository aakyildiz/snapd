package main

// Reproductions of the findings, one scenario each, on their own snapd host
// (REPRO_HOST, "snapd2") next to the main workload. Each scenario drives
// snapd into the finding's window on purpose, with fault points compiled
// into snapd (osutil.MaybeInjectFault, enabled per scenario through the
// agent's /fault endpoint), a systemctl shim that fails chosen calls, and a
// store cut-off; then it checks the finding's signature.
//
// For every finding N there are three properties:
//   - "repro N: the scenario reaches its check" (Reachable): the setup worked
//     and the fault fired where intended, so the check below means something;
//   - "repro N: <correct behaviour>" (Always): fails while the bug exists;
//   - "repro N: finding N reproduced" (Sometimes): passes while it exists.
//
// The host is excluded from Antithesis faults so the scenarios stay exact;
// the main workload on snapd1 keeps all of them.

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"

	"github.com/snapcore/snapd/client"
)

type repro struct {
	t       *target
	store   *storeModel
	round   int
	storeOK bool // the store snaps of finding 11 are published
}

// reproStoreSnaps are published by the reproduction, distinct from the main
// workload's store snaps.
var reproStoreSnaps = []string{"rs-a", "rs-b"}

func newRepro(host string, store *storeModel) *repro {
	return &repro{t: newTarget(99, host), store: store}
}

func (r *repro) logf(format string, args ...any) {
	log.Printf("[repro] "+format, args...)
}

// setup runs before setup complete.
func (r *repro) setup() {
	r.t.waitUp()
	r.t.adminSetup()
	r.store.mu.Lock()
	for _, n := range reproStoreSnaps {
		r.store.next[n] = 1
	}
	r.store.mu.Unlock()
	for _, n := range reproStoreSnaps {
		if err := r.store.publish(n, snapSpec{Name: n, Seed: 7}, []string{"stable"}); err != nil {
			r.logf("publish %s: %v", n, err)
			return
		}
	}
	r.storeOK = true
}

func (r *repro) run() {
	scenarios := []struct {
		n   int
		run func()
	}{
		{1, r.finding1}, {2, r.finding2}, {3, r.finding3}, {4, r.finding4},
		{5, r.finding5}, {6, r.finding6}, {7, r.finding7}, {8, r.finding8and9},
		{10, r.finding10}, {11, r.finding11}, {12, r.finding12}, {13, r.finding13},
	}
	for r.round = 1; ; r.round++ {
		for _, s := range scenarios {
			r.logf("round %d: finding %d", r.round, s.n)
			s.run()
		}
	}
}

func (r *repro) name(finding int) string { return fmt.Sprintf("rp%dn%d", finding, r.round) }

func (r *repro) skip(finding int, why string, err error) {
	r.logf("finding %d: inconclusive: %s: %v", finding, why, err)
}

// ---- controls ----

func (r *repro) agentPost(path, body string, timeout time.Duration) (string, error) {
	c := http.Client{Timeout: timeout}
	rsp, err := c.Post(r.t.agent+path, "text/plain", strings.NewReader(body))
	if err != nil {
		return "", err
	}
	defer rsp.Body.Close()
	var b bytes.Buffer
	b.ReadFrom(rsp.Body)
	if rsp.StatusCode != 200 {
		return b.String(), fmt.Errorf("agent %s: status %d: %s", path, rsp.StatusCode, strings.TrimSpace(b.String()))
	}
	return b.String(), nil
}

func (r *repro) sh(script string) (string, error) { return r.agentPost("/sh", script, 70*time.Second) }

// fault restarts snapd with the given SNAPD_FAULT_INJECT ("" for none),
// clears fault stamps, and waits until snapd answers.
func (r *repro) fault(inject string) error {
	if _, err := agentGet(r.t.agent, "/fault?inject="+url.QueryEscape(inject), 100*time.Second); err != nil {
		return err
	}
	return r.waitUp(90 * time.Second)
}

func (r *repro) waitUp(d time.Duration) error {
	return waitFor(d, func() error { _, err := r.t.cli.SysInfo(); return err }, 500*time.Millisecond)
}

// fired reports whether the fault tag:kind was injected since the last fault().
func (r *repro) fired(tagKind string) bool {
	out, err := r.sh("test -e /var/lib/snapd/faults/" + tagKind + " && echo yes")
	return err == nil && strings.TrimSpace(out) == "yes"
}

func (r *repro) waitFired(tagKind string, d time.Duration) bool {
	return waitFor(d, func() error {
		if r.fired(tagKind) {
			return nil
		}
		return fmt.Errorf("not yet")
	}, 300*time.Millisecond) == nil
}

// reset puts the host back to no faults and removes the scenario's snaps.
func (r *repro) reset(names ...string) {
	agentGet(r.t.agent, "/systemctl-fail", 20*time.Second)
	agentGet(r.t.agent, "/store-up", 20*time.Second)
	if err := r.fault(""); err != nil {
		r.logf("reset: %v", err)
	}
	for _, n := range names {
		st, err := r.t.readState(n)
		if err != nil || !st.Installed {
			continue
		}
		if id, err := r.t.cli.Remove(n, nil, &client.SnapOptions{Purge: true}); err == nil {
			r.t.waitChange(id, 3*time.Minute)
		}
	}
}

// sideload starts installing a new revision of name.
func (r *repro) sideload(name string, seed uint64) (string, error) {
	p, err := snapSpec{Name: name, Seed: seed, Files: []int{4096}}.build()
	if err != nil {
		return "", err
	}
	defer os.Remove(p)
	return r.t.cli.InstallPath(p, "", &client.SnapOptions{Dangerous: true})
}

// install sideloads a revision of name and waits for it to be Done.
func (r *repro) install(name string, seed uint64) error {
	id, err := r.sideload(name, seed)
	if err != nil {
		return err
	}
	chg, err := r.t.waitChange(id, 3*time.Minute)
	if err != nil {
		return err
	}
	if chg.Status != "Done" {
		return fmt.Errorf("install %s: change %s %s: %s", name, id, chg.Status, chg.Err)
	}
	return nil
}

// waitSettled waits for a change while snapd may be restarting under it.
func (r *repro) waitSettled(id string) (*client.Change, error) {
	return r.t.waitChange(id, 4*time.Minute)
}

func taskSummary(chg *client.Change) string {
	if chg == nil {
		return ""
	}
	var b strings.Builder
	for _, t := range chg.Tasks {
		fmt.Fprintf(&b, "%s:%s ", t.Kind, t.Status)
	}
	return b.String()
}

func taskStatus(chg *client.Change, kind string) string {
	if chg == nil {
		return ""
	}
	for _, t := range chg.Tasks {
		if t.Kind == kind {
			return t.Status
		}
	}
	return ""
}

func (r *repro) details(name, id string, chg *client.Change, extra map[string]any) map[string]any {
	d := map[string]any{"snap": name, "change": id, "round": r.round}
	if chg != nil {
		d["status"] = chg.Status
		d["err"] = chg.Err
		d["tasks"] = taskSummary(chg)
	}
	if st, err := r.t.readState(name); err == nil {
		d["snapd"] = st.String()
	}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// fsHas lists what is on disk for name's revision rev.
func fsHas(v *fsView, name, rev string) []string {
	var out []string
	if v.files[name+"_"+rev+".snap"] {
		out = append(out, "blob")
	}
	if v.mounts[name+" "+rev] {
		out = append(out, "mount")
	}
	if v.unitFiles[name+" "+rev] {
		out = append(out, "unit file")
	}
	if s := v.units[name+" "+rev]; s != "" {
		out = append(out, "unit "+s)
	}
	return out
}

// ---- scenarios ----

// Finding 1: abort a sideload right after it is accepted, while
// prerequisites, which has no undo handler, is the only task that ran. No
// fault is needed: prepare-snap usually starts a second or more later. If it
// ran before the abort, the attempt does not count and is repeated.
func (r *repro) finding1() {
	for attempt := 1; attempt <= 5; attempt++ {
		if r.finding1Once(attempt) {
			return
		}
	}
}

func (r *repro) finding1Once(attempt int) (done bool) {
	n := fmt.Sprintf("%sa%d", r.name(1), attempt)
	defer r.reset(n)
	id, err := r.sideload(n, 1)
	if err != nil {
		r.skip(1, "sideload", err)
		return false
	}
	if _, err := r.t.cli.Abort(id); err != nil {
		r.skip(1, "abort", err)
		return false
	}
	chg, err := r.waitSettled(id)
	st, serr := r.t.readState(n)
	if err != nil || serr != nil {
		r.skip(1, "observe", fmt.Errorf("%v %v", err, serr))
		return false
	}
	if len(chg.Tasks) < 2 || chg.Tasks[0].Status != "Done" || chg.Tasks[1].Status != "Hold" {
		r.skip(1, "abort did not land between prerequisites and prepare-snap", fmt.Errorf("%s", taskSummary(chg)))
		return false
	}
	bug := chg.Status == "Done" && !st.Installed
	d := r.details(n, id, chg, map[string]any{"attempt": attempt})
	assert.Reachable("repro 1: the scenario reaches its check", d)
	assert.Always(!bug, "repro 1: an aborted sideload that reports Done has installed the snap", d)
	assert.Sometimes(bug, "repro 1: finding 1 reproduced", d)
	return true
}

// Finding 2: sideload a new revision of a disabled snap.
func (r *repro) finding2() {
	n := r.name(2)
	defer r.reset(n)
	if err := r.install(n, 1); err != nil {
		r.skip(2, "install", err)
		return
	}
	id, err := r.t.cli.Disable(n, nil)
	if err == nil {
		_, err = r.t.waitChange(id, 2*time.Minute)
	}
	if st, _ := r.t.readState(n); err != nil || st.Active {
		r.skip(2, "disable", err)
		return
	}
	_, err = r.sideload(n, 2)
	var cerr *client.Error
	status := 0
	if e, ok := err.(*client.Error); ok {
		cerr = e
		status = cerr.StatusCode
	}
	bug := status >= 500
	d := r.details(n, "", nil, map[string]any{"error": fmt.Sprint(err), "http_status": status})
	assert.Reachable("repro 2: the scenario reaches its check", d)
	assert.Always(!bug, "repro 2: sideloading over a disabled snap is refused as a client error", d)
	assert.Sometimes(bug, "repro 2: finding 2 reproduced", d)
}

// Finding 3: every systemctl call for the snap's mount unit fails while it
// is set up (as when systemd goes down for a reboot).
func (r *repro) finding3() {
	n := r.name(3)
	defer r.reset(n)
	if err := r.fault(""); err != nil {
		r.skip(3, "fault", err)
		return
	}
	pat := "daemon-reload|snap-" + n + "-"
	if _, err := agentGet(r.t.agent, "/systemctl-fail?pattern="+url.QueryEscape(pat), 20*time.Second); err != nil {
		r.skip(3, "systemctl shim", err)
		return
	}
	id, err := r.sideload(n, 1)
	if err != nil {
		r.skip(3, "sideload", err)
		return
	}
	chg, err := r.waitSettled(id)
	agentGet(r.t.agent, "/systemctl-fail", 20*time.Second)
	if err != nil {
		r.skip(3, "wait", err)
		return
	}
	if chg.Status == "Done" {
		r.skip(3, "install succeeded with systemctl failing", nil)
		return
	}
	// Nothing retries the cleanup; give snapd a moment anyway.
	time.Sleep(10 * time.Second)
	st, serr := r.t.readState(n)
	v, ferr := r.t.readFS()
	if serr != nil || ferr != nil {
		r.skip(3, "observe", fmt.Errorf("%v %v", serr, ferr))
		return
	}
	left := fsHas(v, n, "x1")
	bug := !st.Installed && len(left) > 0
	d := r.details(n, id, chg, map[string]any{"left_behind": left})
	assert.Reachable("repro 3: the scenario reaches its check", d)
	assert.Always(!bug, "repro 3: a failed install whose systemctl calls failed leaves no snap file or mount unit", d)
	assert.Sometimes(bug, "repro 3: finding 3 reproduced", d)
}

// Finding 4: abort a remove after clear-snap removed the data.
func (r *repro) finding4() {
	n := r.name(4)
	defer r.reset(n)
	if err := r.fault("clear-snap-done:pause"); err != nil {
		r.skip(4, "fault", err)
		return
	}
	if err := r.install(n, 1); err != nil {
		r.skip(4, "install", err)
		return
	}
	if _, err := r.sh(fmt.Sprintf("echo precious > /var/snap/%[1]s/common/data && echo precious > /var/snap/%[1]s/x1/data", n)); err != nil {
		r.skip(4, "write data", err)
		return
	}
	id, err := r.t.cli.Remove(n, nil, nil)
	if err != nil {
		r.skip(4, "remove", err)
		return
	}
	if !r.waitFired("clear-snap-done:pause", 2*time.Minute) {
		r.skip(4, "fault did not fire", nil)
		return
	}
	r.t.cli.Abort(id)
	chg, err := r.waitSettled(id)
	if err != nil {
		r.skip(4, "wait", err)
		return
	}
	if taskStatus(chg, "save-snapshot") == "" {
		r.skip(4, "remove took no automatic snapshot", nil)
		return
	}
	out, err := r.sh(fmt.Sprintf("cat /var/snap/%[1]s/common/data /var/snap/%[1]s/x1/data 2>/dev/null | grep -c precious; true", n))
	sets, serr := r.t.cli.SnapshotSets(0, []string{n})
	if err != nil || serr != nil {
		r.skip(4, "observe", fmt.Errorf("%v %v", err, serr))
		return
	}
	dataLeft := strings.TrimSpace(out) != "0"
	bug := !dataLeft && len(sets) == 0
	d := r.details(n, id, chg, map[string]any{"data_left": dataLeft, "snapshots": len(sets)})
	assert.Reachable("repro 4: the scenario reaches its check", d)
	assert.Always(!bug, "repro 4: an aborted remove keeps the snap's data or its snapshot", d)
	assert.Sometimes(bug, "repro 4: finding 4 reproduced", d)
}

// Finding 5: abort a remove after unlink-snap, for a snap whose common data
// dir an earlier change lost (as in finding 4).
func (r *repro) finding5() {
	n := r.name(5)
	defer r.reset(n)
	if err := r.fault("unlink-snap-done:pause"); err != nil {
		r.skip(5, "fault", err)
		return
	}
	if err := r.install(n, 1); err != nil {
		r.skip(5, "install", err)
		return
	}
	// Stands in for the earlier aborted remove whose clear-snap (no undo)
	// deleted the data; repro 4 shows that happening.
	if _, err := r.sh("rm -rf /var/snap/" + n + "/common"); err != nil {
		r.skip(5, "remove common", err)
		return
	}
	id, err := r.t.cli.Remove(n, nil, nil)
	if err != nil {
		r.skip(5, "remove", err)
		return
	}
	if !r.waitFired("unlink-snap-done:pause", 2*time.Minute) {
		r.skip(5, "fault did not fire", nil)
		return
	}
	r.t.cli.Abort(id)
	chg, err := r.waitSettled(id)
	st, serr := r.t.readState(n)
	if err != nil || serr != nil {
		r.skip(5, "observe", fmt.Errorf("%v %v", err, serr))
		return
	}
	if taskStatus(chg, "clear-snap") == "Done" {
		r.skip(5, "clear-snap ran", nil)
		return
	}
	bug := chg.Status != "Done" && st.Installed && !st.Active
	d := r.details(n, id, chg, nil)
	assert.Reachable("repro 5: the scenario reaches its check", d)
	assert.Always(!bug, "repro 5: an undone remove whose clear-snap never ran leaves the snap enabled", d)
	assert.Sometimes(bug, "repro 5: finding 5 reproduced", d)
}

// Finding 6: crash in discard-snap after the revision's files are removed
// but before the shorter sequence is saved; abort before the re-run.
func (r *repro) finding6() {
	n := r.name(6)
	defer r.reset(n)
	if err := r.fault(""); err != nil {
		r.skip(6, "fault", err)
		return
	}
	if err := r.install(n, 1); err != nil {
		r.skip(6, "install x1", err)
		return
	}
	if err := r.install(n, 2); err != nil {
		r.skip(6, "install x2", err)
		return
	}
	if err := r.fault("discard-snap-files-removed:panic,discard-snap-start:retry-after-crash"); err != nil {
		r.skip(6, "fault", err)
		return
	}
	id, err := r.t.cli.Remove(n, nil, &client.SnapOptions{Revision: "x1"})
	if err != nil {
		r.skip(6, "remove", err)
		return
	}
	if !r.waitFired("discard-snap-start:retry-after-crash", 3*time.Minute) {
		r.skip(6, "crash and re-run did not happen", nil)
		return
	}
	r.t.cli.Abort(id)
	chg, err := r.waitSettled(id)
	st, serr := r.t.readState(n)
	v, ferr := r.t.readFS()
	if err != nil || serr != nil || ferr != nil {
		r.skip(6, "observe", fmt.Errorf("%v %v %v", err, serr, ferr))
		return
	}
	bug := st.hasRev("x1") && !v.files[n+"_x1.snap"]
	d := r.details(n, id, chg, map[string]any{"x1_on_disk": fsHas(v, n, "x1")})
	assert.Reachable("repro 6: the scenario reaches its check", d)
	assert.Always(!bug, "repro 6: every revision snapd lists after an interrupted discard has its snap file", d)
	assert.Sometimes(bug, "repro 6: finding 6 reproduced", d)
}

// Finding 7: crash after discard-snap saved its work but before the task is
// Done; the re-run forgets the snap's remaining, active revision.
func (r *repro) finding7() {
	n := r.name(7)
	defer r.reset(n)
	if err := r.fault(""); err != nil {
		r.skip(7, "fault", err)
		return
	}
	if err := r.install(n, 1); err != nil {
		r.skip(7, "install x1", err)
		return
	}
	if err := r.install(n, 2); err != nil {
		r.skip(7, "install x2", err)
		return
	}
	if err := r.fault("discard-snap-done:panic"); err != nil {
		r.skip(7, "fault", err)
		return
	}
	id, err := r.t.cli.Remove(n, nil, &client.SnapOptions{Revision: "x1"})
	if err != nil {
		r.skip(7, "remove", err)
		return
	}
	chg, err := r.waitSettled(id)
	if err != nil || !r.fired("discard-snap-done:panic") {
		r.skip(7, "crash did not happen", err)
		return
	}
	st, serr := r.t.readState(n)
	v, ferr := r.t.readFS()
	if serr != nil || ferr != nil {
		r.skip(7, "observe", fmt.Errorf("%v %v", serr, ferr))
		return
	}
	left := fsHas(v, n, "x2")
	bug := !st.Installed && len(left) > 0
	d := r.details(n, id, chg, map[string]any{"x2_on_disk": left, "current_symlink": v.current[n]})
	assert.Reachable("repro 7: the scenario reaches its check", d)
	assert.Always(!bug, "repro 7: removing one revision keeps the snap installed across a re-run of discard-snap", d)
	assert.Sometimes(bug, "repro 7: finding 7 reproduced", d)
}

// Findings 8 and 9: crash in mount-snap after the upload is removed; the
// revision leaks. Then a new sideload of the same snap is given the leaked
// revision number and its file is copied over the mounted one.
func (r *repro) finding8and9() {
	n := r.name(8)
	defer r.reset(n)
	if err := r.fault("mount-snap-upload-removed:panic"); err != nil {
		r.skip(8, "fault", err)
		return
	}
	id, err := r.sideload(n, 1)
	if err != nil {
		r.skip(8, "sideload", err)
		return
	}
	chg, err := r.waitSettled(id)
	if err != nil || !r.fired("mount-snap-upload-removed:panic") {
		r.skip(8, "crash did not happen", err)
		return
	}
	st, serr := r.t.readState(n)
	v, ferr := r.t.readFS()
	if serr != nil || ferr != nil {
		r.skip(8, "observe", fmt.Errorf("%v %v", serr, ferr))
		return
	}
	left := fsHas(v, n, "x1")
	bug8 := !st.Installed && len(left) > 0
	d := r.details(n, id, chg, map[string]any{"x1_on_disk": left})
	assert.Reachable("repro 8: the scenario reaches its check", d)
	assert.Always(!bug8, "repro 8: a sideload interrupted in mount-snap leaves nothing mounted for a snap snapd does not have", d)
	assert.Sometimes(bug8, "repro 8: finding 8 reproduced", d)
	if !bug8 || !v.mounts[n+" x1"] {
		r.skip(9, "no leaked, mounted revision to reuse", nil)
		return
	}

	// Finding 9.
	if err := r.fault(""); err != nil {
		r.skip(9, "fault", err)
		return
	}
	blob := "/var/lib/snapd/snaps/" + n + "_x1.snap"
	before, err := r.sh("stat -c '%i %s' " + blob)
	if err != nil {
		r.skip(9, "stat", err)
		return
	}
	p, err := snapSpec{Name: n, Seed: 2, Files: []int{256 << 10}}.build()
	if err != nil {
		r.skip(9, "build", err)
		return
	}
	defer os.Remove(p)
	id, err = r.t.cli.InstallPath(p, "", &client.SnapOptions{Dangerous: true})
	if err != nil {
		r.skip(9, "sideload", err)
		return
	}
	chg, err = r.waitSettled(id)
	if err != nil {
		r.skip(9, "wait", err)
		return
	}
	after, _ := r.sh("stat -c '%i %s' " + blob + " 2>/dev/null; ls /snap/" + n + "/x1 >/dev/null 2>&1 || echo unreadable")
	bi := strings.Fields(before)
	ai := strings.Fields(after)
	sameInodeNewContent := len(bi) == 2 && len(ai) >= 2 && bi[0] == ai[0] && bi[1] != ai[1]
	aborted := strings.Contains(chg.Err, "connection abort") || strings.Contains(chg.Err, "not connected") ||
		strings.Contains(after, "unreadable")
	bug9 := sameInodeNewContent || aborted
	d = r.details(n, id, chg, map[string]any{"blob_before": strings.TrimSpace(before), "blob_after": strings.TrimSpace(after)})
	assert.Reachable("repro 9: the scenario reaches its check", d)
	assert.Always(!bug9, "repro 9: installing a snap never rewrites a mounted snap file in place", d)
	assert.Sometimes(bug9, "repro 9: finding 9 reproduced", d)
}

// Finding 10: crash in discard-snap while removing a snap's last revision;
// after the restart, snapd's mount-unit ensure recreates the unit while the
// re-run discard-snap has the state unlocked, and the unit is left behind.
func (r *repro) finding10() {
	n := r.name(10)
	defer r.reset(n)
	if err := r.fault(""); err != nil {
		r.skip(10, "fault", err)
		return
	}
	if err := r.install(n, 1); err != nil {
		r.skip(10, "install", err)
		return
	}
	if err := r.fault("discard-snap-files-removed:panic,discard-snap-unlocked:pause-after-crash"); err != nil {
		r.skip(10, "fault", err)
		return
	}
	id, err := r.t.cli.Remove(n, nil, nil)
	if err != nil {
		r.skip(10, "remove", err)
		return
	}
	if !r.waitFired("discard-snap-unlocked:pause-after-crash", 3*time.Minute) {
		r.skip(10, "crash and re-run did not happen", nil)
		return
	}
	// Make snapd run its managers' Ensure while discard-snap is unlocked.
	for _, v := range []int{3, 2} {
		r.t.cli.SetConf("system", map[string]any{"refresh.retain": v})
		time.Sleep(time.Second)
	}
	chg, err := r.waitSettled(id)
	st, serr := r.t.readState(n)
	v, ferr := r.t.readFS()
	if err != nil || serr != nil || ferr != nil {
		r.skip(10, "observe", fmt.Errorf("%v %v %v", err, serr, ferr))
		return
	}
	left := fsHas(v, n, "x1")
	bug := !st.Installed && chg.Status == "Done" && len(left) > 0
	d := r.details(n, id, chg, map[string]any{"x1_on_disk": left})
	assert.Reachable("repro 10: the scenario reaches its check", d)
	assert.Always(!bug, "repro 10: a completed remove interrupted in discard-snap leaves no mount unit behind", d)
	assert.Sometimes(bug, "repro 10: finding 10 reproduced", d)
}

// Finding 11: the store goes away while a multi-snap refresh runs its final
// check-rerefresh task, after every snap was refreshed.
func (r *repro) finding11() {
	if !r.storeOK {
		r.skip(11, "store snaps not published", nil)
		return
	}
	defer r.reset(reproStoreSnaps...)
	if err := r.fault(""); err != nil {
		r.skip(11, "fault", err)
		return
	}
	for _, n := range reproStoreSnaps {
		id, err := r.t.cli.Install(n, nil, &client.SnapOptions{Channel: "stable"})
		if err == nil {
			_, err = r.t.waitChange(id, 3*time.Minute)
		}
		if st, _ := r.t.readState(n); err != nil || !st.Installed {
			r.skip(11, "install "+n, err)
			return
		}
	}
	before := map[string]string{}
	for _, n := range reproStoreSnaps {
		st, _ := r.t.readState(n)
		before[n] = st.Current
		if err := r.store.publish(n, snapSpec{Name: n, Seed: uint64(r.round)}, []string{"stable"}); err != nil {
			r.skip(11, "publish", err)
			return
		}
	}
	if err := r.fault("check-rerefresh-store:pause"); err != nil {
		r.skip(11, "fault", err)
		return
	}
	id, err := r.t.cli.RefreshMany(reproStoreSnaps, nil, nil)
	if err != nil {
		r.skip(11, "refresh", err)
		return
	}
	if !r.waitFired("check-rerefresh-store:pause", 3*time.Minute) {
		r.skip(11, "fault did not fire", nil)
		return
	}
	agentGet(r.t.agent, "/store-down", 20*time.Second)
	chg, err := r.waitSettled(id)
	agentGet(r.t.agent, "/store-up", 20*time.Second)
	if err != nil {
		r.skip(11, "wait", err)
		return
	}
	refreshed := true
	after := map[string]string{}
	for _, n := range reproStoreSnaps {
		st, _ := r.t.readState(n)
		after[n] = st.Current
		refreshed = refreshed && st.Current != before[n]
	}
	if !refreshed {
		r.skip(11, "snaps not refreshed", fmt.Errorf("before %v after %v change %s %s: %s", before, after, chg.Status, taskSummary(chg), chg.Err))
		return
	}
	bug := chg.Status == "Error"
	d := r.details(reproStoreSnaps[0], id, chg, map[string]any{"before": before, "after": after})
	assert.Reachable("repro 11: the scenario reaches its check", d)
	assert.Always(!bug, "repro 11: a refresh that refreshed every snap does not report an error", d)
	assert.Sometimes(bug, "repro 11: finding 11 reproduced", d)
}

// Finding 12: concurrent requests to the fake store as it was before its
// fixes (store:11030), each finding snap files it has not cached yet.
func (r *repro) finding12() {
	base := strings.TrimSuffix(envOr("UNFIXED_STORE_URL", "http://store:11030"), "/")
	dir := filepath.Join(r.store.dir, "unfixed")
	hc := &http.Client{Timeout: 20 * time.Second}
	if err := waitFor(30*time.Second, func() error {
		rsp, err := hc.Get(base + "/")
		if err == nil {
			rsp.Body.Close()
		}
		return err
	}, time.Second); err != nil {
		r.skip(12, "unfixed store not up", err)
		return
	}
	var failures []string
	// A burst only races while the store has files it has not cached, so
	// each burst brings new ones; a few bursts make a crash very likely.
	for burst := 0; burst < 5 && len(failures) == 0; burst++ {
		f, err := r.finding12Burst(base, dir, burst, hc)
		if err != nil {
			r.skip(12, "burst", err)
			return
		}
		failures = f
	}
	bug := len(failures) > 0
	if len(failures) > 3 {
		failures = failures[:3]
	}
	d := map[string]any{"round": r.round, "failures": failures}
	assert.Reachable("repro 12: the scenario reaches its check", d)
	assert.Always(!bug, "repro 12: the fake store answers concurrent requests", d)
	assert.Sometimes(bug, "repro 12: finding 12 reproduced", d)
}

func (r *repro) finding12Burst(base, dir string, burst int, hc *http.Client) ([]string, error) {
	old, _ := filepath.Glob(filepath.Join(dir, "*.snap"))
	for _, f := range old {
		os.Remove(f)
	}
	p, err := snapSpec{Name: "rp12", Seed: uint64(r.round)}.build()
	if err != nil {
		return nil, err
	}
	defer os.Remove(p)
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	for i := range 32 {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("rp12_%d%d%02d.snap", r.round, burst, i)), b, 0644); err != nil {
			return nil, err
		}
	}
	var mu sync.Mutex
	var failures []string
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rsp, err := hc.Get(base + "/api/v1/snaps/details/rp12")
			msg := ""
			if err != nil {
				msg = err.Error()
			} else {
				rsp.Body.Close()
				if rsp.StatusCode >= 500 {
					msg = rsp.Status
				}
			}
			if msg != "" {
				mu.Lock()
				failures = append(failures, msg)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return failures, nil
}

// Finding 13: crash in link-snap after the current symlink exists but before
// the undo data is saved; abort before the re-run.
func (r *repro) finding13() {
	n := r.name(13)
	defer r.reset(n)
	if err := r.fault("link-snap-linked:panic,link-snap-start:retry-after-crash"); err != nil {
		r.skip(13, "fault", err)
		return
	}
	id, err := r.sideload(n, 1)
	if err != nil {
		r.skip(13, "sideload", err)
		return
	}
	if !r.waitFired("link-snap-start:retry-after-crash", 3*time.Minute) {
		r.skip(13, "crash and re-run did not happen", nil)
		return
	}
	r.t.cli.Abort(id)
	chg, err := r.waitSettled(id)
	st, serr := r.t.readState(n)
	v, ferr := r.t.readFS()
	if err != nil || serr != nil || ferr != nil {
		r.skip(13, "observe", fmt.Errorf("%v %v %v", err, serr, ferr))
		return
	}
	bug := !st.Installed && v.current[n] != ""
	d := r.details(n, id, chg, map[string]any{"current_symlink": v.current[n]})
	assert.Reachable("repro 13: the scenario reaches its check", d)
	assert.Always(!bug, "repro 13: an aborted install leaves no current symlink for a snap that is not installed", d)
	assert.Sometimes(bug, "repro 13: finding 13 reproduced", d)
}
