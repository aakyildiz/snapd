// -*- Mode: Go; indent-tabs-mode: t -*-
//go:build faultinject

/*
 * Copyright (C) 2021 Canonical Ltd
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

package osutil

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	injectSysroot           = "/"
	stderr        io.Writer = os.Stderr
	foreverLoop             = func() {
		for {
		}
	}
)

// faultPause is how long a "pause" fault holds the calling code.
var faultPause = 8 * time.Second

func crashedOnFault() bool {
	m, _ := filepath.Glob(filepath.Join(injectSysroot, "/var/lib/snapd/faults", "*:panic"))
	return len(m) > 0
}

func faultEnabled(tagKind string) bool {
	if !GetenvBool("SNAPPY_TESTING") {
		return false
	}
	for _, f := range strings.Split(os.Getenv("SNAPD_FAULT_INJECT"), ",") {
		if f == tagKind {
			return true
		}
	}
	return false
}

// MaybeInjectRetry returns true, once, when SNAPD_FAULT_INJECT has
// "<tag>:retry-after-crash" and snapd already crashed on a panic fault. A
// task handler that then returns a state.Retry stays Doing without running,
// as a task that was in flight when snapd was killed does after a restart
// until the task runner gets to it (Antithesis reproductions).
func MaybeInjectRetry(tag string) bool {
	tagKind := tag + ":retry-after-crash"
	if !faultEnabled(tagKind) || !crashedOnFault() {
		return false
	}
	stamp := filepath.Join(injectSysroot, "/var/lib/snapd/faults", tagKind)
	if FileExists(stamp) {
		return false
	}
	if err := AtomicWriteFile(stamp, nil, 0644, 0); err != nil {
		return false
	}
	fmt.Fprintf(stderr, "injecting retry for tag %q\n", tag)
	return true
}

func injectFault(tagKind string) (injected bool) {
	s := strings.Split(tagKind, ":")
	if len(s) != 2 {
		fmt.Fprintf(stderr, "incorrect fault tag: %q\n", tagKind)
		return false
	}
	tag := s[0]
	kind := s[1]
	stampFile := filepath.Join(injectSysroot, "/var/lib/snapd/faults", tagKind)
	if FileExists(stampFile) {
		// already injected once
		return false
	}

	if err := os.MkdirAll(filepath.Join(injectSysroot, "/var/lib/snapd/faults"), 0755); err != nil {
		fmt.Fprintf(stderr, "cannot create fault stamps directory: %v\n", err)
		return false
	}
	makeStamp := func() bool {
		if err := AtomicWriteFile(stampFile, nil, 0644, 0); err != nil {
			fmt.Fprintf(stderr, "cannot create stamp file for tag %q\n", tagKind)
			return false
		}
		return true
	}
	fmt.Fprintf(stderr, "injecting %q fault for tag %q\n", kind, tag)

	switch kind {
	case "pause":
		// Antithesis reproductions: hold the calling handler for a while,
		// so the driver can act (abort, block the store) at this point.
		if !makeStamp() {
			return false
		}
		time.Sleep(faultPause)
		return true
	case "pause-after-crash":
		// Like pause, but only once snapd has crashed on a panic fault:
		// the handler is running again after a restart.
		if !crashedOnFault() {
			return false
		}
		if !makeStamp() {
			return false
		}
		time.Sleep(faultPause)
		return true
	case "panic":
		if !makeStamp() {
			return false
		}
		panic(fmt.Sprintf("fault %q", tagKind))
	case "reboot":
		f, err := os.OpenFile(filepath.Join(injectSysroot, "/proc/sysrq-trigger"), os.O_WRONLY, 0)
		if err != nil {
			fmt.Fprintf(stderr, "cannot open: %v\n", err)
			return false
		}
		defer f.Close()
		if !makeStamp() {
			return false
		}
		if _, err := f.WriteString("b\n"); err != nil {
			fmt.Fprintf(stderr, "cannot request reboot: %v\n", err)
			return false
		}
		// we should be rebooting now
		foreverLoop()
	}
	// not reached
	return true
}

// MaybeInjectFault allows to inject faults into snapd through the environment
// settings. The faults to inject are listed in a SNAPD_FAULT_INJECT environment
// variable which has the format <tag>:<kind>[,<tag>:<kind>]. Where tag is a
// free form string that can be referenced directly in the code by placing
// MaybeInjectFault("<tag>"), while kind can be "panic" or "reboot". Panic
// causes snapd to panic, while reboot triggers an immediate reboot via
// sysrq-trigger. The fauls can only be injected iff SNAPPY_TESTING is true.
func MaybeInjectFault(tag string) {
	if !GetenvBool("SNAPPY_TESTING") {
		return
	}
	envTagKinds := os.Getenv("SNAPD_FAULT_INJECT")
	if envTagKinds == "" {
		return
	}
	if strings.ContainsAny(envTagKinds, " /") {
		fmt.Fprintf(stderr, "invalid fault tags %q\n", envTagKinds)
		return
	}
	faults := strings.Split(envTagKinds, ",")
	prefix := tag + ":"
	for _, envTagKind := range faults {
		if !strings.HasPrefix(envTagKind, prefix) {
			continue
		}
		injectFault(envTagKind)
	}
}
