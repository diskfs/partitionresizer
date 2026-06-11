package partitionresizer

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

func TestCreatePartitions(t *testing.T) {
	// create a disk with GPT partitions, call createPartitions, verify partitions created correctly
	workDir := t.TempDir()
	f, err := os.CreateTemp(workDir, "disk.img")
	if err != nil {
		t.Fatalf("failed to create temp disk image: %v", err)
	}
	if err := os.Truncate(f.Name(), 1*GB); err != nil {
		t.Fatalf("failed to truncate disk image: %v", err)
	}
	defer func() { _ = f.Close() }()

	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}
	// create a partition table, will use <512MB partitions, so we can
	// cleanly run the test targeting second half of the disk
	var offset uint64 = 2048
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{
				Index:      1,
				Start:      offset,
				Size:       36 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part1",
				Attributes: 0,
			},
			{
				Index:      2,
				Start:      offset + 36*MB,
				Size:       200 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part2",
				Attributes: 0,
			},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("failed to write partition table: %v", err)
	}
	// define resize targets
	resizes := []partitionResizeTarget{
		{
			original: partitionData{
				number: 1,
				start:  int64(offset),
				size:   int64(table.Partitions[0].Size),
				label:  table.Partitions[0].Name,
			},
			target: partitionData{
				number: 3,
				start:  int64(offset + 512*MB),
				size:   int64(table.Partitions[0].Size),
				label:  "part1_resized",
			},
		},
		{
			original: partitionData{
				number: 2,
				start:  int64(offset + 36*MB),
				size:   int64(table.Partitions[1].Size),
				label:  table.Partitions[1].Name,
			},
			target: partitionData{
				number: 4,
				start:  int64(offset + 36*MB + 512*MB),
				size:   int64(table.Partitions[1].Size),
				label:  "part2_resized",
			},
		},
	}
	// call createPartitions
	if err := createPartitions(d, resizes); err != nil {
		t.Fatalf("createPartitions failed: %v", err)
	}
	// verify partitions created
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table: %v", err)
	}
	newTable, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("unsupported partition table type, only GPT is supported")
	}
	expectedCount := len(table.Partitions) + len(resizes)
	if len(newTable.Partitions) != expectedCount {
		t.Fatalf("expected %d partitions after resize, got %d", expectedCount, len(newTable.Partitions))
	}
	sectorSize := newTable.LogicalSectorSize
	for _, r := range resizes {
		newPartRaw, err := d.GetPartition(r.target.number)
		if err != nil {
			t.Fatalf("failed to get new partition %d: %v", r.target.number, err)
		}
		newPart, ok := newPartRaw.(*gpt.Partition)
		if !ok {
			t.Fatalf("unsupported partition table type, only GPT is supported")
		}
		newPartStart := int64(newPart.Start) * int64(sectorSize)
		if newPartStart != r.target.start {
			t.Errorf("partition %d start mismatch: expected %d, got %d", r.target.number, r.target.start, newPartStart)
		}
		if newPart.Size != uint64(r.target.size) {
			t.Errorf("partition %d size mismatch: expected %d, got %d", r.target.number, r.target.size, newPart.Size)
		}
		if newPart.Name != getAlternateLabel(r.original.label) {
			t.Errorf("partition %d label mismatch: expected %s, got %s", r.target.number, getAlternateLabel(r.original.label), newPart.Name)
		}
	}
}

func TestRemovePartitions(t *testing.T) {
	// create a disk with GPT partitions, call removePartitions, verify partitions removed correctly
	workDir := t.TempDir()
	f, err := os.CreateTemp(workDir, "disk.img")
	if err != nil {
		t.Fatalf("failed to create temp disk image: %v", err)
	}
	if err := os.Truncate(f.Name(), 1*GB); err != nil {
		t.Fatalf("failed to truncate disk image: %v", err)
	}
	defer func() { _ = f.Close() }()

	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}
	var offset uint64 = 2048
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{
				Index:      1,
				Start:      offset,
				Size:       36 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part1",
				Attributes: 0,
			},
			{
				Index:      2,
				Start:      offset + 36*MB,
				Size:       200 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part2",
				Attributes: 0,
			},
			{
				Index:      3,
				Start:      offset + 36*MB + 200*MB,
				Size:       100 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part3",
				Attributes: 0,
			},
			{
				Index:      4,
				Start:      offset + 36*MB + 200*MB + 100*MB,
				Size:       100 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part4",
				Attributes: 0,
			},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("failed to write partition table: %v", err)
	}
	// define resize targets
	resizes := []partitionResizeTarget{
		{
			original: partitionData{
				number: 2,
				start:  int64(offset + 36*MB),
				size:   int64(table.Partitions[1].Size),
				label:  table.Partitions[1].Name,
			},
			target: partitionData{
				number: 5,
				start:  0,
				size:   0,
				label:  "",
			},
		},
		{
			original: partitionData{
				number: 3,
				start:  int64(offset + 36*MB + 200*MB),
				size:   int64(table.Partitions[2].Size),
				label:  table.Partitions[2].Name,
			},
			target: partitionData{
				number: 6,
				start:  0,
				size:   0,
				label:  "",
			},
		},
	}

	// call removePartitions
	if err := removePartitions(d, resizes); err != nil {
		t.Fatalf("removePartitions failed: %v", err)
	}
	// verify partitions removed
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table: %v", err)
	}
	newTable, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("unsupported partition table type, only GPT is supported")
	}
	expectedCount := len(table.Partitions) - len(resizes)
	if len(newTable.Partitions) != expectedCount {
		t.Fatalf("expected %d partitions after resize, got %d", expectedCount, len(newTable.Partitions))
	}
}

func TestRemoveAndRenumberPartitions(t *testing.T) {
	// Model the state after createPartitions+copy+swap for two grown partitions:
	// originals 2 and 3 (now carrying throwaway identities) plus their relocated
	// copies in slots 5 and 6 (carrying the real labels). removeAndRenumberPartitions
	// must drop the originals and move the relocated copies back to numbers 2 and 3.
	workDir := t.TempDir()
	f, err := os.CreateTemp(workDir, "disk.img")
	if err != nil {
		t.Fatalf("failed to create temp disk image: %v", err)
	}
	if err := os.Truncate(f.Name(), 1*GB); err != nil {
		t.Fatalf("failed to truncate disk image: %v", err)
	}
	defer func() { _ = f.Close() }()

	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}
	var offset uint64 = 2048
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{Index: 1, Start: offset, Size: 36 * MB, Type: gpt.LinuxFilesystem, Name: "part1"},
			// originals, to be dropped
			{Index: 2, Start: offset + 36*MB, Size: 100 * MB, Type: gpt.LinuxFilesystem, Name: "IMGA_old"},
			{Index: 3, Start: offset + 36*MB + 100*MB, Size: 50 * MB, Type: gpt.LinuxFilesystem, Name: "DATA_old"},
			{Index: 4, Start: offset + 36*MB + 100*MB + 50*MB, Size: 36 * MB, Type: gpt.LinuxFilesystem, Name: "part4"},
			// relocated copies carrying the real identities, to be renumbered to 2 and 3
			{Index: 5, Start: offset + 36*MB + 100*MB + 50*MB + 36*MB, Size: 300 * MB, Type: gpt.LinuxFilesystem, Name: "IMGA"},
			{Index: 6, Start: offset + 36*MB + 100*MB + 50*MB + 36*MB + 300*MB, Size: 200 * MB, Type: gpt.LinuxFilesystem, Name: "DATA"},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("failed to write partition table: %v", err)
	}
	// capture the relocated copies' on-disk starts so we can confirm renumbering keeps
	// the data in place and only changes the slot number
	wantStart := make(map[string]uint64)
	for _, p := range table.Partitions {
		wantStart[p.Name] = p.Start
	}

	resizes := []partitionResizeTarget{
		{
			original: partitionData{number: 2, label: "IMGA"},
			target:   partitionData{number: 5, label: "IMGA"},
		},
		{
			original: partitionData{number: 3, label: "DATA"},
			target:   partitionData{number: 6, label: "DATA"},
		},
	}

	if err := removeAndRenumberPartitions(d, resizes); err != nil {
		t.Fatalf("removeAndRenumberPartitions failed: %v", err)
	}

	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table: %v", err)
	}
	newTable, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("unsupported partition table type, only GPT is supported")
	}

	byIndex := make(map[int]*gpt.Partition)
	for _, p := range newTable.Partitions {
		if p.Type == gpt.Unused {
			continue
		}
		byIndex[p.Index] = p
	}

	// originals are gone, relocated copies now own numbers 2 and 3
	wantByNumber := map[int]string{1: "part1", 2: "IMGA", 3: "DATA", 4: "part4"}
	if len(byIndex) != len(wantByNumber) {
		t.Fatalf("expected %d partitions after renumber, got %d", len(wantByNumber), len(byIndex))
	}
	for number, name := range wantByNumber {
		p, ok := byIndex[number]
		if !ok {
			t.Fatalf("expected partition number %d (%s) to exist after renumber", number, name)
		}
		if p.Name != name {
			t.Errorf("partition %d: expected label %q, got %q", number, name, p.Name)
		}
	}
	// the renumbered partitions must keep the relocated copies' on-disk location
	if got := byIndex[2].Start; got != wantStart["IMGA"] {
		t.Errorf("renumbered IMGA start moved: expected %d, got %d", wantStart["IMGA"], got)
	}
	if got := byIndex[3].Start; got != wantStart["DATA"] {
		t.Errorf("renumbered DATA start moved: expected %d, got %d", wantStart["DATA"], got)
	}
	// the old high-numbered slots must be free
	if _, ok := byIndex[5]; ok {
		t.Errorf("slot 5 should have been vacated by renumbering")
	}
	if _, ok := byIndex[6]; ok {
		t.Errorf("slot 6 should have been vacated by renumbering")
	}
}

func TestCopyFilesystems(t *testing.T) {
	// create a duplicate disk with a partition with the specified filesystem type
	tmpdir := t.TempDir()
	tmpfile := filepath.Join(tmpdir, "testcopyfilesystem")
	if err := testCopyFile(imgFile, tmpfile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}

	f, err := os.OpenFile(tmpfile, os.O_RDWR, 0o666)
	if err != nil {
		t.Fatalf("failed to open disk image: %v", err)
	}
	defer func() { _ = f.Close() }()

	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table: %v", err)
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("unsupported partition table type, only GPT is supported")
	}
	// define resize target
	// find out what partitions we have and where they end, so we can determine where to start
	var (
		maxPart        = 1
		maxEnd  uint64 = 0
	)
	for _, part := range table.Partitions {
		if int(part.Index) > maxPart {
			maxPart = int(part.Index)
		}
		if end := part.Start + part.Size; end > maxEnd {
			maxEnd = end
		}
	}
	resizes := []partitionResizeTarget{
		{
			original: partitionData{
				number: table.Partitions[0].Index,
				start:  int64(table.Partitions[0].Start),
				size:   int64(table.Partitions[0].Size),
				label:  table.Partitions[0].Name,
			},
			target: partitionData{
				number: maxPart + 1,
				start:  int64(maxEnd + MB), // start it 1 MB after end of previous for extra safety
				size:   int64(table.Partitions[0].Size),
				label:  "part1_resized",
			},
		},
	}
	// create the new partition directly
	table.Partitions = append(table.Partitions, &gpt.Partition{
		Start:      uint64(resizes[0].target.start),
		Size:       uint64(resizes[0].target.size),
		Type:       table.Partitions[0].Type,
		Name:       table.Partitions[0].Name,
		Attributes: table.Partitions[0].Attributes,
		Index:      len(table.Partitions) + 1,
	})
	if err := d.Partition(table); err != nil {
		t.Fatalf("failed to write updated partition table: %v", err)
	}
	// call copyFilesystems
	if err := copyFilesystems(d, resizes); err != nil {
		t.Fatalf("copyFilesystems failed: %v", err)
	}
	// get old FS
	fs, err := d.GetFilesystem(resizes[0].original.number)
	if err != nil {
		t.Fatalf("failed to get filesystem on original partition: %v", err)
	}
	// verify filesystem copied
	newFS, err := d.GetFilesystem(resizes[0].target.number)
	if err != nil {
		t.Fatalf("failed to get filesystem on new partition: %v", err)
	}
	if newFS.Type() != fs.Type() {
		t.Errorf("filesystem type mismatch: expected %v, got %v", fs.Type(), newFS.Type())
	}
	// check that the contents match
	if err := iofs.WalkDir(fs, ".", func(path string, d iofs.DirEntry, err error) error {
		if err != nil {
			t.Fatalf("error walking original filesystem: %v", err)
		}
		if path == "." || path == "/" {
			return nil
		}
		origF, err := fs.Open(path)
		if err != nil {
			t.Fatalf("failed to open %s in original filesystem: %v", path, err)
		}
		info, err := origF.Stat()
		if err != nil {
			t.Fatalf("failed to stat %s in original filesystem: %v", path, err)
		}
		newF, err := newFS.Open(path)
		if err != nil {
			t.Fatalf("failed to open %s in new filesystem: %v", path, err)
		}
		newInfo, err := newF.Stat()
		if err != nil {
			t.Fatalf("failed to stat %s in new filesystem: %v", path, err)
		}
		if info.IsDir() && !newInfo.IsDir() {
			t.Errorf("expected %s to be a directory in new filesystem", path)
		}
		if !info.IsDir() && newInfo.IsDir() {
			t.Errorf("expected %s to be a file in new filesystem", path)
		}
		// a directory already was matched, so continue
		if info.IsDir() {
			return nil
		}
		// file, so check contents
		origData := make([]byte, info.Size())
		if _, err := origF.Read(origData); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("failed to read file %s in original filesystem: %v", path, err)
		}
		newData := make([]byte, newInfo.Size())
		if _, err := newF.Read(newData); err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("failed to read file %s in new filesystem: %v", path, err)
		}
		if !bytes.Equal(origData, newData) {
			t.Errorf("file content mismatch for %s: expected %q, got %q", path, string(origData), string(newData))
		}
		return nil
	}); err != nil {
		t.Fatalf("error walking original filesystem: %v", err)
	}
}

// TestCopyFilesystemsRawCopy exercises copyFilesystems' raw-block-copy
// branch — the same one that handles squashfs partitions in the EVE
// IMG[AB] case. partitionresizer routes both Type() == TypeSquashfs
// and "unrecognized filesystem" to CopyPartitionRaw (resize.go); this
// test triggers the path via an unrecognized-filesystem source so it
// doesn't have to work around go-diskfs's current squashfs writer
// limitations (squashfs.Finalize ignores fs.start; squashfs.Read on
// a non-zero start offset fails on fragment decompression).
//
// Source and target partitions are equal-sized here. The "target
// larger than source" grow case — which is what EVE actually wants
// for IMGA→IMGA2 — currently fails at go-diskfs's verifyBlockCopy
// (sync/verify.go) because it asserts target.ReadContents size ==
// source size, ignoring that the target may legitimately be larger
// and only the leading source-sized prefix is the meaningful copy.
// That's a go-diskfs bug to address separately.
func TestCopyFilesystemsRawCopy(t *testing.T) {
	workDir := t.TempDir()
	diskPath := filepath.Join(workDir, "disk.img")

	const (
		diskSize    int64 = 64 * MB
		sectorSize        = 4096
		sourceStart       = 256 // sectors; 1 MiB into disk
		sourceSize        = 8 * MB
		// Equal-sized target — exercises the raw-copy mechanism
		// without tripping go-diskfs's verifyBlockCopy size assertion.
		targetStart = sourceStart + (16 * MB / sectorSize)
		targetSize  = 8 * MB
	)

	// Pre-allocate the disk image.
	if err := os.WriteFile(diskPath, make([]byte, 0), 0o644); err != nil {
		t.Fatalf("create disk file: %v", err)
	}
	if err := os.Truncate(diskPath, diskSize); err != nil {
		t.Fatalf("size disk file: %v", err)
	}

	// Set up GPT with source + target partitions.
	backend, err := file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		_ = backend.Close()
		t.Fatalf("open disk: %v", err)
	}
	table := &gpt.Table{
		LogicalSectorSize:  sectorSize,
		PhysicalSectorSize: sectorSize,
		Partitions: []*gpt.Partition{
			{Index: 1, Start: sourceStart, Size: sourceSize, Type: gpt.LinuxFilesystem, Name: "source"},
			{Index: 2, Start: targetStart, Size: targetSize, Type: gpt.LinuxFilesystem, Name: "target"},
		},
	}
	if err := d.Partition(table); err != nil {
		_ = backend.Close()
		t.Fatalf("write partition table: %v", err)
	}
	if err := backend.Close(); err != nil {
		t.Fatalf("close backend after partition write: %v", err)
	}

	// Fill the source partition with a deterministic non-filesystem
	// pattern that go-diskfs cannot identify as any known type.
	srcPattern := make([]byte, sourceSize)
	for i := range srcPattern {
		// avoid magic bytes that any FS probe might match: rotating
		// non-zero pattern.
		srcPattern[i] = byte((i % 251) + 1)
	}
	rw, err := os.OpenFile(diskPath, os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open disk for embedding pattern: %v", err)
	}
	if _, err := rw.WriteAt(srcPattern, sourceStart*sectorSize); err != nil {
		_ = rw.Close()
		t.Fatalf("write pattern: %v", err)
	}
	if err := rw.Close(); err != nil {
		t.Fatalf("close disk after embedding: %v", err)
	}

	// Re-open and confirm go-diskfs reports the source partition as
	// having an unknown filesystem (which is the trigger for the
	// raw-copy branch we want to exercise).
	backend, err = file.OpenFromPath(diskPath, false)
	if err != nil {
		t.Fatalf("re-open backend: %v", err)
	}
	defer func() { _ = backend.Close() }()
	d, err = diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite), diskfs.WithSectorSize(sectorSize))
	if err != nil {
		t.Fatalf("re-open disk: %v", err)
	}
	if _, getErr := d.GetFilesystem(1); getErr == nil {
		t.Fatal("source partition should not have a recognizable filesystem; got nil error")
	}

	// Exercise the partitionresizer raw-copy path.
	resizes := []partitionResizeTarget{
		{
			original: partitionData{
				number: 1,
				start:  sourceStart * sectorSize,
				size:   sourceSize,
				label:  "source",
			},
			target: partitionData{
				number: 2,
				start:  targetStart * sectorSize,
				size:   targetSize,
				label:  "target",
			},
		},
	}
	if err := copyFilesystems(d, resizes); err != nil {
		t.Fatalf("copyFilesystems failed: %v", err)
	}

	// Verify the target's leading sourceSize bytes match the source —
	// this is the CopyPartitionRaw contract.
	osFile, err := os.Open(diskPath)
	if err != nil {
		t.Fatalf("open disk for verification: %v", err)
	}
	defer func() { _ = osFile.Close() }()
	dstBytes := make([]byte, sourceSize)
	if _, err := osFile.ReadAt(dstBytes, targetStart*sectorSize); err != nil {
		t.Fatalf("read target bytes: %v", err)
	}
	if !bytes.Equal(srcPattern, dstBytes) {
		// Pinpoint the first differing byte to aid debugging.
		for i := range srcPattern {
			if srcPattern[i] != dstBytes[i] {
				t.Errorf("raw-copy mismatch at byte %d: expected %#02x, got %#02x", i, srcPattern[i], dstBytes[i])
				break
			}
		}
	}

	// The bytes after the copied region (target offset sourceSize..targetSize)
	// were not part of the source and are not required to be any
	// particular value — CopyPartitionRaw only copies the source-sized
	// region. Don't assert on them.
}

// TestSwapPartitions verifies that swapPartitions round-trips the
// Name / Type / GUID / Attributes fields between the original slot
// and the target slot — this is the metadata-only step that gives
// the new (large) partition the original name and the old (small)
// partition the alternate label that removePartitions will later
// mark Unused.
func TestSwapPartitions(t *testing.T) {
	workDir := t.TempDir()
	f, err := os.CreateTemp(workDir, "disk.img")
	if err != nil {
		t.Fatalf("failed to create temp disk image: %v", err)
	}
	if err := os.Truncate(f.Name(), 1*GB); err != nil {
		t.Fatalf("failed to truncate disk image: %v", err)
	}
	defer func() { _ = f.Close() }()

	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}

	const (
		origGUID = "11111111-2222-3333-4444-555555555555"
		altGUID  = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
		origName = "part1"
		altName  = "part1_resized2"
	)
	var offset uint64 = 2048
	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{
				Index:      1,
				Start:      offset,
				Size:       36 * MB,
				Type:       gpt.LinuxFilesystem,
				GUID:       origGUID,
				Name:       origName,
				Attributes: 0x1,
			},
			{
				Index:      3,
				Start:      offset + 256*MB,
				Size:       128 * MB,
				Type:       gpt.EFISystemPartition,
				GUID:       altGUID,
				Name:       altName,
				Attributes: 0x4,
			},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("failed to write partition table: %v", err)
	}

	resizes := []partitionResizeTarget{
		{
			original: partitionData{number: 1},
			target:   partitionData{number: 3},
		},
	}
	if err := swapPartitions(d, resizes); err != nil {
		t.Fatalf("swapPartitions failed: %v", err)
	}

	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to re-read partition table: %v", err)
	}
	newTable, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("unsupported partition table type, only GPT is supported")
	}

	var slot1, slot3 *gpt.Partition
	for _, p := range newTable.Partitions {
		switch p.Index {
		case 1:
			slot1 = p
		case 3:
			slot3 = p
		}
	}
	if slot1 == nil {
		t.Fatal("partition slot 1 missing after swap")
	}
	if slot3 == nil {
		t.Fatal("partition slot 3 missing after swap")
	}

	// Slot 1 (the old small one) should now carry the alt-labeled
	// partition's metadata.
	if slot1.Name != altName {
		t.Errorf("slot 1 Name after swap: expected %q, got %q", altName, slot1.Name)
	}
	if slot1.Type != gpt.EFISystemPartition {
		t.Errorf("slot 1 Type after swap: expected %v, got %v", gpt.EFISystemPartition, slot1.Type)
	}
	if !strings.EqualFold(slot1.GUID, altGUID) {
		t.Errorf("slot 1 GUID after swap: expected %q, got %q", altGUID, slot1.GUID)
	}
	if slot1.Attributes != 0x4 {
		t.Errorf("slot 1 Attributes after swap: expected 0x4, got 0x%x", slot1.Attributes)
	}

	// Slot 3 (the new large one) should now carry the original's
	// metadata — this is what makes the new partition usable under
	// the original name without bootloader / UUID-referring callers
	// having to learn the new identity.
	if slot3.Name != origName {
		t.Errorf("slot 3 Name after swap: expected %q, got %q", origName, slot3.Name)
	}
	if slot3.Type != gpt.LinuxFilesystem {
		t.Errorf("slot 3 Type after swap: expected %v, got %v", gpt.LinuxFilesystem, slot3.Type)
	}
	if !strings.EqualFold(slot3.GUID, origGUID) {
		t.Errorf("slot 3 GUID after swap: expected %q, got %q", origGUID, slot3.GUID)
	}
	if slot3.Attributes != 0x1 {
		t.Errorf("slot 3 Attributes after swap: expected 0x1, got 0x%x", slot3.Attributes)
	}

	// Geometry is untouched by swapPartitions — only metadata moves.
	if slot1.Start != offset || slot1.Size != 36*MB {
		t.Errorf("slot 1 geometry changed unexpectedly: start=%d size=%d", slot1.Start, slot1.Size)
	}
	if slot3.Start != offset+256*MB || slot3.Size != 128*MB {
		t.Errorf("slot 3 geometry changed unexpectedly: start=%d size=%d", slot3.Start, slot3.Size)
	}
}

// TestShrinkFilesystems verifies that shrinkFilesystems skips
// partitions already at or below target size, invokes resize2fs only
// when a shrink is needed, and propagates resize errors.
func TestShrinkFilesystems(t *testing.T) {
	// Use the existing small fixture (testdata/dist/disk.img), which
	// has an ext4 partition at slot 2. Open via OpenFromPath so the
	// backend has a non-empty Path() — shrinkFilesystems needs it to
	// hand resize2fs a device path.
	workDir := t.TempDir()
	tmpFile := filepath.Join(workDir, "disk.img")
	if err := testCopyFile(imgFile, tmpFile); err != nil {
		t.Fatalf("failed to copy fixture: %v", err)
	}
	backend, err := file.OpenFromPath(tmpFile, false)
	if err != nil {
		t.Fatalf("failed to open disk image: %v", err)
	}
	defer func() { _ = backend.Close() }()

	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("failed to open disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("failed to get partition table: %v", err)
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("unsupported partition table type, only GPT is supported")
	}

	// Find the ext4 partition in the fixture.
	var ext4Part *gpt.Partition
	for _, p := range table.Partitions {
		fs, fsErr := d.GetFilesystem(p.Index)
		if fsErr == nil && fs.Type() == filesystem.TypeExt4 {
			ext4Part = p
			break
		}
	}
	if ext4Part == nil {
		t.Fatal("fixture has no ext4 partition; check buildimg.sh")
	}
	ext4Size := int64(ext4Part.Size)
	ext4Number := ext4Part.Index

	t.Run("skip when current size already at target", func(t *testing.T) {
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		called := false
		execResize2fs = func(_ string, _ int64, _ bool) error {
			called = true
			return nil
		}
		resizes := []partitionResizeTarget{
			{
				original: partitionData{number: ext4Number, size: ext4Size},
				target:   partitionData{size: ext4Size},
			},
		}
		if err := shrinkFilesystems(d, resizes, false); err != nil {
			t.Fatalf("shrinkFilesystems failed: %v", err)
		}
		if called {
			t.Error("execResize2fs should not be invoked when original.size == target.size")
		}
	})

	t.Run("skip when current size below target", func(t *testing.T) {
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		called := false
		execResize2fs = func(_ string, _ int64, _ bool) error {
			called = true
			return nil
		}
		resizes := []partitionResizeTarget{
			{
				original: partitionData{number: ext4Number, size: ext4Size},
				target:   partitionData{size: ext4Size + 8*MB},
			},
		}
		if err := shrinkFilesystems(d, resizes, false); err != nil {
			t.Fatalf("shrinkFilesystems failed: %v", err)
		}
		if called {
			t.Error("execResize2fs should not be invoked when original.size < target.size")
		}
	})

	t.Run("invokes resize2fs when shrink needed", func(t *testing.T) {
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		var gotPartDevice string
		var gotMB int64
		execResize2fs = func(partDevice string, newSizeMB int64, _ bool) error {
			gotPartDevice = partDevice
			gotMB = newSizeMB
			return nil
		}
		targetSize := ext4Size - 8*MB
		resizes := []partitionResizeTarget{
			{
				original: partitionData{number: ext4Number, size: ext4Size},
				target:   partitionData{size: targetSize},
			},
		}
		if err := shrinkFilesystems(d, resizes, false); err != nil {
			t.Fatalf("shrinkFilesystems failed: %v", err)
		}
		if gotPartDevice == "" {
			t.Error("execResize2fs was not called")
		}
		expectedMB := targetSize / (1024 * 1024)
		if gotMB != expectedMB {
			t.Errorf("execResize2fs newSizeMB: expected %d, got %d", expectedMB, gotMB)
		}
	})

	t.Run("propagates resize2fs error", func(t *testing.T) {
		orig := execResize2fs
		defer func() { execResize2fs = orig }()
		execResize2fs = func(_ string, _ int64, _ bool) error {
			return fmt.Errorf("simulated resize failure")
		}
		resizes := []partitionResizeTarget{
			{
				original: partitionData{number: ext4Number, size: ext4Size},
				target:   partitionData{size: ext4Size - 8*MB},
			},
		}
		err := shrinkFilesystems(d, resizes, false)
		if err == nil {
			t.Fatal("expected error from shrinkFilesystems when resize2fs fails")
		}
		if !strings.Contains(err.Error(), "simulated resize failure") {
			t.Errorf("error did not propagate: got %v", err)
		}
	})

	t.Run("rejects non-ext4 source", func(t *testing.T) {
		// FAT32 partition is slot 1 in the fixture.
		var fat32Number int
		for _, p := range table.Partitions {
			fs, fsErr := d.GetFilesystem(p.Index)
			if fsErr == nil && fs.Type() == filesystem.TypeFat32 {
				fat32Number = p.Index
				break
			}
		}
		if fat32Number == 0 {
			t.Skip("fixture has no FAT32 partition to test against")
		}
		resizes := []partitionResizeTarget{
			{
				original: partitionData{number: fat32Number, size: 30 * MB},
				target:   partitionData{size: 20 * MB},
			},
		}
		err := shrinkFilesystems(d, resizes, false)
		if err == nil {
			t.Fatal("expected error for non-ext4 source partition")
		}
		if !strings.Contains(err.Error(), "unsupported filesystem type") {
			t.Errorf("expected 'unsupported filesystem type' error, got %v", err)
		}
	})
}
