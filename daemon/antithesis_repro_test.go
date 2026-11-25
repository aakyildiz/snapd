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

package daemon_test

import (
	"bytes"
	"net/http"
	"os"

	"gopkg.in/check.v1"

	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/overlord/ifacestate/ifacerepo"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/snapstate/snapstatetest"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/snaptest"
)

// Antithesis finding 2: sideloading a new revision of a disabled snap fails
// with HTTP 500 "cannot update disabled snap", although this is a client
// error. The real snapstate.InstallPath / UpdateWithGoal are used here (not
// mocked), so the error is produced by snapstate exactly as in production.
func (s *sideloadSuite) antithesisFinding2Setup(c *check.C) []byte {
	d := s.daemonWithOverlordMockAndStore()
	s.markSeeded(d)

	st := d.Overlord().State()
	st.Lock()
	ifacerepo.Replace(st, interfaces.NewRepository())
	snapstate.Set(st, "foo", &snapstate.SnapState{
		Active: false, // disabled
		Sequence: snapstatetest.NewSequenceFromSnapSideInfos([]*snap.SideInfo{
			{RealName: "foo", Revision: snap.R(-1)},
		}),
		Current:  snap.R(-1),
		SnapType: "app",
	})
	st.Unlock()

	fooSnap := snaptest.MakeTestSnapWithFiles(c, "name: foo\nversion: 2", nil)
	fooSnapBytes, err := os.ReadFile(fooSnap)
	c.Assert(err, check.IsNil)
	return fooSnapBytes
}

func (s *sideloadSuite) antithesisFinding2Check(c *check.C, body *bytes.Buffer) {
	req, err := http.NewRequest("POST", "/v2/snaps", body)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	rspe := s.errorReq(c, req, nil, actionIsExpected)
	c.Logf("response: status=%d kind=%q message=%q", rspe.Status, rspe.Kind, rspe.Message)
	c.Check(rspe.Message, check.Matches, `.*cannot update disabled snap "foo".*`)
	// correct behaviour: the request is refused because of the client's
	// request and the snap's state, so it must be a 4xx, not a 500
	c.Check(rspe.Status >= 400 && rspe.Status < 500, check.Equals, true,
		check.Commentf("sideloading over a disabled snap returned HTTP %d", rspe.Status))
}

func (s *sideloadSuite) TestAntithesisFinding2SideloadDisabledSnapIs500(c *check.C) {
	fooSnapBytes := s.antithesisFinding2Setup(c)

	body := new(bytes.Buffer)
	body.WriteString("----hello--\r\n" +
		"Content-Disposition: form-data; name=\"dangerous\"\r\n\r\ntrue\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"foo.snap\"\r\n\r\n")
	body.Write(fooSnapBytes)
	body.WriteString("\r\n----hello--\r\n")

	s.antithesisFinding2Check(c, body)
}

func (s *sideloadSuite) TestAntithesisFinding2SideloadManyDisabledSnapIs500(c *check.C) {
	fooSnapBytes := s.antithesisFinding2Setup(c)
	// a second, not yet installed snap in the same request
	barSnapBytes, err := os.ReadFile(snaptest.MakeTestSnapWithFiles(c, "name: bar\nversion: 1", nil))
	c.Assert(err, check.IsNil)

	body := new(bytes.Buffer)
	body.WriteString("----hello--\r\n" +
		"Content-Disposition: form-data; name=\"dangerous\"\r\n\r\ntrue\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"foo.snap\"\r\n\r\n")
	body.Write(fooSnapBytes)
	body.WriteString("\r\n----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"bar.snap\"\r\n\r\n")
	body.Write(barSnapBytes)
	body.WriteString("\r\n----hello--\r\n")

	s.antithesisFinding2Check(c, body)
}
