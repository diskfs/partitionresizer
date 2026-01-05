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

const (
	diskfullImg = "testdata/dist/diskfull.img"
)

func TestRun(t *testing.T) {
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
	for _, p := range table0.Partitions {
		if p.Name == "shrinker" {
			origShrinkSize = int64(p.GetSize())
			break
		}
	}
	if origShrinkSize == 0 {
		t.Fatal("could not find shrinker partition in original disk")
	}

	shrink := NewPartitionIdentifier(IdentifierByLabel, "shrinker")
	growList := []PartitionChange{
		NewPartitionChange(IdentifierByLabel, "parta", 2*GB),
		NewPartitionChange(IdentifierByLabel, "partb", 2*GB),
		NewPartitionChange(IdentifierByLabel, "ESP", 1*GB),
	}
	if err := Run(tmpFile, &shrink, growList, false, false); err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	f1, err := os.Open(tmpFile)
	if err != nil {
		t.Fatalf("failed to open disk image after Run: %v", err)
	}
	defer func() { _ = f1.Close() }()
	backend1 := file.New(f1, true)
	d1, err := diskfs.OpenBackend(backend1)
	if err != nil {
		t.Fatalf("failed to open disk after Run: %v", err)
	}
	tableRaw1, err := d1.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table after Run: %v", err)
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
		seen[name] = true
	}
	for _, n := range []string{"shrinker", "parta", "partb", "ESP"} {
		if !seen[n] {
			t.Errorf("missing active partition %q", n)
		}
	}
}
