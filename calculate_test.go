package partitionresizer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestComputeUnused_Synthetic verifies computeUnused on synthetic used intervals.
func TestComputeUnused_Synthetic(t *testing.T) {
	size := int64(100)
	used := []usableBlock{
		{start: 0, end: 8},
		{start: 20, end: 29},
		{start: 50, end: 69},
	}
	unused := computeUnused(size, used)
	want := []usableBlock{
		{start: 9, end: 19},
		{start: 30, end: 49},
		{start: 70, end: 99},
	}
	if len(unused) != len(want) {
		t.Fatalf("unexpected number of gaps: got %d, want %d", len(unused), len(want))
	}
	for i, u := range unused {
		if u.start != want[i].start || u.end != want[i].end {
			t.Errorf("gap[%d] = %+v, want %+v", i, u, want[i])
		}
	}
}

// TestCalculateResizes_OnTestDisk verifies calculateResizes against the real disk image.
func TestCalculateResizes_OnTestDisk(t *testing.T) {
	tmpdir := t.TempDir()
	tmpfile := filepath.Join(tmpdir, "disk.img")
	if err := testCopyFile(imgFile, tmpfile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}
	f, err := os.Open(tmpfile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatal(err)
	}
	table := tableRaw.(*gpt.Table)
	parts := table.Partitions
	var used []usableBlock
	for _, p := range parts {
		used = append(used, usableBlock{
			start: p.GetStart(),
			end:   p.GetStart() + p.GetSize() - 1,
			size:  p.GetSize(),
		})
	}
	unused := computeUnused(d.Size, used)
	if len(unused) == 0 {
		t.Fatalf("no unused space on disk")
	}
	// allocate half of the first gap
	gap := unused[0]
	targetSize := (gap.end - gap.start + 1) / 2
	prt := partitionResizeTarget{
		original: partitionData{
			start:  parts[0].GetStart(),
			size:   parts[0].GetSize(),
			label:  parts[0].Name,
			number: int(parts[0].Index),
		},
		target: partitionData{
			size: targetSize,
		},
	}
	resizes, err := calculateResizes(d.Size, parts, []partitionResizeTarget{prt})
	if err != nil {
		t.Fatalf("calculateResizes failed: %v", err)
	}
	if len(resizes) != 1 {
		t.Fatalf("got %d resizes, want 1", len(resizes))
	}
	r := resizes[0]
	if r.target.start != gap.start {
		t.Errorf("resize start = %d, want %d", r.target.start, gap.start)
	}
	if r.target.end != gap.start+targetSize-1 {
		t.Errorf("resize end = %d, want %d", r.target.end, gap.start+targetSize-1)
	}
}

// TestCalculateResizes_Insufficient_OnTestDisk verifies InsufficientSpaceError when requesting too large a resize.
func TestCalculateResizes_Insufficient_OnTestDisk(t *testing.T) {
	tmpdir := t.TempDir()
	tmpfile := filepath.Join(tmpdir, "disk.img")
	if err := testCopyFile(imgFile, tmpfile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}
	f, err := os.Open(tmpfile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatal(err)
	}
	table := tableRaw.(*gpt.Table)
	parts := table.Partitions
	var used []usableBlock
	for _, p := range parts {
		used = append(used, usableBlock{
			start: p.GetStart(),
			end:   p.GetStart() + p.GetSize() - 1,
			size:  p.GetSize(),
		})
	}
	unused := computeUnused(d.Size, used)
	if len(unused) == 0 {
		t.Fatalf("no unused space on disk")
	}
	// use the largest gap at the end of the disk
	gap := unused[len(unused)-1]
	targetSize := (gap.end - gap.start + 1) + 1
	prt := partitionResizeTarget{
		original: partitionData{
			start:  parts[0].GetStart(),
			size:   parts[0].GetSize(),
			label:  parts[0].Name,
			number: int(parts[0].Index),
		},
		target: partitionData{
			size: targetSize,
		},
	}
	_, err = calculateResizes(d.Size, parts, []partitionResizeTarget{prt})
	if err == nil {
		t.Fatal("expected insufficient space error, got nil")
	}
	var ise *InsufficientSpaceError
	if !errors.As(err, &ise) {
		t.Fatalf("expected InsufficientSpaceError, got %T", err)
	}
}

// TestComputeUnused_OnTestDisk uses testdata/dist/disk.img to verify a free-space gap at disk end.
func TestComputeUnused_OnTestDisk(t *testing.T) {
	tmpdir := t.TempDir()
	tmpfile := filepath.Join(tmpdir, "disk.img")
	if err := testCopyFile(imgFile, tmpfile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}
	f, err := os.Open(tmpfile)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	backend := file.New(f, false)
	d, err := diskfs.OpenBackend(backend, diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatal(err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatal(err)
	}
	table := tableRaw.(*gpt.Table)
	var used []usableBlock
	for _, p := range table.Partitions {
		used = append(used, usableBlock{start: int64(p.GetStart() + 1), end: int64(p.GetStart() + p.GetSize() - 1), size: int64(p.GetSize())})
	}
	unused := computeUnused(int64(d.Size), used)
	if len(unused) == 0 {
		t.Fatal("expected at least one unused block")
	}
	last := unused[len(unused)-1]
	if last.end != int64(d.Size)-1 {
		t.Errorf("last unused end = %d, want %d", last.end, d.Size-1)
	}
}
