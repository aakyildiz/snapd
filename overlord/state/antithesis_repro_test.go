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

package state_test

import (
	"fmt"

	. "gopkg.in/check.v1"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/overlord/state"
)

// Antithesis finding 1: a change aborted after only no-undo tasks (such as
// snapstate's "prerequisites") have run reports Done, although none of its
// real work was applied.
func (ts *taskRunnerSuite) TestAntithesisFinding1AbortedChangeReportsDone(c *C) {
	sb := &stateBackend{}
	st := state.New(sb)
	r := state.NewTaskRunner(st)
	defer r.Stop()

	var events []string
	record := func(ev string) state.HandlerFunc {
		return func(t *state.Task, tb *tomb.Tomb) error {
			st.Lock()
			defer st.Unlock()
			events = append(events, fmt.Sprintf("%s:%s", ev, t.Kind()))
			return nil
		}
	}
	// mirrors snapstate: prerequisites has no undo handler, the
	// following tasks do
	r.AddHandler("prerequisites", record("do"), nil)
	r.AddHandler("prepare-snap", record("do"), record("undo"))
	r.AddHandler("mount-snap", record("do"), record("undo"))

	st.Lock()
	chg := st.NewChange("install-snap", "install a snap")
	prereq := st.NewTask("prerequisites", "prereq")
	prepare := st.NewTask("prepare-snap", "prepare")
	prepare.WaitFor(prereq)
	mount := st.NewTask("mount-snap", "mount")
	mount.WaitFor(prepare)
	chg.AddAll(state.NewTaskSet(prereq, prepare, mount))
	st.Unlock()

	// run exactly one round: only prerequisites can run
	r.Ensure()
	r.Wait()

	st.Lock()
	c.Assert(prereq.Status(), Equals, state.DoneStatus)
	c.Assert(prepare.Status(), Equals, state.DoStatus)
	chg.Abort()
	st.Unlock()

	ensureChange(c, r, sb, chg)

	st.Lock()
	defer st.Unlock()
	c.Logf("events: %v", events)
	c.Logf("task statuses: prerequisites=%s prepare-snap=%s mount-snap=%s",
		prereq.Status(), prepare.Status(), mount.Status())
	c.Logf("change status=%s err=%v", chg.Status(), chg.Err())
	// nothing but prerequisites ran
	c.Check(events, DeepEquals, []string{"do:prerequisites"})
	// correct behaviour: an aborted change whose work was not applied must
	// not claim success
	c.Check(chg.Status(), Not(Equals), state.DoneStatus,
		Commentf("aborted change reports %s although only a no-undo task ran", chg.Status()))
}
