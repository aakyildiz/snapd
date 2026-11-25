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

package osutil_test

import (
	"io"
	"os"
	"path/filepath"
	"syscall"

	. "gopkg.in/check.v1"

	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/testutil"
)

// Antithesis finding 9 (osutil part): with CopyFlagPreserveAll, CopyFile
// ignores the absence of CopyFlagOverwrite and runs "cp -av", which
// truncates and rewrites an existing destination in place (same inode).

// Without CopyFlagOverwrite, CopyFile refuses an existing destination
// (TestCpNoOverwrite); with CopyFlagPreserveAll it does not.
func (s *cpSuite) TestAntithesisFinding9PreserveAllIgnoresNoOverwrite(c *C) {
	c.Assert(os.WriteFile(s.f2, []byte("old"), 0644), IsNil)

	err := osutil.CopyFile(s.f1, s.f2, osutil.CopyFlagPreserveAll)
	c.Logf("CopyFile(PreserveAll) over an existing dst: err=%v", err)
	c.Check(err, NotNil, Commentf("CopyFile without CopyFlagOverwrite overwrote an existing destination"))
}

// The existing destination is rewritten in place: a reader that already has
// it open (as squashfuse has a mounted snap blob) sees the new bytes.
func (s *cpSuite) TestAntithesisFinding9PreserveAllRewritesDstInPlace(c *C) {
	// do not sync the whole host; cp itself is the real one
	mockSync := testutil.MockCommand(c, "sync", "")
	defer mockSync.Restore()

	src := filepath.Join(s.dir, "new.snap")
	dst := filepath.Join(s.dir, "target.snap")
	oldData := []byte("OLD-IMAGE-OLD-IMAGE-OLD-IMAGE")
	newData := []byte("new-image")
	c.Assert(os.WriteFile(dst, oldData, 0644), IsNil)
	c.Assert(os.WriteFile(src, newData, 0644), IsNil)

	var before syscall.Stat_t
	c.Assert(syscall.Stat(dst, &before), IsNil)

	reader, err := os.Open(dst)
	c.Assert(err, IsNil)
	defer reader.Close()

	copyErr := osutil.CopyFile(src, dst, osutil.CopyFlagPreserveAll|osutil.CopyFlagSync)
	c.Logf("CopyFile err=%v", copyErr)

	var after syscall.Stat_t
	c.Assert(syscall.Stat(dst, &after), IsNil)
	seen, err := io.ReadAll(reader)
	c.Assert(err, IsNil)
	c.Logf("dst inode before=%d after=%d, size before=%d after=%d", before.Ino, after.Ino, before.Size, after.Size)
	c.Logf("already-open reader now reads %q (originally %q)", seen, oldData)

	// correct behaviour: whatever happens to the path, a file that is
	// already open (e.g. a mounted snap blob) is never modified underneath
	// its reader; if the copy succeeded the path must point to a new inode
	// (atomic replace), if it refused the old file must be untouched
	if copyErr == nil {
		c.Check(after.Ino == before.Ino, Equals, false, Commentf("destination rewritten in place (same inode %d)", after.Ino))
	}
	c.Check(string(seen), Equals, string(oldData), Commentf("open reader saw the destination's content change"))
}
