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

package backend_test

import (
	"fmt"
	"path/filepath"
	"strings"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/progress"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/testutil"
)

// Antithesis finding 3: when a systemctl call fails while snapd cleans up a
// snap's files, RemoveSnapFiles returns before deleting the .snap blob (and,
// depending on which call fails, the mount unit file), and nothing retries.

func (s *setupSuite) antithesisFinding3Leftovers(c *C) {
	units, _ := filepath.Glob(filepath.Join(dirs.SnapServicesDir, "*.mount"))
	blobs, _ := filepath.Glob(filepath.Join(dirs.SnapBlobDir, "*.snap"))
	c.Logf("left behind: mount units=%v blobs=%v", units, blobs)
	c.Check(blobs, HasLen, 0, Commentf("snap blob leaked: %v", blobs))
	c.Check(units, HasLen, 0, Commentf("mount unit file leaked: %v", units))
}

// The run 6 scenario: the mount task's SetupSnap fails on daemon-reload
// (systemd going down), and its own cleanup hits the same failure.
func (s *setupSuite) TestAntithesisFinding3SetupSnapDaemonReloadFailsLeaksBlob(c *C) {
	snapPath := makeTestSnap(c, helloYaml1)
	si := snap.SideInfo{RealName: "hello", Revision: snap.R(14)}

	var calls []string
	r := systemd.MockSystemctl(func(cmd ...string) ([]byte, error) {
		calls = append(calls, strings.Join(cmd, " "))
		if cmd[0] == "daemon-reload" {
			return nil, fmt.Errorf("daemon-reload failed: transport endpoint is not connected")
		}
		return []byte("ActiveState=inactive\n"), nil
	})
	defer r()

	_, _, err := s.be.SetupSnap(snapPath, "hello", &si, mockDev, nil, progress.Null)
	c.Assert(err, ErrorMatches, ".*daemon-reload failed.*")
	c.Logf("systemctl calls: %q", calls)

	// correct behaviour: a failed setup cleans up everything it created
	s.antithesisFinding3Leftovers(c)
}

// Same, but every systemctl call fails (as when systemd is shutting down).
func (s *setupSuite) TestAntithesisFinding3SetupSnapAllSystemctlFailsLeaksBlobAndUnit(c *C) {
	snapPath := makeTestSnap(c, helloYaml1)
	si := snap.SideInfo{RealName: "hello", Revision: snap.R(14)}

	var calls []string
	r := systemd.MockSystemctl(func(cmd ...string) ([]byte, error) {
		calls = append(calls, strings.Join(cmd, " "))
		return nil, fmt.Errorf("systemctl %s failed: system is going down", cmd[0])
	})
	defer r()

	_, _, err := s.be.SetupSnap(snapPath, "hello", &si, mockDev, nil, progress.Null)
	c.Assert(err, NotNil)
	c.Logf("SetupSnap error: %v", err)
	c.Logf("systemctl calls: %q", calls)

	s.antithesisFinding3Leftovers(c)
}

// RemoveSnapFiles on its own (as used by discard-snap / undo of mount-snap)
// with a failing daemon-reload: it reports the error but must still delete
// the blob, since nothing will come back to delete it.
func (s *setupSuite) TestAntithesisFinding3RemoveSnapFilesDaemonReloadFailsLeaksBlob(c *C) {
	snapPath := makeTestSnap(c, helloYaml1)
	si := snap.SideInfo{RealName: "hello", Revision: snap.R(14)}
	_, _, err := s.be.SetupSnap(snapPath, "hello", &si, mockDev, nil, progress.Null)
	c.Assert(err, IsNil)
	c.Assert(filepath.Join(dirs.SnapBlobDir, "hello_14.snap"), testutil.FilePresent)

	r := systemd.MockSystemctl(func(cmd ...string) ([]byte, error) {
		if cmd[0] == "daemon-reload" {
			return nil, fmt.Errorf("daemon-reload failed")
		}
		return []byte("ActiveState=inactive\n"), nil
	})
	defer r()

	minInfo := snap.MinimalPlaceInfo("hello", snap.R(14))
	err = s.be.RemoveSnapFiles(minInfo, snap.TypeApp, nil, mockDev, progress.Null)
	c.Logf("RemoveSnapFiles error: %v", err)
	c.Check(err, NotNil)

	s.antithesisFinding3Leftovers(c)
}
