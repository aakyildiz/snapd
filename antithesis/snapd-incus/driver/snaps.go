package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

var snapSeq atomic.Int64

// snapSpec describes a test snap to build. Every value comes from hegel.
type snapSpec struct {
	Name    string
	Version string // default: a unique "1.<n>"
	Files   []int  // sizes in bytes
	Seed    uint64
	Plug    bool // declares a "network" plug, so auto-connect has work to do
	Corrupt bool // truncate the squashfs image, so a later task fails and undoes
}

// build packs the spec into a .snap and returns its path.
func (s snapSpec) build() (string, error) {
	seq := snapSeq.Add(1)
	dir, err := os.MkdirTemp("", "snap-"+s.Name)
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		return "", err
	}
	var y strings.Builder
	version := s.Version
	if version == "" {
		version = fmt.Sprintf("1.%d", seq)
	}
	fmt.Fprintf(&y, "name: %s\nversion: %q\nbase: none\nsummary: test snap\ndescription: test snap\n", s.Name, version)
	if s.Plug {
		y.WriteString("plugs:\n  network:\n")
	}
	if err := os.MkdirAll(filepath.Join(dir, "meta"), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "meta/snap.yaml"), []byte(y.String()), 0644); err != nil {
		return "", err
	}
	rng := rand.New(rand.NewPCG(s.Seed, uint64(seq)))
	for i, size := range s.Files {
		b := make([]byte, size)
		for j := range b {
			b[j] = byte(rng.Uint32())
		}
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("data-%d.bin", i)), b, 0644); err != nil {
			return "", err
		}
	}
	out := fmt.Sprintf("/tmp/%s_%d.snap", s.Name, seq)
	if err := mksquashfs(dir, out); err != nil {
		return "", err
	}
	if s.Corrupt {
		st, err := os.Stat(out)
		if err != nil {
			return "", err
		}
		// Keep the superblock so snapd accepts the upload; lose the tail.
		if err := os.Truncate(out, max(4096, st.Size()*2/3)); err != nil {
			return "", err
		}
	}
	return out, nil
}

func mksquashfs(dir, out string) error {
	os.Remove(out)
	cmd := exec.Command("mksquashfs", dir, out, "-noappend", "-comp", "gzip", "-all-root",
		"-no-progress", "-quiet", "-no-xattrs", "-mkfs-time", "0", "-all-time", "0", "-processors", "1")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mksquashfs: %v: %s", err, b)
	}
	return nil
}
