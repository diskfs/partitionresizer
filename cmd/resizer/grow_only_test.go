package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestGrowOnlyDoesNotPanic builds the resizer and runs a grow-only invocation
// (no --shrink) against a minimal GPT image where the grow cannot fit. It must
// fail gracefully (insufficient space, exit 1), never panic (exit 2).
func TestGrowOnlyDoesNotPanic(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "resizer")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build resizer: %v\n%s", err, out)
	}
	img := makeMinimalGPTImage(t)

	cmd := exec.Command(bin, "--partition", "match=label:data,minsize=1G", img)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()

	if strings.Contains(stderr.String(), "panic:") {
		t.Fatalf("grow-only invocation panicked:\n%s", stderr.String())
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 2 {
		t.Fatalf("grow-only invocation crashed (exit 2 = panic):\n%s", stderr.String())
	}
	// a clean non-zero exit (e.g. insufficient space) is the expected behavior
	t.Logf("grow-only exited cleanly: err=%v\n%s", err, strings.TrimSpace(stderr.String()))
}

// makeMinimalGPTImage writes a small disk image with a valid GPT containing one
// "data" partition, enough for discovery to succeed and reach the matching path.
func makeMinimalGPTImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "disk.img")
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := f.Truncate(64 * 1024 * 1024); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_ = f.Close()

	f, err = os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, false), diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{Index: 1, Start: 2048, Size: 16 * 1024 * 1024, Type: gpt.LinuxFilesystem, Name: "data"},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("write partition table: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return p
}
