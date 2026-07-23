package partitionresizer

import (
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestApplyCreatesESPB drives Apply end-to-end on a disk image: a pre-ESP-B GPT
// (one small partition + free tail) gains the reserved ESP-B as an empty FAT32
// at #7 with the fresh-install GUID, published in a single final write.
func TestApplyCreatesESPB(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.img")
	const size = int64(3) * GiB

	d, err := diskfs.Create(img, size, diskfs.SectorSize512)
	if err != nil {
		t.Fatalf("create image: %v", err)
	}
	table := &gpt.Table{
		LogicalSectorSize: 512,
		Partitions: []*gpt.Partition{
			{Index: 4, Name: "CONFIG", Type: gpt.LinuxFilesystem, GUID: confGUID, Start: 2048, Size: uint64(100 * MiB)},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("write initial GPT: %v", err)
	}
	_ = d.Close()

	desired := []PartitionSpec{{
		Label: "EFI System", TypeGUID: string(gpt.EFISystemPartition),
		GUID: espBGUID, Index: 7, MinSize: 2 * GiB, FS: FSFAT32,
	}}
	if err := Apply(img, desired, nil, false, false); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	espb := readPartition(t, img, 7)
	if !strings.EqualFold(espb.GUID, espBGUID) {
		t.Errorf("#7 GUID = %s, want %s", espb.GUID, espBGUID)
	}
	if espb.Name != "EFI System" {
		t.Errorf("#7 label = %q, want \"EFI System\"", espb.Name)
	}
	if espb.GetSize() < 2*GiB {
		t.Errorf("#7 size = %d, want >= %d", espb.GetSize(), 2*GiB)
	}
	if !strings.EqualFold(string(espb.Type), string(gpt.EFISystemPartition)) {
		t.Errorf("#7 type = %s, want EFI System type", espb.Type)
	}
	// CONFIG (#4) must be untouched.
	cfg := readPartition(t, img, 4)
	if !strings.EqualFold(cfg.GUID, confGUID) {
		t.Errorf("#4 GUID changed to %s", cfg.GUID)
	}

	// The created partition carries an empty FAT32.
	d2, err := diskfs.Open(img, diskfs.WithSectorSize(512))
	if err != nil {
		t.Fatal(err)
	}
	fs, err := d2.GetFilesystem(7)
	if err != nil {
		_ = d2.Close()
		t.Fatalf("GetFilesystem(7): %v", err)
	}
	if fs.Type() != filesystem.TypeFat32 {
		t.Errorf("#7 filesystem = %v, want FAT32", fs.Type())
	}
	_ = d2.Close()

	// Idempotent: rerunning with ESP-B present is a no-op, still exactly one #7.
	if err := Apply(img, desired, nil, false, false); err != nil {
		t.Fatalf("Apply rerun: %v", err)
	}
	espb2 := readPartition(t, img, 7)
	if !strings.EqualFold(espb2.GUID, espBGUID) {
		t.Errorf("after rerun #7 GUID = %s, want %s", espb2.GUID, espBGUID)
	}
}

func getFilesystem(t *testing.T, img string, index int) filesystem.FileSystem {
	t.Helper()
	d, err := diskfs.Open(img, diskfs.WithSectorSize(512))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	fs, err := d.GetFilesystem(index)
	if err != nil {
		t.Fatalf("GetFilesystem(%d): %v", index, err)
	}
	return fs
}

func readPartition(t *testing.T, img string, index int) *gpt.Partition {
	t.Helper()
	d, err := diskfs.Open(img, diskfs.WithSectorSize(512))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	tr, err := d.GetPartitionTable()
	if err != nil {
		t.Fatal(err)
	}
	tbl := tr.(*gpt.Table)
	for _, p := range tbl.Partitions {
		if p.Index == index {
			return p
		}
	}
	t.Fatalf("partition #%d not found; table: %+v", index, tbl.Partitions)
	return nil
}
