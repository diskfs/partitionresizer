package partitionresizer

import (
	"os"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

const (
	KB = 1024
	MB = 1024 * KB
	GB = 1024 * MB
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
				Start:      offset,
				Size:       36 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part1",
				Attributes: 0,
			},
			{
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
	if err := createPartitions(d, resizes, false); err != nil {
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
	for i, r := range resizes {
		newPart := newTable.Partitions[i+2] // new partitions are at the end
		if int64(newPart.Start) != r.target.start {
			t.Errorf("partition %d start mismatch: expected %d, got %d", r.target.number, r.target.start, newPart.Start)
		}
		if int64(newPart.Size) != r.target.size {
			t.Errorf("partition %d size mismatch: expected %d, got %d", r.target.number, r.target.size, newPart.Size)
		}
		if newPart.Name != r.original.label {
			t.Errorf("partition %d label mismatch: expected %s, got %s", r.target.number, r.original.label, newPart.Name)
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
				Start:      offset,
				Size:       36 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part1",
				Attributes: 0,
			},
			{
				Start:      offset + 36*MB,
				Size:       200 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part2",
				Attributes: 0,
			},
			{
				Start:      offset + 36*MB + 200*MB,
				Size:       100 * MB,
				Type:       gpt.LinuxFilesystem,
				Name:       "part3",
				Attributes: 0,
			},
			{
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
	toRemove := []int{2, 3}

	// call removePartitions
	if err := removePartitions(d, toRemove, false); err != nil {
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
	expectedCount := len(table.Partitions) - len(toRemove)
	if len(newTable.Partitions) != expectedCount {
		t.Fatalf("expected %d partitions after resize, got %d", expectedCount, len(newTable.Partitions))
	}
}
