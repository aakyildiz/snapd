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

// Deterministic reproductions of undo-related findings from the Antithesis
// fault-injection runs (see antithesis/STATUS.md, findings 4, 5, 11 and 13).
// Each test asserts the behaviour snapd *should* have, so on code that still
// has the bug it fails and the failure message shows the bug.
//
// Run with:
//
//	go test -tags antithesisrepro ./overlord/snapstate -check.f 'AntithesisFinding' -check.v

package snapstate_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/snapshotstate"
	"github.com/snapcore/snapd/overlord/snapshotstate/backend"
	"github.com/snapcore/snapd/overlord/snapstate"
	snapbackend "github.com/snapcore/snapd/overlord/snapstate/backend"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/naming"
	"github.com/snapcore/snapd/store"
)

func antithesisOpsOf(ops fakeOps) []string {
	var out []string
	for _, op := range ops {
		out = append(out, op.op+" "+op.path)
	}
	return out
}

// Finding 13: a crash inside doLinkSnap after m.backend.LinkSnap but before
// the undo data ("old-channel", "old-current", ...) and the snap state are
// saved, followed by an abort, leaves the snap linked.
//
// Crash modelling: doLinkSnap holds the state lock from its start until it
// returns, and everything it records for undo (t.Set("old-*") and
// snapstate.Set) happens after backend.LinkSnap. A SIGKILL between the two
// therefore persists exactly the state from before the handler ran: the task
// is Doing (the runner marked it so before calling the handler), it has
// snap-setup but no old-* keys, and (first install) the snap is not in
// snapstate. On disk the side effect of backend.LinkSnap remains; with the
// fake backend we model it by calling the fake LinkSnap, which records a
// "link-snap" op. After the restart the change is aborted before the runner
// re-runs the task: Change.Abort turns Doing into Abort and the runner then
// runs the undo handler.
func (s *linkSnapSuite) TestAntithesisFinding13UndoLinkSnapAfterCrashBeforeUndoData(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	si := &snap.SideInfo{RealName: "foo", Revision: snap.R(33)}
	t := s.state.NewTask("link-snap", "test")
	t.Set("snap-setup", &snapstate.SnapSetup{
		SideInfo: si,
		Channel:  "beta",
	})
	chg := s.state.NewChange("install", "...")
	chg.AddTask(t)
	// state as persisted at the crash point
	t.SetStatus(state.DoingStatus)

	// the side effect that did reach the disk: /snap/foo/current -> 33
	info, err := s.fakeBackend.ReadInfo("foo", si)
	c.Assert(err, IsNil)
	c.Assert(s.fakeBackend.LinkSnap(info, nil, snapbackend.LinkContext{FirstInstall: true}, nil), IsNil)
	c.Assert(s.fakeBackend.ops, HasLen, 1)
	c.Assert(s.fakeBackend.ops[0].op, Equals, "link-snap")

	// sanity: no undo data was recorded and the snap is not in state
	c.Assert(t.Has("old-channel"), Equals, false)
	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "foo", &snapst), NotNil)

	// after restart, the user aborts the change
	chg.Abort()
	c.Assert(t.Status(), Equals, state.AbortStatus)

	s.state.Unlock()
	for i := 0; i < 6; i++ {
		s.se.Ensure()
		s.se.Wait()
	}
	s.state.Lock()

	c.Logf("task log: %q", t.Log())
	c.Logf("backend ops: %q", antithesisOpsOf(s.fakeBackend.ops))

	// correct behaviour: undo copes with the partial execution and unlinks
	c.Check(t.Status(), Equals, state.UndoneStatus,
		Commentf("undo of a link-snap interrupted before it saved its undo data failed; log: %q", t.Log()))
	var unlinked bool
	for _, op := range s.fakeBackend.ops[1:] {
		if op.op == "unlink-snap" && op.path == info.MountDir() {
			unlinked = true
		}
	}
	c.Check(unlinked, Equals, true,
		Commentf("snap was linked by the interrupted link-snap but never unlinked by its undo; ops: %q", antithesisOpsOf(s.fakeBackend.ops)))
}

// Finding 5: undoUnlinkSnap refuses to re-enable a snap whose common data dir
// is missing, taking that as evidence that this change's clear-snap ran,
// even when the dir was missing before the change started.
//
// No crash involved. An error-trigger placed right before clear-snap makes
// the change undo while clear-snap is still pending (it goes to Hold), which
// is the same as aborting the remove at that point.
func (s *snapmgrTestSuite) TestAntithesisFinding5UndoneRemoveLeavesSnapDisabledIfCommonMissing(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	si := snap.SideInfo{SnapID: "some-snap-id", RealName: "some-snap", Revision: snap.R(1)}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{&si}),
		Current:  si.Revision,
		SnapType: "app",
	})
	// /var/snap/some-snap/1 exists, /var/snap/some-snap/common does not
	// (deleted before this change, e.g. by an earlier aborted remove whose
	// clear-snap ran).
	c.Assert(os.MkdirAll(snap.DataDir("some-snap", si.Revision), 0755), IsNil)
	c.Assert(osutilDirMissing(snap.CommonDataDir("some-snap")), Equals, true)

	chg := s.state.NewChange("remove", "remove a snap")
	ts, err := snapstate.Remove(s.state, "some-snap", snap.R(0), nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	injectError(c, chg, "clear-snap", si.Revision)

	s.settle(c)

	c.Assert(chg.Status(), Equals, state.ErrorStatus)
	for _, t := range chg.Tasks() {
		switch t.Kind() {
		case "unlink-snap":
			c.Check(t.Status(), Equals, state.UndoneStatus)
			c.Logf("unlink-snap log: %q", t.Log())
		case "clear-snap", "discard-snap":
			// clear-snap never ran in this change
			c.Check(t.Status(), Equals, state.HoldStatus)
		}
	}
	// the revision data dir this change could have removed is still there
	c.Check(osutilDirMissing(snap.DataDir("some-snap", si.Revision)), Equals, false)

	var snapst snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "some-snap", &snapst), IsNil)
	c.Logf("backend ops: %q", antithesisOpsOf(s.fakeBackend.ops))
	// correct behaviour: the remove was fully undone before any of its data
	// was removed, so the snap must be active again
	c.Check(snapst.Active, Equals, true,
		Commentf("undone remove (clear-snap never ran) left the snap disabled"))
}

func osutilDirMissing(p string) bool {
	_, err := os.Stat(p)
	return errors.Is(err, os.ErrNotExist)
}

// failingReRefreshStore fails every SnapAction once fail is set, standing in
// for a store that times out during check-rerefresh.
type failingReRefreshStore struct {
	*fakeStore
	fail        bool
	calls       int
	failedCalls int
}

func (f *failingReRefreshStore) SnapAction(ctx context.Context, currentSnaps []*store.CurrentSnap, actions []*store.SnapAction, assertQuery store.AssertionQuery, user *auth.UserState, opts *store.RefreshOptions) ([]store.SnapActionResult, []store.AssertionResult, error) {
	f.calls++
	if f.fail {
		f.failedCalls++
		return nil, nil, errors.New(`Post "https://api.snapcraft.io/v2/snaps/refresh": context deadline exceeded`)
	}
	return f.fakeStore.SnapAction(ctx, currentSnaps, actions, assertQuery, user, opts)
}

// Finding 11: a multi-snap refresh whose final check-rerefresh store query
// fails is reported as Error although every snap was refreshed.
//
// No crash involved. The store answers the initial refresh query; every
// later SnapAction (i.e. the one from check-rerefresh) fails. The
// snapmgrTestSuite stubs reRefreshUpdateMany out, so the test puts the real
// updateManyFiltered back.
func (s *snapmgrTestSuite) TestAntithesisFinding11ReRefreshStoreErrorFailsSuccessfulRefresh(c *C) {
	s.state.Lock()
	defer s.state.Unlock()

	// the suite stubs out reRefreshUpdateMany; use the real one so that
	// check-rerefresh really queries the store
	defer snapstate.MockReRefreshUpdateMany(snapstate.UpdateManyFiltered)()

	fstore := &failingReRefreshStore{fakeStore: s.fakeStore}
	snapstate.ReplaceStore(s.state, fstore)

	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "some-snap", Revision: snap.R(5), SnapID: "some-snap-id"},
		}),
		Current:  snap.R(5),
		SnapType: "app",
	})
	snapstate.Set(s.state, "services-snap", &snapstate.SnapState{
		Active: true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "services-snap", Revision: snap.R(2), SnapID: "services-snap-id"},
		}),
		Current:  snap.R(2),
		SnapType: "app",
	})

	chg := s.state.NewChange("refresh", "refresh snaps")
	updated, tts, err := snapstate.UpdateMany(context.Background(), s.state, []naming.InstanceName{"some-snap", "services-snap"}, nil, 0, nil)
	c.Assert(err, IsNil)
	c.Assert(updated, HasLen, 2)
	var hasReRefresh bool
	for _, ts := range tts {
		chg.AddAll(ts)
		for _, t := range ts.Tasks() {
			if t.Kind() == "check-rerefresh" {
				hasReRefresh = true
			}
		}
	}
	c.Assert(hasReRefresh, Equals, true)

	// from now on the store times out
	fstore.fail = true

	s.settle(c)
	c.Logf("store SnapAction calls: %d, failed: %d", fstore.calls, fstore.failedCalls)

	for _, name := range []string{"some-snap", "services-snap"} {
		var snapst snapstate.SnapState
		c.Assert(snapstate.Get(s.state, name, &snapst), IsNil)
		c.Logf("%s: current %s, active %v", name, snapst.Current, snapst.Active)
		c.Check(snapst.Active, Equals, true)
	}
	var someSnap, servicesSnap snapstate.SnapState
	c.Assert(snapstate.Get(s.state, "some-snap", &someSnap), IsNil)
	c.Assert(snapstate.Get(s.state, "services-snap", &servicesSnap), IsNil)
	// both snaps were refreshed and stay refreshed
	c.Check(someSnap.Current, Not(Equals), snap.R(5))
	c.Check(servicesSnap.Current, Not(Equals), snap.R(2))

	for _, t := range chg.Tasks() {
		if t.Kind() == "check-rerefresh" {
			c.Logf("check-rerefresh status %s, log: %q", t.Status(), t.Log())
		}
	}
	// correct behaviour: the refresh took effect, so the change is not Error
	c.Check(chg.Status(), Equals, state.DoneStatus,
		Commentf("refresh of 2 snaps succeeded but change reports %s: %v", chg.Status(), chg.Err()))
}

// Finding 4: an aborted remove whose clear-snap already ran loses both the
// snap's data and its automatic snapshot, because the undo of save-snapshot
// is doForget.
//
// No crash involved. This uses the real snapshotstate handlers for
// save-snapshot (doSave, undo doForget) and the real AutomaticSnapshot hook;
// only the archive writer (backendSave) is replaced by one that writes a
// placeholder zip at the real snapshot path. The snapdBackend used by the
// remove tests really deletes the data dirs in clear-snap. An error-trigger
// placed right before discard-snap (after clear-snap is Done) stands in for
// the user's abort at that point: both drive the same undo of the Done tasks.
func (s *snapmgrTestSuite) TestAntithesisFinding4AbortedRemoveLosesDataAndSnapshot(c *C) {
	b := &snapdBackend{}
	snapstate.SetSnapManagerBackend(s.snapmgr, b)
	AddForeignTaskHandlers(s.o.TaskRunner(), b)

	var savedTo []string
	defer snapshotstate.MockBackendSave(func(_ context.Context, setID uint64, si *snap.Info, _ map[string]any, _ []string, _ *snap.SnapshotOptions, _ *dirs.SnapDirOptions) (*client.Snapshot, error) {
		sh := &client.Snapshot{SetID: setID, Snap: si.InstanceName().String(), Revision: si.Revision, Version: si.Version}
		fn := backend.Filename(sh)
		if err := os.MkdirAll(filepath.Dir(fn), 0700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(fn, []byte("placeholder snapshot"), 0600); err != nil {
			return nil, err
		}
		savedTo = append(savedTo, fn)
		return sh, nil
	})()

	s.state.Lock()
	defer s.state.Unlock()

	// real snapshot handlers and hooks (the suite restores the snapstate
	// hooks it mocked in its cleanup)
	snapshotstate.Manager(s.state, s.o.TaskRunner())
	snapstate.AutomaticSnapshot = snapshotstate.AutomaticSnapshot
	snapstate.AutomaticSnapshotExpiration = snapshotstate.AutomaticSnapshotExpiration
	tr := config.NewTransaction(s.state)
	c.Assert(tr.Set("core", "snapshots.automatic.retention", "24h"), IsNil)
	tr.Commit()

	si := snap.SideInfo{SnapID: "some-snap-id", RealName: "some-snap", Revision: snap.R(1)}
	snapstate.Set(s.state, "some-snap", &snapstate.SnapState{
		Active:   true,
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{&si}),
		Current:  si.Revision,
		SnapType: "app",
	})
	dataDir := snap.DataDir("some-snap", si.Revision)
	commonDir := snap.CommonDataDir("some-snap")
	c.Assert(os.MkdirAll(dataDir, 0755), IsNil)
	c.Assert(os.MkdirAll(commonDir, 0755), IsNil)
	c.Assert(os.MkdirAll(snap.CommonDataSaveDir("some-snap"), 0755), IsNil)

	chg := s.state.NewChange("remove", "remove a snap")
	ts, err := snapstate.Remove(s.state, "some-snap", snap.R(0), nil)
	c.Assert(err, IsNil)
	chg.AddAll(ts)

	var kinds []string
	for _, t := range chg.Tasks() {
		kinds = append(kinds, t.Kind())
	}
	c.Logf("remove tasks: %v", kinds)

	injectError(c, chg, "discard-snap", si.Revision)

	s.settle(c)

	c.Check(chg.Status(), Equals, state.ErrorStatus)
	for _, t := range chg.Tasks() {
		c.Logf("%-20s %s %q", t.Kind(), t.Status(), t.Log())
		switch t.Kind() {
		case "save-snapshot":
			c.Check(t.Status(), Equals, state.UndoneStatus)
		case "clear-snap":
			c.Check(t.Status(), Equals, state.DoneStatus)
		}
	}
	c.Assert(savedTo, HasLen, 1, Commentf("save-snapshot did not run"))

	dataLeft := !osutilDirMissing(dataDir) && !osutilDirMissing(commonDir)
	snapshotLeft := !osutilDirMissing(savedTo[0])
	c.Logf("data dirs present: %v, snapshot %s present: %v", dataLeft, savedTo[0], snapshotLeft)

	// correct behaviour: after an aborted remove, the data survives either in
	// place or in the automatic snapshot
	c.Check(dataLeft || snapshotLeft, Equals, true,
		Commentf("aborted remove deleted the snap's data (clear-snap, no undo) AND its automatic snapshot (save-snapshot undone by forget)"))
}
