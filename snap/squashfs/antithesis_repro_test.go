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

package squashfs_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/testutil"
)

// Antithesis finding 9 (squashfs part): Install over an existing, different
// target (a leaked, still-mounted blob of the same name and revision) cannot
// hard link, falls back to osutil.CopyFile(PreserveAll|Sync) = "cp -av", and
// rewrites the existing target in place.
func (s *SquashfsTestSuite) TestAntithesisFinding9InstallOverwritesExistingTargetInPlace(c *C) {
	// do not sync the whole host; cp is the real one, os.Link is not mocked
	mockSync := testutil.MockCommand(c, "sync", "")
	defer mockSync.Restore()

	oldSnap := makeSnapInDir(c, c.MkDir(), "name: test\nversion: 1", "old-old-old-old-old")
	newSnap := makeSnapInDir(c, c.MkDir(), "name: test\nversion: 2", "new")

	// the target already exists: the previously installed (and, in the
	// field, still mounted) blob for this name and revision. It is an
	// independent file with a single link, as in the field once the
	// sideload's upload file is deleted (with a second hard link, GNU
	// "cp -a" unlinks the target first and the overwrite is not in place).
	oldBytes, err := os.ReadFile(oldSnap.Path())
	c.Assert(err, IsNil)
	targetPath := filepath.Join(c.MkDir(), "test_x1.snap")
	c.Assert(os.WriteFile(targetPath, oldBytes, 0644), IsNil)
	newBytes, err := os.ReadFile(newSnap.Path())
	c.Assert(err, IsNil)
	c.Assert(bytes.Equal(oldBytes, newBytes), Equals, false)

	var before syscall.Stat_t
	c.Assert(syscall.Stat(targetPath, &before), IsNil)
	// stands in for squashfuse holding the mounted image open
	reader, err := os.Open(targetPath)
	c.Assert(err, IsNil)
	defer reader.Close()

	didNothing, installErr := newSnap.Install(targetPath, c.MkDir(), nil)
	c.Logf("Install: didNothing=%v err=%v", didNothing, installErr)

	var after syscall.Stat_t
	c.Assert(syscall.Stat(targetPath, &after), IsNil)
	seen, err := io.ReadAll(reader)
	c.Assert(err, IsNil)
	c.Logf("target inode before=%d after=%d, size before=%d after=%d", before.Ino, after.Ino, before.Size, after.Size)
	c.Logf("already-open reader sees old image: %v, new image: %v",
		bytes.Equal(seen, oldBytes), bytes.Equal(seen, newBytes))

	// correct behaviour: the existing target is either refused or replaced
	// atomically; a file that is open (mounted) is never rewritten under
	// its reader
	if installErr == nil {
		c.Check(after.Ino == before.Ino, Equals, false, Commentf("existing target rewritten in place (same inode %d)", after.Ino))
	}
	c.Check(bytes.Equal(seen, oldBytes), Equals, true, Commentf("open reader of the existing target saw its content change"))
}
