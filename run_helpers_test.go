package partitionresizer

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

const (
	testSectorSize = 512
)

// makeTable constructs a GPT table with partitions of given sizes (bytes).
// Assumes sectorSize of 512 bytes.
func makeTable(sizes ...int64) *gpt.Table {
	parts := make([]*gpt.Partition, len(sizes))
	var start int64 = 1
	for i, sz := range sizes {
		blocks := sz / testSectorSize
		parts[i] = &gpt.Partition{Index: i + 1, Start: uint64(start), Size: uint64(sz), End: uint64(start + blocks - 1)}
		start += blocks
	}
	return &gpt.Table{Partitions: parts}
}

// TestShrinkFilesystem verifies that an error from execResize2fs is wrapped correctly.
func TestShrinkFilesystem(t *testing.T) {
	t.Run("nonexistent", func(t *testing.T) {
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		execResize2fs = func(_ string, _ int64, _ bool) error {
			return fmt.Errorf("resize failure")
		}

		data := partitionData{name: "pY", number: 1, size: 5 * 1024 * 1024}
		totalGrow := int64(1 * 1024 * 1024)
		err := resizeFilesystem(filepath.Join("/dev", data.name), data, -1*totalGrow, true)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("error", func(t *testing.T) {
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "disk.img")
		if err := testCopyFile(imgFile, tmpFile); err != nil {
			t.Fatalf("failed to copy disk image: %v", err)
		}
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		resizeErr := fmt.Errorf("resize failure")
		execResize2fs = func(_ string, _ int64, _ bool) error {
			return resizeErr
		}

		data := partitionData{name: "pY", number: 1, size: 5 * 1024 * 1024}
		totalGrow := int64(1 * 1024 * 1024)
		err := resizeFilesystem(tmpFile, data, -1*totalGrow, true)
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if !errors.Is(err, resizeErr) {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("success", func(t *testing.T) {
		tmpDir := t.TempDir()
		tmpFile := filepath.Join(tmpDir, "disk.img")
		if err := testCopyFile(imgFile, tmpFile); err != nil {
			t.Fatalf("failed to copy disk image: %v", err)
		}
		var (
			calledDevice string
			calledMB     int64
		)
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		execResize2fs = func(dev string, mb int64, _ bool) error {
			calledDevice = dev
			calledMB = mb
			return nil
		}

		data := partitionData{
			name:   "pX",
			number: 3,
			size:   10 * 1024 * 1024, // 10MB
			start:  2048,
		}
		totalGrow := int64(2 * 1024 * 1024) // 2MB
		if err := resizeFilesystem(tmpFile, data, -1*totalGrow, true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		calledDeviceName := filepath.Base(calledDevice)
		if calledDeviceName != partTmpFilename {
			t.Errorf("device = %q, want %q", calledDevice, partTmpFilename)
		}
		wantMB := (data.size - totalGrow) / (1024 * 1024)
		if calledMB != wantMB {
			t.Errorf("newSizeMB = %d, want %d", calledMB, wantMB)
		}
	})
}

// makeDiskPartitionData produces partitionData entries matching table partitions.
func makeDiskPartitionData(names []string, table *gpt.Table) []partitionData {
	var data []partitionData
	for i, p := range table.Partitions {
		data = append(data, partitionData{
			name:   names[i],
			label:  names[i],
			size:   int64(p.Size),
			start:  int64(p.Start),
			end:    int64(p.Start + p.Size - 1),
			number: i + 1,
		})
	}
	return data
}

func TestPlanResizes(t *testing.T) {
	t.Run("open space", func(t *testing.T) {
		table := makeTable(5 * GB)
		diskData := makeDiskPartitionData([]string{"p1"}, table)
		d := &disk.Disk{Size: 10 * GB}
		resizes, err := planResizes(
			d,
			table,
			diskData,
			[]PartitionChange{NewPartitionChange(IdentifierByName, "p1", 3*GB)},
			nil,
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(resizes) != 1 {
			t.Fatalf("expected 1 resize, got %d", len(resizes))
		}
		if resizes[0].target.size != 3*GB {
			t.Errorf("target.size = %d, want %d", resizes[0].target.size, 3*GB)
		}
		// should check that it did not shrink
	})
	t.Run("with shrink", func(t *testing.T) {
		t.Run("no partition space available", func(t *testing.T) {
			table := makeTable(5 * GB)
			diskData := makeDiskPartitionData([]string{"p1"}, table)
			d := &disk.Disk{Size: 5 * GB}
			_, err := planResizes(
				d,
				table,
				diskData,
				[]PartitionChange{NewPartitionChange(IdentifierByName, "p1", 8*GB)},
				nil,
			)
			if err == nil {
				t.Fatal("expected error due to insufficient space and no shrinkPartition, got nil")
			}
		})
		t.Run("with partition space available", func(t *testing.T) {
			table := makeTable(1*GB, 20*GB)
			diskData := makeDiskPartitionData([]string{"p1", "p2"}, table)
			d := &disk.Disk{Size: 21 * GB}
			shrink := NewPartitionIdentifier(IdentifierByName, "p2")
			resizes, err := planResizes(
				d,
				table,
				diskData,
				[]PartitionChange{NewPartitionChange(IdentifierByName, "p1", 5*GB)},
				&shrink,
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			// partition entry size should now be 20GB-5GB = 15GB
			if len(resizes) != 2 {
				t.Fatalf("expected 2 resizes, got %d", len(resizes))
			}
			if resizes[0].target.size != 15*GB {
				t.Errorf("target %d size = %d, want %d", resizes[0].target.number, resizes[0].target.size, 15*GB)
			}
			if resizes[1].target.size != 5*GB {
				t.Errorf("target %d size = %d, want %d", resizes[1].target.number, resizes[1].target.size, 5*GB)
			}
		})
	})
}
