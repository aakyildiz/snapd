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

package snapstate_test

import (
	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/backend"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/snap"
)

// Antithesis finding 1 through the real snapstate sideload task chain: abort
// the install after "prerequisites" (no undo handler) ran and before
// "prepare-snap" did. The change reports Done but the snap is not installed.
func (s *snapmgrTestSuite) TestAntithesisFinding1AbortedSideloadReportsDone(c *C) {
	snapstate.MockOpenSnapFile(backend.OpenSnapFile)

	s.state.Lock()
	defer s.state.Unlock()

	someSnap := makeTestSnap(c, "name: some-snap\nversion: 1.0")
	si := &snap.SideInfo{RealName: "some-snap", Revision: snap.R(-1)}
	ts, err := snapstate.InstallPath(s.state, si, someSnap, "", "", snapstate.Flags{}, nil)
	c.Assert(err, IsNil)
	chg := s.state.NewChange("install-snap", "install a local snap")
	chg.AddAll(ts)

	var prereq, prepare *state.Task
	for _, t := range chg.Tasks() {
		switch t.Kind() {
		case "prerequisites":
			// the first one; the sideload chain has a second one later
			if prereq == nil {
				prereq = t
			}
		case "prepare-snap":
			prepare = t
		}
	}
	c.Assert(prereq, NotNil)
	c.Assert(prepare, NotNil)

	// hold prepare-snap back so that the abort lands between
	// prerequisites and prepare-snap
	blockPrepare := true
	s.o.TaskRunner().AddBlocked(func(t *state.Task, running []*state.Task) bool {
		return blockPrepare && t.Kind() == "prepare-snap"
	})
	for i := 0; i < 10 && prereq.Status() != state.DoneStatus; i++ {
		s.state.Unlock()
		c.Assert(s.se.Ensure(), IsNil)
		s.se.Wait()
		s.state.Lock()
	}
	c.Assert(prereq.Status(), Equals, state.DoneStatus)
	c.Assert(prepare.Status(), Equals, state.DoStatus)

	chg.Abort()
	blockPrepare = false
	s.settle(c)

	for _, t := range chg.Tasks() {
		c.Logf("  %s (%s): %s", t.Kind(), t.Summary(), t.Status())
	}
	var snapst snapstate.SnapState
	stErr := snapstate.Get(s.state, "some-snap", &snapst)
	c.Logf("change status=%s err=%v; snapstate.Get(some-snap): %v", chg.Status(), chg.Err(), stErr)
	c.Check(snapst.IsInstalled(), Equals, false)
	// correct behaviour: the aborted install did not install anything, so
	// it must not report success
	c.Check(chg.Status(), Not(Equals), state.DoneStatus,
		Commentf("aborted sideload reports %s although the snap is not installed", chg.Status()))
}
