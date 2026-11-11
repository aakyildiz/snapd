package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antithesishq/antithesis-sdk-go/assert"
	"hegel.dev/go/hegel"

	"github.com/snapcore/snapd/client"
)

type opKind string

const (
	opInstall opKind = "install"
	opRemove  opKind = "remove"
	opRevert  opKind = "revert"
	opEnable  opKind = "enable"
	opDisable opKind = "disable"
	opSetConf opKind = "set-conf"

	// store ops (store.go)
	opStoreInstall opKind = "store-install"
	opRefresh      opKind = "refresh"
	opRefreshAll   opKind = "refresh-all"
	opSwitch       opKind = "switch"
	opHold         opKind = "hold"
	opUnhold       opKind = "unhold"
)

// op is one request that may start a change.
type op struct {
	kind   opKind
	t      *target
	names  []string
	txn    client.TransactionType // install only
	rev    string                 // remove only: a specific revision
	key    string                 // set-conf only
	val    string                 // set-conf only
	broken map[string]bool        // install only: snaps whose image is corrupt
	risk   string                 // store-install, refresh, switch: the channel's risk ("" = keep tracking)
	maxAt  map[string]int         // store ops: per snap, the channel's latest revision at submit

	// per snap, filled at submit time
	judgeable map[string]bool
	before    map[string]snapState

	id       string
	sysGen   int64 // set-conf only
	aborted  atomic.Bool
	crashed  atomic.Bool // the driver crashed snapd while this was in flight
	submitAt time.Time
}

func (o *op) String() string {
	s := fmt.Sprintf("%s %v", o.kind, o.names)
	switch o.kind {
	case opInstall:
		s += fmt.Sprintf(" txn=%q broken=%v", o.txn, o.broken)
	case opRemove:
		if o.rev != "" {
			s += " rev=" + o.rev
		}
	case opStoreInstall, opRefresh, opSwitch:
		s += fmt.Sprintf(" risk=%q maxAt=%v", o.risk, o.maxAt)
	case opSetConf:
		s += fmt.Sprintf(" %s=%s", o.key, o.val)
	}
	if o.id != "" {
		s += " change=" + o.id
	}
	return s
}

// workload is the hegel state machine.
type workload struct {
	targets []*target
	store   *storeModel

	inflightMu sync.Mutex
	inflight   map[*op]struct{}

	evMu sync.Mutex

	stats sync.Map // label -> *atomic.Int64
}

func newWorkload(ts []*target) *workload {
	return &workload{targets: ts, inflight: map[*op]struct{}{}}
}

func (w *workload) count(label string) { w.countN(label) }

func (w *workload) countN(label string) int64 {
	v, _ := w.stats.LoadOrStore(label, new(atomic.Int64))
	return v.(*atomic.Int64).Add(1)
}

// definiteErr: snapd answered, and refused the request.
func definiteErr(err error) bool {
	var cerr *client.Error
	return errors.As(err, &cerr) && cerr.StatusCode >= 400 && cerr.StatusCode < 500
}

func (w *workload) reportLoop() {
	for {
		time.Sleep(30 * time.Second)
		var parts []string
		w.stats.Range(func(k, v any) bool {
			parts = append(parts, fmt.Sprintf("%s=%d", k, v.(*atomic.Int64).Load()))
			return true
		})
		sort.Strings(parts)
		log.Printf("stats: %s", strings.Join(parts, " "))
	}
}

func (w *workload) drawTarget(tc hegel.TestCase) *target {
	return w.targets[hegel.Draw(tc, hegel.Integers(0, len(w.targets)-1))]
}

func drawName(tc hegel.TestCase) string {
	return hegel.Draw(tc, hegel.SampledFrom(allSnapNames))
}

// drawNameWhere usually picks a snap the model says fits, so actions that
// need a particular state (two revisions, disabled) get to run.
func (t *target) drawNameWhere(tc hegel.TestCase, fits func(snapState) bool) string {
	var ok []string
	for _, n := range allSnapNames {
		m := t.snaps[n]
		m.mu.Lock()
		if fits(m.state) {
			ok = append(ok, n)
		}
		m.mu.Unlock()
	}
	if len(ok) == 0 || hegel.Draw(tc, hegel.WeightedBooleans(0.2)) {
		return drawName(tc)
	}
	return hegel.Draw(tc, hegel.SampledFrom(ok))
}

// ---- rules ----

// RuleInstall sideloads one to three generated snaps in one change.
func (w *workload) RuleInstall(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	n := 1
	if hegel.Draw(tc, hegel.WeightedBooleans(0.3)) {
		n = hegel.Draw(tc, hegel.Integers(2, len(snapNames)))
	}
	pool := append([]string(nil), snapNames...)
	var picked []string
	for range n {
		i := hegel.Draw(tc, hegel.Integers(0, len(pool)-1))
		picked = append(picked, pool[i])
		pool = append(pool[:i], pool[i+1:]...)
	}
	o := &op{kind: opInstall, t: t, names: picked, broken: map[string]bool{}}
	if n > 1 && hegel.Draw(tc, hegel.Booleans()) {
		o.txn = client.TransactionAllSnaps
	} else if n > 1 {
		o.txn = client.TransactionPerSnap
	}
	var paths []string
	for _, name := range o.names {
		spec := snapSpec{
			Name:    name,
			Seed:    hegel.Draw(tc, hegel.Integers(uint64(0), math64)),
			Plug:    hegel.Draw(tc, hegel.WeightedBooleans(0.3)),
			Corrupt: hegel.Draw(tc, hegel.WeightedBooleans(0.12)),
		}
		nf := hegel.Draw(tc, hegel.Integers(0, 3))
		for range nf {
			// mostly small, sometimes a few MB so copies take a while
			if hegel.Draw(tc, hegel.WeightedBooleans(0.15)) {
				spec.Files = append(spec.Files, hegel.Draw(tc, hegel.Integers(1<<20, 6<<20)))
			} else {
				spec.Files = append(spec.Files, hegel.Draw(tc, hegel.Integers(0, 64<<10)))
			}
		}
		o.broken[name] = spec.Corrupt
		p, err := spec.build()
		if err != nil {
			log.Printf("build snap: %v", err)
			return
		}
		defer os.Remove(p)
		paths = append(paths, p)
	}
	opts := &client.SnapOptions{Dangerous: true, Transaction: o.txn}
	w.submit(o, func() (string, error) {
		if len(paths) == 1 {
			return t.cli.InstallPath(paths[0], "", opts)
		}
		return t.cli.InstallPathMany(paths, opts)
	})
}

// RuleRemove removes a snap, or one of its non-current revisions.
func (w *workload) RuleRemove(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	name := t.drawNameWhere(tc, func(s snapState) bool { return s.Installed })
	o := &op{kind: opRemove, t: t, names: []string{name}}
	opts := &client.SnapOptions{}
	if hegel.Draw(tc, hegel.WeightedBooleans(0.4)) {
		m := t.snaps[name]
		m.mu.Lock()
		var others []string
		for _, r := range m.state.Revs {
			if r != m.state.Current {
				others = append(others, r)
			}
		}
		m.mu.Unlock()
		if len(others) > 0 {
			o.rev = hegel.Draw(tc, hegel.SampledFrom(others))
			opts.Revision = o.rev
		}
	}
	opts.Purge = hegel.Draw(tc, hegel.Booleans())
	w.submit(o, func() (string, error) { return t.cli.Remove(name, nil, opts) })
}

// RuleRevert reverts a snap to its previous revision.
func (w *workload) RuleRevert(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	name := t.drawNameWhere(tc, func(s snapState) bool { return s.Installed && len(s.Revs) > 1 })
	o := &op{kind: opRevert, t: t, names: []string{name}}
	w.submit(o, func() (string, error) { return t.cli.Revert(name, nil) })
}

// RuleToggle enables or disables a snap.
func (w *workload) RuleToggle(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	enable := hegel.Draw(tc, hegel.Booleans())
	name := t.drawNameWhere(tc, func(s snapState) bool { return s.Installed && s.Active != enable })
	if enable {
		o := &op{kind: opEnable, t: t, names: []string{name}}
		w.submit(o, func() (string, error) { return t.cli.Enable(name, nil) })
	} else {
		o := &op{kind: opDisable, t: t, names: []string{name}}
		w.submit(o, func() (string, error) { return t.cli.Disable(name, nil) })
	}
}

// RuleSetConf sets the system's refresh.retain, which decides how many
// revisions an install keeps. (Snap configuration needs a configure hook,
// and hooks need a base snap, which the test snaps don't have.)
func (w *workload) RuleSetConf(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	o := &op{kind: opSetConf, t: t, key: "refresh.retain",
		val: strconv.Itoa(hegel.Draw(tc, hegel.Integers(2, 4)))}
	w.submit(o, func() (string, error) {
		return t.cli.SetConf("system", map[string]any{o.key: o.val})
	})
}

// RuleAbort aborts a change the driver is still waiting on.
func (w *workload) RuleAbort(tc hegel.TestCase) {
	w.inflightMu.Lock()
	var ops []*op
	for o := range w.inflight {
		ops = append(ops, o)
	}
	w.inflightMu.Unlock()
	if len(ops) == 0 {
		return
	}
	sort.Slice(ops, func(i, j int) bool { return ops[i].submitAt.Before(ops[j].submitAt) })
	o := ops[hegel.Draw(tc, hegel.Integers(0, len(ops)-1))]
	o.aborted.Store(true)
	_, err := o.t.cli.Abort(o.id)
	w.checkAPIError(err, "abort")
	if err == nil {
		w.count("abort-ok")
	} else {
		w.count("abort-err")
	}
}

// RuleCrash kills snapd, restarts it, or power-cycles its container.
func (w *workload) RuleCrash(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	t.crashMu.Lock()
	if time.Since(t.lastCrash) < 60*time.Second {
		t.crashMu.Unlock()
		return
	}
	t.lastCrash = time.Now()
	t.crashMu.Unlock()
	kind := hegel.Draw(tc, hegel.SampledFrom([]string{"kill-snapd", "kill-snapd", "kill-snapd", "kill-snapd", "restart-snapd", "restart-snapd", "reboot"}))
	w.inflightMu.Lock()
	for o := range w.inflight {
		if o.t == t {
			o.crashed.Store(true)
		}
	}
	w.inflightMu.Unlock()
	w.count(kind)
	t.logf("crash: %s", kind)
	go func() {
		if _, err := agentGet(t.agent, "/"+kind, 150*time.Second); err != nil {
			t.logf("crash %s: %v", kind, err)
		}
	}()
}

// RuleCheck compares snapd's state, the model, and the container's
// filesystem while no change is in flight.
func (w *workload) RuleCheck(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	w.checkTarget(t, false)
}

// RuleEventually stops faults and checks that snapd recovers.
func (w *workload) RuleEventually(tc hegel.TestCase) {
	if os.Getenv("NO_EVENTUALLY") != "" {
		return
	}
	if !hegel.Draw(tc, hegel.WeightedBooleans(0.002)) {
		return
	}
	w.eventually()
}

const math64 = uint64(1<<63 - 1)

// ---- submission and change tracking ----

func (w *workload) checkAPIError(err error, what string) {
	var cerr *client.Error
	if !errors.As(err, &cerr) {
		return
	}
	d := map[string]any{"request": what, "status": cerr.StatusCode, "kind": cerr.Kind, "message": cerr.Message}
	if strings.Contains(cerr.Message, "cannot update disabled snap") {
		// Known finding, kept apart so the general property stays meaningful.
		assert.Always(cerr.StatusCode < 500, "snapd rejects an update of a disabled snap as a client error", d)
		return
	}
	assert.Always(cerr.StatusCode < 500, "snapd never answers a request with an internal server error", d)
}

// submit sends a request that may start a change on o.names, and tracks it.
func (w *workload) submit(o *op, send func() (string, error)) {
	t := o.t
	t.gate.RLock()
	defer t.gate.RUnlock()

	var lastErr error
	o.judgeable = map[string]bool{}
	o.before = map[string]snapState{}
	overlap := map[string]bool{}
	for _, name := range o.names {
		m := t.snaps[name]
		m.mu.Lock()
		overlap[name] = m.pending > 0
		o.judgeable[name] = m.known && !m.tainted && m.pending == 0
		o.before[name] = m.state
		m.pending++
		m.mu.Unlock()
	}
	if o.kind == opSetConf {
		t.sysMu.Lock()
		t.sysGen++
		o.sysGen = t.sysGen
		if t.sysPending > 0 {
			t.sysOverlapGen = t.sysGen // everything in flight now overlaps
		}
		t.sysPending++
		t.sysMu.Unlock()
		defer func() {
			if o.id == "" { // not accepted: no change to track
				t.sysMu.Lock()
				t.sysPending--
				if !definiteErr(lastErr) {
					// it may still apply, later
					t.sysTaintedUntil = time.Now().Add(2 * time.Minute)
				}
				t.sysMu.Unlock()
			}
		}()
	}
	o.submitAt = time.Now()
	id, err := send()
	lastErr = err
	w.checkAPIError(err, o.String())

	if err != nil && !definiteErr(err) {
		// snapd is likely down; don't spin.
		time.Sleep(500 * time.Millisecond)
	}
	if err == nil {
		o.id = id
		w.count(string(o.kind) + "-accepted")
		for name := range overlap {
			if overlap[name] {
				m := t.snaps[name]
				m.mu.Lock()
				m.tainted = true
				m.mu.Unlock()
				w.count("overlap")
			}
		}
		w.inflightMu.Lock()
		w.inflight[o] = struct{}{}
		w.inflightMu.Unlock()
		go w.track(o)
		return
	}

	// The request didn't start a change, or we can't tell.
	var cerr *client.Error
	errors.As(err, &cerr)
	definite := definiteErr(err)
	if definite {
		w.count(string(o.kind) + "-rejected")
		if cerr.Kind == client.ErrorKindSnapChangeConflict {
			w.count("conflict")
			assert.Sometimes(true, "snapd rejects a request that conflicts with a change in progress", map[string]any{"op": o.String()})
		}
	} else {
		if w.countN(string(o.kind)+"-unknown")%50 == 1 {
			t.logf("%s: outcome unknown: %v", o, err)
		}
	}
	for _, name := range o.names {
		m := t.snaps[name]
		m.mu.Lock()
		m.pending--
		if !definite {
			m.tainted = true
		}
		m.mu.Unlock()
	}
}

// track polls a change until it's ready, then judges it.
func (w *workload) track(o *op) {
	t := o.t
	backoff := 300 * time.Millisecond
	var chg *client.Change
	for {
		c, err := t.cli.Change(o.id)
		if err == nil && c.Ready {
			chg = c
			break
		}
		var cerr *client.Error
		if errors.As(err, &cerr) && cerr.StatusCode == 404 {
			assert.Unreachable("an accepted change is never lost", map[string]any{
				"op": o.String(), "error": err.Error()})
			break
		}
		time.Sleep(backoff)
		if err != nil {
			backoff = min(backoff*2, 2*time.Second)
		} else {
			backoff = 300 * time.Millisecond
		}
	}
	w.inflightMu.Lock()
	delete(w.inflight, o)
	w.inflightMu.Unlock()
	w.finish(o, chg)
}

// finish updates the model with a ready change and judges its outcome.
func (w *workload) finish(o *op, chg *client.Change) {
	t := o.t
	status := "lost"
	if chg != nil {
		status = chg.Status
	}
	w.count(fmt.Sprintf("%s-%s", o.kind, status))
	if chg != nil {
		w.exploreOutcome(o, chg)
	}
	if o.kind == opSetConf {
		t.sysMu.Lock()
		t.sysPending--
		// Judge only a set that no other set overlapped: concurrent
		// configure changes may apply in either order.
		alone := t.sysGen == o.sysGen && t.sysOverlapGen < o.sysGen && t.sysPending == 0 &&
			o.submitAt.After(t.sysTaintedUntil)
		if chg != nil && chg.Status == "Done" && alone {
			conf, err := t.cli.Conf("system", []string{o.key})
			if err == nil {
				got := fmt.Sprint(conf[o.key])
				assert.Always(got == o.val, "a completed configuration change is readable",
					map[string]any{"host": t.host, "key": o.key, "want": o.val, "got": got, "change": o.id})
			}
		}
		t.sysMu.Unlock()
	}
	for _, name := range o.names {
		m := t.snaps[name]
		m.mu.Lock()
		m.pending--
		if m.pending > 0 || chg == nil {
			m.tainted = true
			m.mu.Unlock()
			continue
		}
		judge := o.judgeable[name] && !m.tainted
		switch {
		case !o.judgeable[name]:
			w.count("nojudge-at-submit")
		case m.tainted:
			w.count("nojudge-tainted")
		}
		after, err := t.readQuiet(name)
		if err != nil {
			// snapd is down, or another change on this snap started that the
			// driver doesn't know of; resync later.
			m.known, m.tainted = false, true
			m.mu.Unlock()
			continue
		}
		if judge {
			w.judge(o, name, chg, o.before[name], after)
		}
		m.state, m.known, m.tainted = after, true, false
		m.mu.Unlock()
	}
}

// readQuiet reads a snap's state, failing if any change on it is in progress.
func (t *target) readQuiet(name string) (snapState, error) {
	chgs, err := t.inProgress(name)
	if err != nil {
		return snapState{}, err
	}
	if len(chgs) > 0 {
		return snapState{}, fmt.Errorf("%d changes in progress on %s", len(chgs), name)
	}
	st, err := t.readState(name)
	if err != nil {
		return st, err
	}
	chgs, err = t.inProgress(name)
	if err != nil {
		return snapState{}, err
	}
	if len(chgs) > 0 {
		return snapState{}, fmt.Errorf("%d changes in progress on %s", len(chgs), name)
	}
	return st, nil
}

func (w *workload) exploreOutcome(o *op, chg *client.Change) {
	failed := chg.Status != "Done"
	d := map[string]any{"op": o.String(), "status": chg.Status, "err": chg.Err}
	assert.Sometimes(o.kind == opInstall && !failed, "an install completes", d)
	assert.Sometimes(o.kind == opInstall && failed, "an install fails", d)
	assert.Sometimes(o.kind == opInstall && failed && o.txn == client.TransactionAllSnaps, "a multi-snap all-snaps install fails", d)
	assert.Sometimes(o.kind == opRemove && !failed, "a remove completes", d)
	assert.Sometimes(o.kind == opRevert && !failed, "a revert completes", d)
	assert.Sometimes(o.kind == opDisable && !failed, "a disable completes", d)
	assert.Sometimes(o.kind == opEnable && !failed, "an enable completes", d)
	assert.Sometimes(o.kind == opSetConf && !failed, "a configuration change completes", d)
	assert.Sometimes(o.aborted.Load() && chg.Status == "Undone", "an aborted change is undone", d)
	assert.Sometimes(o.crashed.Load() && !failed, "a change completes after snapd was crashed during it", d)
	assert.Sometimes(o.crashed.Load() && failed, "a change fails after snapd was crashed during it", d)
}

// judge checks one snap's outcome against its state before the change.
func (w *workload) judge(o *op, name string, chg *client.Change, before, after snapState) {
	done := chg.Status == "Done"
	d := map[string]any{"op": o.String(), "snap": name, "status": chg.Status, "err": chg.Err,
		"before": before.String(), "after": after.String(), "aborted": o.aborted.Load(), "crashed": o.crashed.Load()}
	unchanged := after.equal(before)
	if done && o.aborted.Load() && unchanged {
		// Known finding: abort marks pending tasks Hold, and tasks without an
		// undo handler (prerequisites) stay Done, so the change reports Done.
		assert.Unreachable("an aborted change that reports Done has taken effect", d)
		return
	}
	if !done {
		// Some tasks have no undo by design: clear-snap (a revision's data)
		// and discard-snap (a revision's files). If an abort or error lands
		// after one ran, its effect stays. Expect exactly those effects.
		want, irreversible := expectAfterIrreversible(before, name, chg)
		d["expected"] = want.String()
		assert.Sometimes(irreversible && o.aborted.Load(), "an abort lands after a change's point of no return", d)
		ok := after.equal(want)
		if o.kind == opInstall && o.txn == client.TransactionPerSnap {
			// Each snap is its own transaction: this one may have made it.
			ok = ok || (after.Installed && after.Active && !before.hasRev(after.Current))
		}
		if !ok && refusedRelink(chg, name) && !irreversible {
			// Known finding: undoUnlinkSnap won't relink a snap if its data
			// dirs are missing, taking that as "this change's clear-snap ran",
			// although an earlier change may have removed them.
			assert.Unreachable("an undone change relinks a snap whose data it did not remove", d)
			return
		}
		assert.Always(ok, "a change that does not complete leaves its snaps as they were", d)
		return
	}
	if w.judgeStore(o, name, before, after, d) {
		return
	}
	switch o.kind {
	case opInstall:
		ok := after.Installed && after.Active && !before.hasRev(after.Current) &&
			(!before.Installed || after.hasRev(before.Current))
		assert.Always(ok, "a completed install makes a new revision current and keeps the previous one", d)
	case opRemove:
		if o.rev == "" {
			assert.Always(!after.Installed, "a completed remove removes the snap", d)
		} else {
			ok := after.Installed && !after.hasRev(o.rev) && after.Current == before.Current && after.Active == before.Active
			assert.Always(ok, "a completed revision remove removes only that revision", d)
		}
	case opRevert:
		ok := after.Installed && after.Current != before.Current && before.hasRev(after.Current) &&
			strings.Join(after.Revs, ",") == strings.Join(before.Revs, ",") && after.Active
		assert.Always(ok, "a completed revert makes an earlier revision current", d)
	case opEnable, opDisable:
		ok := after.Installed && after.Active == (o.kind == opEnable) && after.Current == before.Current &&
			strings.Join(after.Revs, ",") == strings.Join(before.Revs, ",")
		assert.Always(ok, "a completed enable or disable changes only whether the snap is active", d)
	}
}

var taskRevRe = regexp.MustCompile(`"([^"]+)" \((x?[0-9]+)\)`)

// expectAfterIrreversible applies to before the effects of the change's
// tasks that snapd never undoes, for snap name.
func expectAfterIrreversible(before snapState, name string, chg *client.Change) (snapState, bool) {
	want := before
	want.Revs = append([]string(nil), before.Revs...)
	irreversible := false
	for _, task := range chg.Tasks {
		if task.Status != "Done" || (task.Kind != "discard-snap" && task.Kind != "clear-snap") {
			continue
		}
		m := taskRevRe.FindStringSubmatch(task.Summary)
		if m == nil || m[1] != name {
			continue
		}
		irreversible = true
		rev := m[2]
		switch task.Kind {
		case "discard-snap":
			var kept []string
			for _, r := range want.Revs {
				if r != rev {
					kept = append(kept, r)
				}
			}
			want.Revs = kept
			if len(kept) == 0 {
				want = snapState{}
			}
		case "clear-snap":
			// undoUnlinkSnap won't relink a snap whose data is gone.
			if rev == want.Current {
				want.Active = false
			}
		}
	}
	return want, irreversible
}

// refusedRelink: some task of chg logged that it would not link name back.
func refusedRelink(chg *client.Change, name string) bool {
	for _, task := range chg.Tasks {
		for _, l := range task.Log {
			if strings.Contains(l, fmt.Sprintf("cannot link snap %q back", name)) {
				return true
			}
		}
	}
	return false
}

// ---- consistency ----

type fsView struct {
	files       map[string]bool // name_rev.snap
	mounts      map[string]bool // name rev
	current     map[string]string
	data        map[string]bool // name rev
	dataCurrent map[string]string
	units       map[string]string // "name rev" -> active state
	unitFiles   map[string]bool   // "name rev", from /etc/systemd/system
}

// unitKey maps snap-<escaped name>-<rev>.mount to "name rev".
func unitKey(unit string) (string, bool) {
	parts := strings.Split(strings.TrimSuffix(unit, ".mount"), "-")
	if len(parts) != 3 || parts[0] != "snap" {
		return "", false
	}
	return strings.ReplaceAll(parts[1], `\x2d`, "-") + " " + parts[2], true
}

func (t *target) readFS() (*fsView, error) {
	out, err := agentGet(t.agent, "/fs", 40*time.Second)
	if err != nil {
		return nil, err
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "end") {
		return nil, fmt.Errorf("truncated fs listing")
	}
	v := &fsView{files: map[string]bool{}, mounts: map[string]bool{}, current: map[string]string{},
		data: map[string]bool{}, dataCurrent: map[string]string{}, units: map[string]string{},
		unitFiles: map[string]bool{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "file":
			v.files[f[1]] = true
		case f[0] == "mount" && len(f) == 3:
			v.mounts[f[1]+" "+f[2]] = true
		case f[0] == "current" && len(f) == 3:
			v.current[f[1]] = f[2]
		case f[0] == "data" && len(f) == 3:
			v.data[f[1]+" "+f[2]] = true
		case f[0] == "datacurrent" && len(f) == 3:
			v.dataCurrent[f[1]] = f[2]
		case f[0] == "unitfile":
			if k, ok := unitKey(f[1]); ok {
				v.unitFiles[k] = true
			}
		}
	}
	out, err = agentGet(t.agent, "/units", 40*time.Second)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		// unit load active sub; a unit whose file is gone is only systemd's memory
		if len(f) < 4 || !strings.HasSuffix(f[0], ".mount") || f[1] != "loaded" {
			continue
		}
		// snap-<escaped name>-<rev>.mount, with "-" in names escaped as \x2d
		if k, ok := unitKey(f[0]); ok {
			v.units[k] = f[2] + "/" + f[3]
		}
	}
	return v, nil
}

// checkTarget, with snapd quiescent, checks snapd's state against the model
// and the filesystem. In an eventually, failures to observe are failures.
func (w *workload) checkTarget(t *target, final bool) bool {
	// Holding the gate stops new submissions; wait for in-flight ones.
	t.gate.Lock()
	defer t.gate.Unlock()
	drained := waitFor(45*time.Second, func() error {
		for _, m := range t.snaps {
			m.mu.Lock()
			p := m.pending
			m.mu.Unlock()
			if p > 0 {
				return fmt.Errorf("pending")
			}
		}
		t.sysMu.Lock()
		defer t.sysMu.Unlock()
		if t.sysPending > 0 {
			return fmt.Errorf("pending")
		}
		return nil
	}, 200*time.Millisecond)
	if drained != nil {
		return false
	}
	chgs, err := t.inProgress("")
	if err != nil || len(chgs) > 0 {
		return false
	}
	states := map[string]snapState{}
	names := append([]string{"snapd"}, allSnapNames...)
	for _, name := range names {
		st, err := t.readState(name)
		if err != nil {
			return false
		}
		states[name] = st
	}
	fs, err := t.readFS()
	if err != nil {
		t.logf("check: read fs: %v", err)
		return false
	}
	if chgs, err := t.inProgress(""); err != nil || len(chgs) > 0 {
		return false
	}
	w.count("check")

	for _, name := range allSnapNames {
		m := t.snaps[name]
		m.mu.Lock()
		if m.known && !m.tainted {
			assert.Always(m.state.equal(states[name]),
				"a snap only changes through changes that complete", map[string]any{
					"host": t.host, "snap": name, "model": m.state.String(), "snapd": states[name].String()})
		}
		m.state, m.known, m.tainted = states[name], true, false
		m.mu.Unlock()
	}

	var problems []string
	want := map[string]bool{}
	for _, name := range names {
		st := states[name]
		for _, r := range st.Revs {
			key := name + " " + r
			want[key] = true
			if !fs.files[name+"_"+r+".snap"] {
				problems = append(problems, "missing blob "+name+"_"+r+".snap")
			}
			if !fs.mounts[key] {
				problems = append(problems, "missing mount "+key)
			}
			if u, ok := fs.units[key]; !ok || u != "active/mounted" {
				problems = append(problems, fmt.Sprintf("mount unit %s is %q", key, u))
			}
		}
		if st.Installed && st.Active && fs.current[name] != st.Current {
			problems = append(problems, fmt.Sprintf("/snap/%s/current -> %q, want %s", name, fs.current[name], st.Current))
		}
		if st.Installed && !st.Active && fs.current[name] != "" {
			problems = append(problems, fmt.Sprintf("disabled %s has /snap/%s/current -> %s", name, name, fs.current[name]))
		}
		if !st.Installed && fs.current[name] != "" {
			problems = append(problems, fmt.Sprintf("removed %s has /snap/%s/current", name, name))
		}
	}
	for f := range fs.files {
		i := strings.LastIndex(strings.TrimSuffix(f, ".snap"), "_")
		if i < 0 || !want[f[:i]+" "+strings.TrimSuffix(f[i+1:], ".snap")] {
			problems = append(problems, "leaked blob "+f)
		}
	}
	for k := range fs.mounts {
		if !want[k] {
			problems = append(problems, "leaked mount "+k)
		}
	}
	for k := range fs.units {
		if !want[k] {
			problems = append(problems, "leaked mount unit "+k)
		}
	}
	for k := range fs.unitFiles {
		if !want[k] {
			problems = append(problems, "leaked mount unit file "+k)
		}
	}
	for k := range want {
		if !fs.unitFiles[k] {
			problems = append(problems, "missing mount unit file "+k)
		}
	}
	for k := range fs.data {
		// A refresh trashes the previous data as <rev>.old; the change's
		// cleanup removes it after the change is ready, so only count it once
		// everything has settled.
		if strings.HasSuffix(k, ".old") && !final {
			continue
		}
		if !want[k] && !(strings.HasSuffix(k, ".old") && want[strings.TrimSuffix(k, ".old")] && !final) {
			problems = append(problems, "leaked data dir /var/snap/"+strings.Replace(k, " ", "/", 1))
		}
	}
	sort.Strings(problems)
	details := map[string]any{"host": t.host, "problems": problems, "final": final}
	for name, st := range states {
		details["snapd:"+name] = st.String()
	}
	var leaks, wrong []string
	for _, p := range problems {
		if strings.HasPrefix(p, "leaked ") {
			leaks = append(leaks, p)
		} else {
			wrong = append(wrong, p)
		}
	}
	details["leaks"], details["wrong"] = leaks, wrong
	assert.Always(len(wrong) == 0, "with no change in progress, every revision snapd has is mounted and linked", details)
	assert.Always(len(leaks) == 0, "with no change in progress, nothing is left behind for revisions snapd does not have", details)
	if len(problems) > 0 {
		t.logf("check: problems: %v", problems)
	}
	return true
}

// ---- eventually ----

func (w *workload) eventually() {
	w.evMu.Lock() // never released: the driver exits below
	log.Printf("eventually: stopping faults")
	if p := os.Getenv("ANTITHESIS_STOP_FAULTS"); p != "" {
		if out, err := exec.Command(p, "3600").CombinedOutput(); err != nil {
			log.Printf("eventually: stop faults: %v: %s", err, out)
		}
	}
	for _, t := range w.targets {
		up := waitFor(10*time.Minute, func() error { _, err := t.cli.SysInfo(); return err })
		assert.Always(up == nil, "snapd answers requests once faults stop", map[string]any{"host": t.host, "error": fmt.Sprint(up)})
		var stuck []*client.Change
		settled := waitFor(10*time.Minute, func() error {
			chgs, err := t.inProgress("")
			if err != nil {
				return err
			}
			stuck = chgs
			if len(chgs) > 0 {
				return fmt.Errorf("%d changes in progress", len(chgs))
			}
			return nil
		})
		var desc []string
		for _, c := range stuck {
			desc = append(desc, fmt.Sprintf("%s %s %s", c.ID, c.Kind, c.Status))
		}
		assert.Always(settled == nil, "every change finishes once faults stop", map[string]any{
			"host": t.host, "error": fmt.Sprint(settled), "changes": desc})
	}
	// Let the trackers see the final change states.
	waitFor(2*time.Minute, func() error {
		w.inflightMu.Lock()
		defer w.inflightMu.Unlock()
		if len(w.inflight) > 0 {
			return fmt.Errorf("%d tracked", len(w.inflight))
		}
		return nil
	})
	log.Printf("eventually: assertions start")
	for _, t := range w.targets {
		ok := false
		waitFor(3*time.Minute, func() error {
			if ok = w.checkTarget(t, true); !ok {
				return fmt.Errorf("not quiescent")
			}
			return nil
		})
		assert.Always(ok, "snapd's state can be checked once faults stop", map[string]any{"host": t.host})
	}
	log.Printf("eventually: ending the timeline")
	os.Exit(0)
}

func waitFor(d time.Duration, f func() error, every ...time.Duration) error {
	step := time.Second
	if len(every) > 0 {
		step = every[0]
	}
	deadline := time.Now().Add(d)
	for {
		err := f()
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(step)
	}
}
