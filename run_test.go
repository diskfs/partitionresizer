package partitionresizer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestApplyResize drives a full grow+shrink-to-fit Apply on the diskfull fixture:
// grow parta/partb to 2 GB and ESP to 1 GB, shrinking "shrinker" only as much as
// needed to make room. It asserts the resulting sizes, that ESP keeps a FAT32
// filesystem, and that every partition keeps its original number (Apply always
// preserves numbers).
func TestApplyResize(t *testing.T) {
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "diskfull.img")
	if err := testCopyFile(diskfullImg, tmpFile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}

	f0, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("failed to open disk image: %v", err)
	}
	defer func() { _ = f0.Close() }()
	backend0 := file.New(f0, false)
	d0, err := diskfs.OpenBackend(backend0, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}
	tableRaw0, err := d0.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table: %v", err)
	}
	table0 := tableRaw0.(*gpt.Table)
	var origShrinkSize int64
	origNumber := map[string]int{}
	for _, p := range table0.Partitions {
		if p.Type == gpt.Unused {
			continue
		}
		origNumber[p.Name] = int(p.Index)
		if p.Name == "shrinker" {
			origShrinkSize = int64(p.GetSize())
		}
	}
	if origShrinkSize == 0 {
		t.Fatal("could not find shrinker partition in original disk")
	}
	_ = f0.Close()

	desired := []PartitionSpec{
		growByLabel("parta", 2*GB),
		growByLabel("partb", 2*GB),
		growByLabel("ESP", 1*GB),
	}
	if err := Apply(tmpFile, desired, shrinkToFitLabel("shrinker"), false, false); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	f1, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("failed to open disk image after Apply: %v", err)
	}
	defer func() { _ = f1.Close() }()
	backend1 := file.New(f1, true)
	d1, err := diskfs.OpenBackend(backend1)
	if err != nil {
		t.Fatalf("failed to open disk after Apply: %v", err)
	}
	tableRaw1, err := d1.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table after Apply: %v", err)
	}
	table1 := tableRaw1.(*gpt.Table)

	var active []*gpt.Partition
	for _, p := range table1.Partitions {
		if p.Type != gpt.Unused {
			active = append(active, p)
		}
	}
	if len(active) != 4 {
		t.Fatalf("expected 4 active partitions, got %d", len(active))
	}

	totalGrow := int64(2*GB + 2*GB + 1*GB)
	expectShrink := origShrinkSize - totalGrow

	seen := map[string]bool{}
	for _, p := range active {
		name := p.Name
		size := int64(p.GetSize())
		switch name {
		case "shrinker":
			if size != expectShrink {
				t.Errorf("shrinker partition size = %d, want %d", size, expectShrink)
			}
		case "parta", "partb":
			if size != int64(2*GB) {
				t.Errorf("%s partition size = %d, want %d", name, size, 2*GB)
			}
		case "ESP":
			if size != int64(1*GB) {
				t.Errorf("ESP partition size = %d, want %d", size, 1*GB)
			}
			fs, err := d1.GetFilesystem(int(p.Index))
			if err != nil {
				t.Errorf("unexpected error when getting FAT 32 filesystem: %v", err)
			}
			if fs.Type() != filesystem.TypeFat32 {
				t.Errorf("ESP filesystem type = %v, want FAT32", fs.Type())
			}
		default:
			t.Errorf("unexpected active partition %q", name)
		}
		// Apply preserves numbers: every partition keeps the number it had before.
		if int(p.Index) != origNumber[name] {
			t.Errorf("%s partition number = %d, want %d", name, p.Index, origNumber[name])
		}
		seen[name] = true
	}
	for _, n := range []string{"shrinker", "parta", "partb", "ESP"} {
		if !seen[n] {
			t.Errorf("missing active partition %q", n)
		}
	}
}
