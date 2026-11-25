//go:build antithesisrepro

// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2026 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

// Reproduction tests for crash-window findings 6, 7, 8 and 10 reported by
// the Antithesis fault-injection runs (see antithesis/STATUS.md).
//
// Every test asserts the CORRECT behaviour, so on the code these findings
// were reported against it fails, and its failure output shows the bug.
//
// A "crash window" is snapd being SIGKILLed between two steps of a task
// handler. On restart the task is still Doing in the persisted state (state is
// only persisted when the state lock is released), so the handler runs again
// from the start. We model this deterministically in one of two ways:
//   - run the handler to completion, then put back the parts of the state that
//     the crash would not have persisted (the task's Done status, and anything
//     else written in the same locked section) and run the handler again;
//   - stage the persisted state exactly as it is at the crash point, by
//     hooking the fake backend at the moment the handler calls into it.
//
// Run with:
//   go test -tags antithesisrepro ./overlord/snapstate -check.f 'AntithesisFinding' -check.v

package snapstate_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "gopkg.in/check.v1"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaptest"
	"github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/testutil"
)

// antithesisRunUntil runs the state engine (and so the task runner) until
// cond holds, without requiring every change to become ready. The state must
// be locked by the caller.
func antithesisRunUntil(c *C, st *state.State, se *overlord.StateEngine, cond func() bool) {
	for i := 0; i < 50 && !cond(); i++ {
		st.Unlock()
		if err := se.Ensure(); err != nil {
			c.Logf("ensure: %v", err)
		}
		se.Wait()
		st.Lock()
	}
}

func antithesisSeqRevs(snapst *snapstate.SnapState) []string {
	var revs []string
	for _, si := range snapst.Sequence.Revisions {
		revs = append(revs, si.Snap.Revision.String())
	}
	return revs
}

func antithesisOpsSummary(ops fakeOps) string {
	var out []string
	for _, op := range ops {
		s := op.op
		if op.path != "" {
			s += " " + op.path
		}
		if op.name != "" {
			s += " name=" + op.name
		}
		out = append(out, s)
	}
	return strings.Join(out, "\n")
}

func antithesisCountOps(ops fakeOps, kind string) int {
	n := 0
	for _, op := range ops {
		if op.op == kind {
			n++
		}
	}
	return n
}

func antithesisTaskLog(chg *state.Change) string {
	var out []string
	for _, t := range chg.Tasks() {
		out = append(out, fmt.Sprintf("%s [%s]: %s", t.Kind(), t.Status(), strings.Join(t.Log(), " | ")))
	}
	return strings.Join(out, "\n")
}

// Finding 7: discard-snap is not idempotent. Re-running it for a revision
// that is no longer in the sequence drops the whole snap from state.
//
// Crash model: doDiscardSnap saves the sequence [x1] with Set and returns;
// the task runner sets Done afterwards, in a separate locked section, so a
// kill in between persists [x1] with discard-snap still Doing. We run the
// remove change under the task runner until discard-snap is Done (a trailing
// "gate" task keeps the change from becoming ready, as it would not be ready
// in the crashed snapd either), then put discard-snap back to Doing, as if its
// Done status had never been persisted, and let the task runner run it again.
func (s *snapmgrTestSuite) TestAntithesisFinding7DiscardSnapRerunForgetsActiveSnap(c *C) {
	si1 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x1")}
	si2 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x2")}

	gateOpen := false
	s.o.TaskRunner().AddHandler("antithesis-gate", func(t *state.Task, _ *tomb.Tomb) error {
		st := t.State()
		st.Lock()
		defer st.Unlock()
		if !gateOpen {
			return &state.Retry{After: time.Hour}
		}
		return nil
	}, nil)

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1, si2}),
		Current:  si1.Revision,
		SnapType: "app",
	})

	chg := s.state.NewChange("remove", "remove revision x2 of some-snap")
	ts, err := snapstate.Remove(s.state, "some-snap", si2.Revision, nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)
	discard := findKindInTaskSet(ts, "discard-snap")
	c.Assert(discard, NotNil)
	gate := s.state.NewTask("antithesis-gate", "keep the change unready")
	gate.WaitFor(discard)
	chg.AddTask(gate)

	antithesisRunUntil(c, s.state, s.se, func() bool { return discard.Status() == state.DoneStatus })
	c.Assert(discard.Status(), Equals, state.DoneStatus, Commentf("%v", discard.Log()))
	c.Assert(chg.IsReady(), Equals, false)

	// what the first run of discard-snap persisted
	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "some-snap", &snapst), IsNil)
	c.Assert(antithesisSeqRevs(&snapst), DeepEquals, []string{"x1"})
	c.Assert(snapst.Current, Equals, si1.Revision)
	c.Assert(snapst.Active, Equals, true)
	opsBefore := len(s.fakeBackend.ops)

	// --- crash + restart: discard-snap's Done status was never persisted ---
	discard.SetStatus(state.DoingStatus)

	antithesisRunUntil(c, s.state, s.se, func() bool { return discard.Status().Ready() })
	gateOpen = true
	s.settle(c)

	rerunOps := s.fakeBackend.ops[opsBefore:]
	c.Logf("ops of the discard-snap re-run:\n%s", antithesisOpsSummary(rerunOps))
	c.Logf("tasks:\n%s", antithesisTaskLog(chg))
	// sanity: the re-run really happened
	c.Assert(antithesisCountOps(rerunOps, "remove-snap-files") >= 1, Equals, true,
		Commentf("discard-snap did not run again"))
	c.Check(discard.Status(), Equals, state.DoneStatus)
	c.Logf("change status after the re-run: %s", chg.Status())

	// Correct behaviour: x1 is untouched, still installed and current.
	var after snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &after)
	c.Assert(err, IsNil, Commentf("BUG (finding 7): re-running discard-snap for x2, which was no longer in the sequence [x1], removed some-snap from state entirely although x1 is current and active; backend ops of the re-run:\n%s", antithesisOpsSummary(rerunOps)))
	c.Check(antithesisSeqRevs(&after), DeepEquals, []string{"x1"})
	c.Check(after.Current, Equals, si1.Revision)
	c.Check(after.Active, Equals, true)
}

// Finding 6, first half: doDiscardSnap deletes the revision's files before it
// saves the shorter sequence, with the state lock released in between. A
// crash in that window persists a state that still lists a revision whose
// blob, mount unit and mount are gone.
//
// Crash model: we hook the fake backend's RemoveSnapFiles. At that moment
// the state lock is released (doDiscardSnap unlocks around the call), so the
// state as seen by the hook is exactly what a checkpoint at that instant, and
// hence a kill right after the files are deleted, would leave on disk.
func (s *snapmgrTestSuite) TestAntithesisFinding6DiscardSnapRemovesFilesBeforeSavingSequence(c *C) {
	si1 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x1")}
	si2 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x2")}

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1, si2}),
		Current:  si1.Revision,
		SnapType: "app",
	})

	var seqAtRemoval []string
	hookRan := false
	s.fakeBackend.maybeInjectErr = func(op *fakeOp) error {
		if op.op != "remove-snap-files" {
			return nil
		}
		hookRan = true
		// The state is unlocked here; this is what would be persisted
		// if snapd were killed right after the files were deleted.
		s.state.Lock()
		defer s.state.Unlock()
		var snapst snapstate.SnapState
		if err := snapstate.Get(s.state, "some-snap", &snapst); err == nil {
			seqAtRemoval = antithesisSeqRevs(&snapst)
		}
		return nil
	}

	chg := s.state.NewChange("remove", "remove revision x2 of some-snap")
	ts, err := snapstate.Remove(s.state, "some-snap", si2.Revision, nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)
	c.Assert(chg.Err(), IsNil)
	c.Assert(hookRan, Equals, true)

	// Correct behaviour: by the time x2's files are deleted, the state
	// no longer lists x2 (save the shorter sequence first).
	c.Check(seqAtRemoval, DeepEquals, []string{"x1"},
		Commentf("BUG (finding 6): when RemoveSnapFiles deleted x2's files, the state (unlocked, so a crash would persist it) still listed sequence %v", seqAtRemoval))
}

// Finding 6, second half: after the crash above, the restarted snapd has
// discard-snap in Doing and x2 still in the sequence (its files are gone).
// If the change is aborted before the task runner re-runs it, the task
// runner puts the in-flight task with no undo handler on Hold, and it never
// runs again.
//
// Crash model: we stage the persisted state at the crash point directly: the
// sequence still [x1, x2], every task of the remove change before
// discard-snap Done, and discard-snap Doing (it was running when snapd was
// killed). Then we abort the change, as the driver did after the restart,
// and let the task runner settle.
func (s *snapmgrTestSuite) TestAntithesisFinding6AbortAfterCrashInDiscardSnapLeavesRevision(c *C) {
	si1 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x1")}
	si2 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x2")}

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1, si2}),
		Current:  si1.Revision,
		SnapType: "app",
	})

	chg := s.state.NewChange("remove", "remove revision x2 of some-snap")
	ts, err := snapstate.Remove(s.state, "some-snap", si2.Revision, nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	discard := findKindInTaskSet(ts, "discard-snap")
	c.Assert(discard, NotNil)
	// --- persisted state at the crash point ---
	for _, t := range ts.Tasks() {
		if t == discard {
			continue
		}
		// in a remove of a non-current revision all other tasks run
		// before discard-snap
		t.SetStatus(state.DoneStatus)
	}
	discard.SetStatus(state.DoingStatus)

	// --- restart, then the abort lands before the task runner re-runs it ---
	chg.Abort()

	s.settle(c)

	c.Logf("tasks:\n%s", antithesisTaskLog(chg))
	c.Logf("backend ops:\n%s", antithesisOpsSummary(s.fakeBackend.ops))

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "some-snap", &snapst), IsNil)
	seq := antithesisSeqRevs(&snapst)

	// Correct behaviour: the remove either finishes discarding x2, or the
	// change does not claim success. A Done change with x2 still listed
	// (and, on a real system, its files deleted) is the bug.
	x2Listed := false
	for _, r := range seq {
		if r == "x2" {
			x2Listed = true
		}
	}
	c.Check(x2Listed && chg.Status() == state.DoneStatus, Equals, false,
		Commentf("BUG (finding 6): change status %s, discard-snap status %s, sequence %v: snapd still lists x2 (whose files the interrupted discard-snap already deleted) and the change reports Done", chg.Status(), discard.Status(), seq))
	c.Check(discard.Status(), Not(Equals), state.HoldStatus,
		Commentf("BUG (finding 6): the interrupted no-undo discard-snap was put on Hold and will never run again"))
}

// Finding 8: a crash during a sideload's mount-snap, after the uploaded file
// was deleted but before Done was saved, leaks the mounted revision.
//
// Crash model: doMountSnap deletes snapsup.SnapPath and then, in one locked
// section, clears SnapPath in the task's snap-setup and sets Done. A kill
// between the delete and that unlock persists the task as Doing with the old
// snap-setup (SnapPath still set to the now-deleted upload). "snap-type" and
// "install-record" were set and unlocked earlier, so they are persisted. We
// run mount-snap once, then restore the old snap-setup, set the task back to
// Doing and run it again. A trailing "gate" task keeps the change unready
// throughout, as it is in the crashed snapd.
func (s *mountSnapSuite) TestAntithesisFinding8MountSnapRerunAfterUploadRemoved(c *C) {
	v1 := "name: foo\nversion: 1.0\n"
	testSnap := snaptest.MakeTestSnapWithFiles(c, v1, nil)

	gateOpen := false
	s.runner.AddHandler("antithesis-gate", func(t *state.Task, _ *tomb.Tomb) error {
		if !gateOpen {
			return &state.Retry{After: time.Hour}
		}
		return nil
	}, nil)

	s.state.Lock()
	origSnapsup := &snapstate.SnapSetup{
		SideInfo: &snap.SideInfo{
			RealName: "foo",
			Revision: snap.R("x1"),
		},
		SnapPath: testSnap,
		Flags:    snapstate.Flags{RemoveSnapPath: true},
	}
	t := s.state.NewTask("mount-snap", "test")
	t.Set("snap-setup", origSnapsup)
	chg := s.state.NewChange("install-snap", "sideload foo")
	chg.AddTask(t)
	gate := s.state.NewTask("antithesis-gate", "keep the change unready")
	gate.WaitFor(t)
	chg.AddTask(gate)

	antithesisRunUntil(c, s.state, s.se, func() bool { return t.Status().Ready() })
	c.Assert(t.Status(), Equals, state.DoneStatus, Commentf("%v", t.Log()))
	c.Assert(osutil.FileExists(testSnap), Equals, false)
	c.Assert(antithesisCountOps(s.fakeBackend.ops, "setup-snap"), Equals, 1)
	opsBefore := len(s.fakeBackend.ops)

	// --- crash + restart: SnapPath="" and Done were never persisted ---
	t.Set("snap-setup", origSnapsup)
	t.SetStatus(state.DoingStatus)

	antithesisRunUntil(c, s.state, s.se, func() bool { return t.Status().Ready() })
	defer s.state.Unlock()

	rerunOps := s.fakeBackend.ops[opsBefore:]
	c.Logf("ops of the mount-snap re-run:\n%s", antithesisOpsSummary(rerunOps))
	c.Logf("task log: %v", t.Log())

	// Correct behaviour: the re-run either succeeds (the setup it did
	// before the crash is still there), or it fails but undoes that setup.
	// The task runner never runs the undo of a task that ends in Error, so
	// the handler itself must clean up.
	status := t.Status()
	setupPath := ""
	for _, op := range s.fakeBackend.ops {
		if op.op == "setup-snap" {
			setupPath = op.path
		}
	}
	cleanedUp := antithesisCountOps(rerunOps, "undo-setup-snap") > 0 || antithesisCountOps(rerunOps, "remove-snap-files") > 0
	c.Check(status == state.DoneStatus || cleanedUp, Equals, true,
		Commentf("BUG (finding 8): re-run of mount-snap ended %s with log %q; backend ops of the re-run: %q; the blob/unit/mount from the first run (setup-snap %s) are never undone",
			status, t.Log(), antithesisOpsSummary(rerunOps), setupPath))
}

// Finding 10, part A: after a restart, ensureMountsUpdatedAfterSeed writes a
// mount unit for the current revision of every snap in state, including one
// that an in-progress remove has already deleted (blob, unit and mount gone),
// and keeps retrying on every Ensure while it fails.
//
// Crash model: we stage the persisted state of finding 10's history: snapd
// was killed inside discard-snap for the last revision of some-snap, after
// RemoveSnapFiles (so no blob and no mount unit exist) and before the state
// was saved. So some-snap is still in state (inactive, Current x1) and its
// remove change has discard-snap Doing. ensuredMountsUpdated is false as after
// every restart. systemctl restart of the unit fails, as it does for real when
// the blob is gone.
func (s *snapmgrTestSuite) TestAntithesisFinding10EnsureRecreatesMountUnitOfSnapBeingRemoved(c *C) {
	si1 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x1")}

	// snap.yaml and the blob are gone: use the real reader
	s.AddCleanup(snapstate.MockSnapReadInfo(snap.ReadInfo))
	s.AddCleanup(snapstate.MockEnsuredMountsUpdated(s.snapmgr, false))

	mountDir := snap.MinimalPlaceInfo("some-snap", si1.Revision).MountDir()
	unitFile := systemd.MountUnitPath(dirs.StripRootDir(mountDir))
	unitName := filepath.Base(unitFile)
	blob := snap.MinimalPlaceInfo("some-snap", si1.Revision).MountFile()

	restarts := 0
	s.AddCleanup(systemd.MockSystemctl(func(args ...string) ([]byte, error) {
		if len(args) == 1 && args[0] == "daemon-reload" {
			return nil, nil
		}
		if len(args) == 3 && args[0] == "--no-reload" && args[1] == "enable" {
			return nil, nil
		}
		if len(args) == 2 && args[0] == "restart" && args[1] == unitName {
			restarts++
			return nil, fmt.Errorf("Job for %s failed: mount of %s failed (no such file)", unitName, blob)
		}
		if len(args) == 2 && args[0] == "restart" {
			// units of other snaps in the suite's state (core)
			return nil, nil
		}
		c.Logf("unexpected systemctl %v", args)
		return nil, errors.New("unexpected systemctl call")
	}))

	s.state.Lock()
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   false, // unlink-snap already ran
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
		SnapType: "app",
	})
	chg := s.state.NewChange("remove", "remove some-snap")
	ts, err := snapstate.Remove(s.state, "some-snap", snap.R(0), nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)
	discard := findKindInTaskSet(ts, "discard-snap")
	c.Assert(discard, NotNil)
	for _, t := range ts.Tasks() {
		if t != discard {
			t.SetStatus(state.DoneStatus)
		}
	}
	discard.SetStatus(state.DoingStatus)
	s.state.Unlock()

	c.Assert(osutil.FileExists(blob), Equals, false)
	c.Assert(osutil.FileExists(unitFile), Equals, false)

	ensureErr := s.snapmgr.Ensure()
	c.Logf("first Ensure: %v", ensureErr)

	// Correct behaviour: no mount unit for a revision whose remove is in
	// progress and whose blob does not exist.
	c.Check(osutil.FileExists(unitFile), Equals, false,
		Commentf("BUG (finding 10): Ensure wrote %s for some-snap x1, which a remove (discard-snap Doing) is deleting and whose blob %s does not exist (systemctl restart attempts: %d, Ensure error: %v)", unitFile, blob, restarts, ensureErr))

	// The discard-snap re-run deletes the unit again (as the real
	// RemoveSnapFiles would); the next Ensure recreates it, because the
	// failed attempt left ensuredMountsUpdated false.
	os.Remove(unitFile)
	ensureErr = s.snapmgr.Ensure()
	c.Logf("second Ensure: %v", ensureErr)
	c.Check(osutil.FileExists(unitFile), Equals, false,
		Commentf("BUG (finding 10): the next Ensure recreated %s again (retries until it succeeds; restart attempts so far: %d)", unitFile, restarts))
}

// Finding 10, part B: the full leak. The ensure runs while discard-snap has
// the state unlocked around RemoveSnapFiles; discard-snap then drops the snap
// from state, and the recreated unit is left behind for a snap snapd no
// longer knows.
//
// Crash model: the remove runs normally under the task runner. The hook on
// the fake RemoveSnapFiles first deletes the unit file (what the real
// RemoveSnapFiles does), then runs the SnapManager's Ensure, as the
// concurrent ensure loop of a freshly restarted snapd (ensuredMountsUpdated
// false, because its earlier attempts failed on the missing blob) can do
// at that moment.
func (s *snapmgrTestSuite) TestAntithesisFinding10MountUnitLeakedAfterRemove(c *C) {
	si1 := &snap.SideInfo{RealName: "some-snap", Revision: snap.R("x1")}

	mountDir := snap.MinimalPlaceInfo("some-snap", si1.Revision).MountDir()
	unitFile := systemd.MountUnitPath(dirs.StripRootDir(mountDir))

	s.state.Lock()
	defer s.state.Unlock()

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{si1}),
		Current:  si1.Revision,
		SnapType: "app",
	})

	var hookEnsureErr error
	hookRan := false
	s.fakeBackend.maybeInjectErr = func(op *fakeOp) error {
		if op.op != "remove-snap-files" || hookRan {
			return nil
		}
		hookRan = true
		// what the real RemoveSnapFiles does to the unit
		os.Remove(unitFile)
		// concurrent ensure of a restarted snapd
		restore := snapstate.MockEnsuredMountsUpdated(s.snapmgr, false)
		hookEnsureErr = s.snapmgr.Ensure()
		_ = restore
		return nil
	}

	chg := s.state.NewChange("remove", "remove some-snap")
	ts, err := snapstate.Remove(s.state, "some-snap", snap.R(0), nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	s.settle(c)
	c.Logf("tasks:\n%s", antithesisTaskLog(chg))
	c.Assert(hookRan, Equals, true)
	c.Assert(chg.Status(), Equals, state.DoneStatus)
	c.Logf("Ensure inside the discard-snap window: %v", hookEnsureErr)

	var snapst snapstate.SnapState
	err = snapstate.Get(s.state, "some-snap", &snapst)
	c.Assert(err, testutil.ErrorIs, state.ErrNoState)

	// Correct behaviour: no mount unit is left for a snap that is not in
	// state.
	c.Check(osutil.FileExists(unitFile), Equals, false,
		Commentf("BUG (finding 10): some-snap was removed from state but its mount unit %s was recreated by Ensure during discard-snap and leaked", unitFile))
}
