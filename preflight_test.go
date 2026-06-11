package partitionresizer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/filesystem/squashfs"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// openFixtureExt4 copies the small fixture image (which has an ext4 partition)
// to a temp file, opens it read-write via a path so the backend has a non-empty
// Path(), and returns the disk plus the ext4 partition's data.
func openFixtureExt4(t *testing.T) (*disk.Disk, partitionData, func()) {
	t.Helper()
	tmpFile := filepath.Join(t.TempDir(), "disk.img")
	if err := testCopyFile(imgFile, tmpFile); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	backend, err := file.OpenFromPath(tmpFile, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		_ = backend.Close()
		t.Fatalf("open disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		_ = backend.Close()
		t.Fatalf("get partition table: %v", err)
	}
	table := tableRaw.(*gpt.Table)
	for _, p := range table.Partitions {
		fs, fsErr := d.GetFilesystem(p.Index)
		if fsErr == nil && fs.Type() == filesystem.TypeExt4 {
			pd := partitionData{
				number: p.Index,
				start:  int64(p.Start) * int64(table.LogicalSectorSize),
				size:   int64(p.Size),
				label:  p.Name,
			}
			return d, pd, func() { _ = backend.Close() }
		}
	}
	_ = backend.Close()
	t.Fatal("fixture has no ext4 partition; check buildimg.sh")
	return nil, partitionData{}, nil
}

func TestCheckSourceFilesystems(t *testing.T) {
	t.Run("ext4 source is checked with e2fsck", func(t *testing.T) {
		d, ext4, cleanup := openFixtureExt4(t)
		defer cleanup()

		origE, origF := execE2fsck, execFsckFat
		defer func() { execE2fsck, execFsckFat = origE, origF }()
		var e2fsckCalls, fatCalls int
		execE2fsck = func(string, bool) error { e2fsckCalls++; return nil }
		execFsckFat = func(string, bool) error { fatCalls++; return nil }

		resizes := []partitionResizeTarget{{original: ext4, target: partitionData{number: 99}}}
		if err := checkSourceFilesystems(d, resizes, false); err != nil {
			t.Fatalf("checkSourceFilesystems: %v", err)
		}
		if e2fsckCalls != 1 {
			t.Errorf("e2fsck call count = %d, want 1", e2fsckCalls)
		}
		if fatCalls != 0 {
			t.Errorf("fsck.fat call count = %d, want 0", fatCalls)
		}
	})

	t.Run("inconsistent ext4 source aborts the resize", func(t *testing.T) {
		d, ext4, cleanup := openFixtureExt4(t)
		defer cleanup()

		origE := execE2fsck
		defer func() { execE2fsck = origE }()
		sentinel := errors.New("e2fsck failed: exit status 4")
		execE2fsck = func(string, bool) error { return sentinel }

		resizes := []partitionResizeTarget{{original: ext4, target: partitionData{number: 99}}}
		err := checkSourceFilesystems(d, resizes, false)
		if err == nil {
			t.Fatal("expected error from an inconsistent source, got nil")
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("returned error does not wrap the e2fsck error: %v", err)
		}
	})

	t.Run("fat32 source is checked with fsck.fat", func(t *testing.T) {
		d, src, cleanup := newFat32SourceDisk(t)
		defer cleanup()

		origE, origF := execE2fsck, execFsckFat
		defer func() { execE2fsck, execFsckFat = origE, origF }()
		var e2fsckCalls, fatCalls int
		execE2fsck = func(string, bool) error { e2fsckCalls++; return nil }
		execFsckFat = func(string, bool) error { fatCalls++; return nil }

		resizes := []partitionResizeTarget{{original: src, target: partitionData{number: 99}}}
		if err := checkSourceFilesystems(d, resizes, false); err != nil {
			t.Fatalf("checkSourceFilesystems: %v", err)
		}
		if fatCalls != 1 {
			t.Errorf("fsck.fat call count = %d, want 1", fatCalls)
		}
		if e2fsckCalls != 0 {
			t.Errorf("e2fsck call count = %d, want 0", e2fsckCalls)
		}
	})

	t.Run("squashfs source is skipped, not errored", func(t *testing.T) {
		d, src, cleanup := newSquashfsSourceDisk(t)
		defer cleanup()

		origE, origF := execE2fsck, execFsckFat
		defer func() { execE2fsck, execFsckFat = origE, origF }()
		var e2fsckCalls, fatCalls int
		execE2fsck = func(string, bool) error { e2fsckCalls++; return nil }
		execFsckFat = func(string, bool) error { fatCalls++; return nil }

		resizes := []partitionResizeTarget{{original: src, target: partitionData{number: 99}}}
		if err := checkSourceFilesystems(d, resizes, false); err != nil {
			t.Fatalf("checkSourceFilesystems should skip squashfs, got error: %v", err)
		}
		if e2fsckCalls != 0 || fatCalls != 0 {
			t.Errorf("no checker should run for squashfs; e2fsck=%d fsck.fat=%d", e2fsckCalls, fatCalls)
		}
	})
}

// newFat32SourceDisk builds a disk image with a single FAT32 partition and
// returns the open disk plus that partition's data.
func newFat32SourceDisk(t *testing.T) (*disk.Disk, partitionData, func()) {
	t.Helper()
	const (
		diskSize    int64 = 64 * MB
		sectorSize        = 512
		sourceStart       = 2048
		sourceSize        = 16 * MB
	)
	diskPath := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(diskPath, nil, 0o644); err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := os.Truncate(diskPath, diskSize); err != nil {
		t.Fatalf("size disk: %v", err)
	}
	bk, err := file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	d, err := diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		_ = bk.Close()
		t.Fatalf("open disk: %v", err)
	}
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{Index: 1, Start: sourceStart, Size: sourceSize, Type: gpt.EFISystemPartition, Name: "source"},
		},
	}
	if err := d.Partition(table); err != nil {
		_ = bk.Close()
		t.Fatalf("write partition table: %v", err)
	}
	if _, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 1, FSType: filesystem.TypeFat32, VolumeLabel: "source"}); err != nil {
		_ = bk.Close()
		t.Fatalf("CreateFilesystem(fat32): %v", err)
	}
	pd := partitionData{number: 1, start: sourceStart * sectorSize, size: sourceSize, label: "source"}
	return d, pd, func() { _ = bk.Close() }
}

// newSquashfsSourceDisk builds a disk image with a single squashfs partition
// and returns the open disk plus that partition's data.
func newSquashfsSourceDisk(t *testing.T) (*disk.Disk, partitionData, func()) {
	t.Helper()
	const (
		diskSize    int64 = 64 * MB
		sectorSize        = 4096 // squashfs requires blocksize >= 4096
		sourceStart       = 256
		sourceSize        = 8 * MB
	)
	diskPath := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(diskPath, nil, 0o644); err != nil {
		t.Fatalf("create disk: %v", err)
	}
	if err := os.Truncate(diskPath, diskSize); err != nil {
		t.Fatalf("size disk: %v", err)
	}
	// First pass: write the partition table.
	bk, err := file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	d, err := diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		_ = bk.Close()
		t.Fatalf("open disk: %v", err)
	}
	table := &gpt.Table{
		LogicalSectorSize:  sectorSize,
		PhysicalSectorSize: sectorSize,
		Partitions: []*gpt.Partition{
			{Index: 1, Start: sourceStart, Size: sourceSize, Type: gpt.LinuxFilesystem, Name: "source"},
		},
	}
	if err := d.Partition(table); err != nil {
		_ = bk.Close()
		t.Fatalf("write partition table: %v", err)
	}
	// Build a squashfs in the source partition and finalize it.
	srcFS, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: 1, FSType: filesystem.TypeSquashfs})
	if err != nil {
		_ = bk.Close()
		t.Fatalf("CreateFilesystem(squashfs): %v", err)
	}
	sqs, ok := srcFS.(*squashfs.FileSystem)
	if !ok {
		_ = bk.Close()
		t.Fatalf("source not *squashfs.FileSystem")
	}
	if err := sqs.Finalize(squashfs.FinalizeOptions{NoCompressInodes: true, NoCompressData: true, NoCompressFragments: true}); err != nil {
		_ = bk.Close()
		t.Fatalf("squashfs Finalize: %v", err)
	}
	_ = bk.Close()

	// Reopen so the partition reads back as squashfs.
	bk, err = file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("reopen backend: %v", err)
	}
	d, err = diskfs.OpenBackend(bk, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		_ = bk.Close()
		t.Fatalf("reopen disk: %v", err)
	}
	if _, err := d.GetPartitionTable(); err != nil {
		_ = bk.Close()
		t.Fatalf("re-read partition table: %v", err)
	}
	pd := partitionData{number: 1, start: sourceStart * sectorSize, size: sourceSize, label: "source"}
	return d, pd, func() { _ = bk.Close() }
}
