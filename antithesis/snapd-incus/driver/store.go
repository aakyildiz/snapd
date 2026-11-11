package main

// The fake store side of the workload: the driver publishes revisions of the
// store snaps into the fake store's directory (a volume shared with the
// "store" container), signed with snapd's test keys, and records what each
// channel holds so it can judge installs, refreshes and channel switches.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/antithesishq/antithesis-sdk-go/assert"
	"hegel.dev/go/hegel"

	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/tests/lib/fakestore/refresh"
)

// storeSnapNames are the snaps the workload installs from the store. They're
// distinct from the sideloaded snapNames.
var storeSnapNames = []string{"st-a", "st-b"}

// risks are the channels the workload publishes to, all on the "latest" track.
var risks = []string{"stable", "candidate", "beta", "edge"}

func channelOf(risk string) string { return "latest/" + risk }

// maxPublishes bounds the store's size: the fake store hashes every blob on
// every request.
const maxPublishes = 40

type storeModel struct {
	dir     string
	staging string

	mu   sync.Mutex
	next map[string]int                     // next revision to publish
	chs  map[string]map[string]map[int]bool // snap -> risk -> revisions
}

func newStoreModel(dir string) *storeModel {
	return &storeModel{
		dir: dir, staging: filepath.Join(dir, ".staging"),
		next: map[string]int{}, chs: map[string]map[string]map[int]bool{},
	}
}

// latest is the highest revision published to snap's risk channel, 0 if none.
func (s *storeModel) latest(snap, risk string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := 0
	for r := range s.chs[snap][risk] {
		m = max(m, r)
	}
	return m
}

func (s *storeModel) inChannel(snap, risk string, rev int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.chs[snap][risk][rev]
}

// setup publishes each store snap's declaration, and revision 1 of each to
// every channel, so installs have something to get. Safe to repeat.
func (s *storeModel) setup() error {
	for _, d := range []string{s.dir, s.staging, filepath.Join(s.dir, "asserts"), filepath.Join(s.dir, "channels")} {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}
	for _, name := range storeSnapNames {
		s.mu.Lock()
		s.next[name] = 1
		s.mu.Unlock()
		if err := s.publish(name, snapSpec{Name: name, Seed: 1}, risks); err != nil {
			return err
		}
	}
	return nil
}

// publish builds a new revision of name and makes it visible in chans.
func (s *storeModel) publish(name string, spec snapSpec, chans []string) error {
	s.mu.Lock()
	rev := s.next[name]
	if rev > maxPublishes {
		s.mu.Unlock()
		return fmt.Errorf("store: %s reached %d revisions", name, maxPublishes)
	}
	s.next[name] = rev + 1
	s.mu.Unlock()

	spec.Name = name
	spec.Version = fmt.Sprintf("r%d", rev)
	path, err := spec.build()
	if err != nil {
		return err
	}
	defer os.Remove(path)
	snapID := name + "-id"

	// The declaration, once per snap.
	decl := filepath.Join(s.dir, "asserts", "16,"+snapID+".snap-declaration")
	if _, err := os.Stat(decl); err != nil {
		p, err := refresh.NewSnapDeclaration(s.staging, path, map[string]any{"snap-id": snapID, "snap-name": name})
		if err != nil {
			return fmt.Errorf("sign declaration: %w", err)
		}
		if err := os.Rename(p, decl); err != nil {
			return err
		}
	}
	p, err := refresh.NewSnapRevision(s.staging, path, map[string]any{"snap-id": snapID, "snap-revision": strconv.Itoa(rev)})
	if err != nil {
		return fmt.Errorf("sign revision: %w", err)
	}
	key := strings.TrimSuffix(filepath.Base(p), ".snap-revision")
	if err := os.Rename(p, filepath.Join(s.dir, "asserts", filepath.Base(p))); err != nil {
		return err
	}
	var lines []string
	for _, r := range chans {
		// snapd asks for "beta" on install and "latest/beta" on refresh;
		// the fake store matches channel names exactly.
		lines = append(lines, r, channelOf(r))
	}
	if err := s.writeAtomic(filepath.Join(s.dir, "channels", key), []byte(strings.Join(lines, "\n")+"\n")); err != nil {
		return err
	}
	// The blob last: the store only lists a revision once its blob exists.
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := s.writeAtomic(filepath.Join(s.dir, fmt.Sprintf("%s_%d.snap", name, rev)), b); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chs[name] == nil {
		s.chs[name] = map[string]map[int]bool{}
	}
	for _, r := range chans {
		if s.chs[name][r] == nil {
			s.chs[name][r] = map[int]bool{}
		}
		s.chs[name][r][rev] = true
	}
	return nil
}

func (s *storeModel) writeAtomic(dst string, b []byte) error {
	tmp, err := os.CreateTemp(s.staging, "w")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// String summarises the store for assertion details.
func (s *storeModel) describe(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var parts []string
	for _, r := range risks {
		var revs []int
		for rev := range s.chs[name][r] {
			revs = append(revs, rev)
		}
		sort.Ints(revs)
		parts = append(parts, fmt.Sprintf("%s=%v", r, revs))
	}
	return strings.Join(parts, " ")
}

// ---- rules ----

func (w *workload) drawStoreName(tc hegel.TestCase) string {
	return hegel.Draw(tc, hegel.SampledFrom(storeSnapNames))
}

func (w *workload) snapshotMax(names []string, risk func(string) string) map[string]int {
	m := map[string]int{}
	for _, n := range names {
		if r := risk(n); r != "" {
			m[n] = w.store.latest(n, r)
		}
	}
	return m
}

// RulePublish publishes a new revision of a store snap to some channels.
func (w *workload) RulePublish(tc hegel.TestCase) {
	if !hegel.Draw(tc, hegel.WeightedBooleans(0.25)) {
		return
	}
	name := w.drawStoreName(tc)
	var chans []string
	for _, r := range risks {
		if hegel.Draw(tc, hegel.WeightedBooleans(0.4)) {
			chans = append(chans, r)
		}
	}
	if len(chans) == 0 {
		chans = []string{hegel.Draw(tc, hegel.SampledFrom(risks))}
	}
	spec := snapSpec{Seed: hegel.Draw(tc, hegel.Integers(uint64(0), math64))}
	if hegel.Draw(tc, hegel.WeightedBooleans(0.2)) {
		spec.Files = []int{hegel.Draw(tc, hegel.Integers(1<<20, 4<<20))}
	}
	if err := w.store.publish(name, spec, chans); err != nil {
		if w.countN("publish-err")%20 == 1 {
			log.Printf("publish %s: %v", name, err)
		}
		return
	}
	w.count("publish")
}

// RuleStoreInstall installs a store snap from a channel.
func (w *workload) RuleStoreInstall(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	name := t.drawStoreNameWhere(tc, func(s snapState) bool { return !s.Installed })
	risk := hegel.Draw(tc, hegel.SampledFrom(risks))
	o := &op{kind: opStoreInstall, t: t, names: []string{name}, risk: risk,
		maxAt: map[string]int{name: w.store.latest(name, risk)}}
	w.submit(o, func() (string, error) {
		return t.cli.Install(name, nil, &client.SnapOptions{Channel: risk})
	})
}

// RuleRefresh refreshes a store snap, sometimes to another channel.
func (w *workload) RuleRefresh(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	name := t.drawStoreNameWhere(tc, func(s snapState) bool { return s.Installed })
	o := &op{kind: opRefresh, t: t, names: []string{name}}
	opts := &client.SnapOptions{}
	if hegel.Draw(tc, hegel.Booleans()) {
		o.risk = hegel.Draw(tc, hegel.SampledFrom(risks))
		opts.Channel = o.risk
	}
	o.maxAt = w.snapshotMax(o.names, func(n string) string { return o.effectiveRisk(t, n) })
	w.submit(o, func() (string, error) { return t.cli.Refresh(name, nil, opts) })
}

// RuleRefreshAll refreshes every snap, as `snap refresh` does.
func (w *workload) RuleRefreshAll(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	o := &op{kind: opRefreshAll, t: t, names: append([]string(nil), storeSnapNames...)}
	o.maxAt = w.snapshotMax(o.names, func(n string) string { return o.effectiveRisk(t, n) })
	w.submit(o, func() (string, error) { return t.cli.RefreshMany(nil, nil, &client.SnapOptions{}) })
}

// RuleSwitch switches the channel a store snap tracks.
func (w *workload) RuleSwitch(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	name := t.drawStoreNameWhere(tc, func(s snapState) bool { return s.Installed })
	risk := hegel.Draw(tc, hegel.SampledFrom(risks))
	o := &op{kind: opSwitch, t: t, names: []string{name}, risk: risk}
	w.submit(o, func() (string, error) { return t.cli.Switch(name, &client.SnapOptions{Channel: risk}) })
}

// RuleHold holds or releases a store snap's refreshes.
func (w *workload) RuleHold(tc hegel.TestCase) {
	t := w.drawTarget(tc)
	hold := hegel.Draw(tc, hegel.Booleans())
	name := t.drawStoreNameWhere(tc, func(s snapState) bool { return s.Installed && s.Held != hold })
	if hold {
		o := &op{kind: opHold, t: t, names: []string{name}}
		w.submit(o, func() (string, error) {
			return t.cli.HoldRefreshes(name, &client.SnapOptions{Time: "forever", HoldLevel: "general"})
		})
	} else {
		o := &op{kind: opUnhold, t: t, names: []string{name}}
		w.submit(o, func() (string, error) { return t.cli.UnholdRefreshes(name, nil) })
	}
}

func (t *target) drawStoreNameWhere(tc hegel.TestCase, fits func(snapState) bool) string {
	var ok []string
	for _, n := range storeSnapNames {
		m := t.snaps[n]
		m.mu.Lock()
		if fits(m.state) {
			ok = append(ok, n)
		}
		m.mu.Unlock()
	}
	if len(ok) == 0 || hegel.Draw(tc, hegel.WeightedBooleans(0.2)) {
		return hegel.Draw(tc, hegel.SampledFrom(storeSnapNames))
	}
	return hegel.Draw(tc, hegel.SampledFrom(ok))
}

// effectiveRisk is the channel a refresh of name will use: the requested one,
// or the one the model says it tracks.
func (o *op) effectiveRisk(t *target, name string) string {
	if o.risk != "" {
		return o.risk
	}
	m := t.snaps[name]
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.TrimPrefix(m.state.Tracking, "latest/")
}

// judgeStore judges completed store ops; it returns false for other ops.
func (w *workload) judgeStore(o *op, name string, before, after snapState, d map[string]any) bool {
	switch o.kind {
	case opStoreInstall, opRefresh, opRefreshAll, opSwitch, opHold, opUnhold:
	default:
		return false
	}
	d["store"] = w.store.describe(name)
	rev, _ := strconv.Atoi(after.Current)
	risk := o.risk
	if risk == "" {
		risk = strings.TrimPrefix(before.Tracking, "latest/")
	}
	// The store hands out the latest revision of the channel at the time of
	// the request: at least what it held when the driver sent it.
	fresh := after.Installed && rev >= o.maxAt[name] && w.store.inChannel(name, risk, rev)
	switch o.kind {
	case opStoreInstall:
		ok := fresh && after.Active && after.Tracking == channelOf(o.risk)
		assert.Always(ok, "a completed store install gets the channel's latest revision and tracks the channel", d)
	case opRefresh:
		ok := fresh && after.Tracking == channelOf(risk) && after.Held == before.Held
		assert.Always(ok, "a completed refresh gets the channel's latest revision and tracks the channel", d)
	case opRefreshAll:
		if before.Held {
			assert.Always(after.equal(before), "refreshing all snaps leaves a held snap alone", d)
		} else {
			ok := after.Tracking == before.Tracking && (!before.Installed || after.Installed)
			assert.Always(ok, "refreshing all snaps keeps each snap's channel", d)
		}
	case opSwitch:
		ok := after.Installed && after.Tracking == channelOf(o.risk) && after.Current == before.Current &&
			after.Active == before.Active && strings.Join(after.Revs, ",") == strings.Join(before.Revs, ",")
		assert.Always(ok, "a completed channel switch changes only the tracked channel", d)
	case opHold, opUnhold:
		want := before
		want.Held = o.kind == opHold
		assert.Always(after.equal(want), "a completed hold or unhold changes only whether refreshes are held", d)
	}
	assert.Sometimes(o.kind == opRefresh && after.Current != before.Current, "a refresh moves a snap to a new revision", d)
	assert.Sometimes(o.kind == opRefreshAll && before.Held, "a refresh of all snaps meets a held snap", d)
	return true
}
