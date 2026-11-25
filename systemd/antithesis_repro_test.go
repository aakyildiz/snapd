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

package systemd_test

import (
	"errors"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/osutil"
	. "github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/testutil"
)

// Antithesis finding 3 (systemd part): RemoveMountUnitFile returns before
// os.Remove(unit) when a systemctl call that precedes it fails, leaving the
// unit file behind.

// "systemctl disable" fails (unmounted case): the unit file is left behind.
func (s *SystemdTestSuite) TestAntithesisFinding3RemoveMountUnitDisableFailsLeaksUnit(c *C) {
	rootDir := dirs.GlobalRootDir
	restore := osutil.MockMountInfo("")
	defer restore()

	mountDir := rootDir + "/snap/foo/42"
	mountUnit := makeMockMountUnit(c, mountDir)

	s.errors = []error{errors.New("disable failed: system is going down")}
	err := NewUnderRoot(rootDir, SystemMode, nil).RemoveMountUnitFile(mountDir)
	c.Logf("RemoveMountUnitFile error: %v; systemctl calls: %q", err, s.argses)
	c.Check(err, NotNil)
	// correct behaviour: the unit file is removed even if systemd could not
	// be told about it, so that it does not outlive the snap
	c.Check(mountUnit, testutil.FileAbsent)
}

// "systemctl stop" fails (mounted case): the unit file is left behind.
func (s *SystemdTestSuite) TestAntithesisFinding3RemoveMountUnitStopFailsLeaksUnit(c *C) {
	rootDir := dirs.GlobalRootDir
	mountDir := rootDir + "/snap/foo/42"
	mountUnit := makeMockMountUnit(c, mountDir)

	restore := MockOsutilIsMounted(func(path string) (bool, error) { return true, nil })
	defer restore()
	mockUmountCmd := testutil.MockCommand(c, "umount", "")
	defer mockUmountCmd.Restore()

	s.stopErrors = []error{errors.New("stop failed: system is going down")}
	err := NewUnderRoot(rootDir, SystemMode, nil).RemoveMountUnitFile(mountDir)
	c.Logf("RemoveMountUnitFile error: %v; stop calls: %q; other calls: %q", err, s.stopArgses, s.argses)
	c.Check(err, NotNil)
	c.Check(mountUnit, testutil.FileAbsent)
}

// Control: a failing daemon-reload happens after os.Remove(unit), so the unit
// file IS removed in that case (only the caller's later cleanup is skipped).
func (s *SystemdTestSuite) TestAntithesisFinding3RemoveMountUnitDaemonReloadFailsRemovesUnit(c *C) {
	rootDir := dirs.GlobalRootDir
	restore := osutil.MockMountInfo("")
	defer restore()

	mountDir := rootDir + "/snap/foo/42"
	mountUnit := makeMockMountUnit(c, mountDir)

	s.errors = []error{nil, errors.New("daemon-reload failed")}
	err := NewUnderRoot(rootDir, SystemMode, nil).RemoveMountUnitFile(mountDir)
	c.Logf("RemoveMountUnitFile error: %v; systemctl calls: %q", err, s.argses)
	c.Check(err, ErrorMatches, ".*daemon-reload failed.*")
	c.Check(mountUnit, testutil.FileAbsent)
}
