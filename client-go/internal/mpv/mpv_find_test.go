package mpv

import (
	"os"
	"path/filepath"
	"testing"
)

// W2: the release bundle ships mpv in a subfolder next to the
// executable — the finder must find it there.
func TestFindMPVInBundleDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "mpv-x86_64")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(sub, "mpv")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findMPVIn(dir); got != bin {
		t.Fatalf("findMPVIn = %q, want the bundled %q", got, bin)
	}
	// the sibling wins over the subfolder
	sibling := filepath.Join(dir, "mpv")
	if err := os.WriteFile(sibling, []byte(""), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := findMPVIn(dir); got != sibling {
		t.Fatalf("findMPVIn = %q, want the sibling %q", got, sibling)
	}
}

// W2: no mpv anywhere -> the error says where it looked.
func TestFindMPVNowhere(t *testing.T) {
	if got := findMPVIn(t.TempDir()); got != "" {
		t.Fatalf("findMPVIn found %q in an empty folder", got)
	}
}
