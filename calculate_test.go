package partitionresizer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/go-test/deep"
)

// TestComputeUnused verifies computeUnused on synthetic used intervals.
func TestComputeUnused(t *testing.T) {
	t.Run("gaps", func(t *testing.T) {
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
	})
	t.Run("full", func(t *testing.T) {
		size := int64(100)
		used := []usableBlock{
			{start: 0, end: 49},
			{start: 50, end: 99},
		}
		unused := computeUnused(size, used)
		if len(unused) != 0 {
			t.Fatalf("unexpected gaps: got %d, want 0", len(unused))
		}
	})
	t.Run("actual disk", func(t *testing.T) {
		tmpdir := t.TempDir()
		tmpfile := filepath.Join(tmpdir, "disk.img")
		if err := testCopyFile(imgFile, tmpfile); err != nil {
			t.Fatalf("failed to copy disk image: %v", err)
		}
		f, err := os.Open(tmpfile)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
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
	})
}

func TestCalculateResizes(t *testing.T) {
	tmpdir := t.TempDir()
	tmpfile := filepath.Join(tmpdir, "disk.img")
	if err := testCopyFile(imgFile, tmpfile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}
	f, err := os.Open(tmpfile)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
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
	t.Run("insufficient space", func(t *testing.T) {
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
	})
	t.Run("with existing space", func(t *testing.T) {
		// allocate half of the space at the end of the disk gap
		index := len(unused) - 1
		gap := unused[index]
		targetSize := (gap.end - gap.start + 1) / 2
		prt := partitionResizeTarget{
			original: partitionData{
				start:  parts[index].GetStart(),
				size:   parts[index].GetSize(),
				label:  parts[index].Name,
				number: int(parts[index].Index),
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
	})
	t.Run("with shrinking", func(t *testing.T) {
		// find out the size of the last used partition
		lastPart := parts[len(parts)-1]
		lastPartSize := int64(lastPart.GetSize())
		// first try without shrinking - should fail
		lastIndex := len(unused) - 1
		gap := unused[lastIndex]
		openSize := (gap.end - gap.start + 1)
		targetSize := openSize + (lastPartSize / 2)
		prt := partitionResizeTarget{
			original: partitionData{
				start:  parts[0].GetStart(),
				size:   parts[0].GetSize(),
				end:    parts[0].GetStart() + parts[0].GetSize() - 1,
				label:  parts[0].Name,
				number: int(parts[0].Index),
			},
			target: partitionData{
				size: targetSize,
			},
		}
		resizes, err := calculateResizes(d.Size, parts, []partitionResizeTarget{prt})
		if err == nil {
			t.Fatal("expected insufficient space error, got nil")
		}
		var ise *InsufficientSpaceError
		if !errors.As(err, &ise) {
			t.Fatalf("expected InsufficientSpaceError, got %T", err)
		}

		// now try with shrinking
		shrinkPart := partitionResizeTarget{
			original: partitionData{
				start:  lastPart.GetStart(),
				size:   lastPart.GetSize(),
				end:    lastPart.GetStart() + lastPart.GetSize() - 1,
				label:  lastPart.Name,
				number: int(lastPart.Index),
			},
			target: partitionData{
				size: lastPartSize / 2,
			},
		}
		resizes, err = calculateResizes(d.Size, parts, []partitionResizeTarget{shrinkPart, prt})
		if err != nil {
			t.Fatalf("calculateResizes with shrinking failed: %v", err)
		}
		if len(resizes) != 2 {
			t.Fatalf("got %d resizes, want 2", len(resizes))
		}
		// index 0 is the shrink, index 1 is the grow
		r := resizes[0]
		if r.target.start != r.original.start {
			t.Errorf("shrink resize start should be unchanged at %d, got %d", r.original.start, r.target.start)
		}
		if r.target.end != r.original.start+(lastPartSize/2)-1 {
			t.Errorf("shrink resize end = %d, want %d", r.target.end, r.original.start+(lastPartSize/2)-1)
		}
		if r.target.number != r.original.number {
			t.Errorf("shrink resize number should be unchanged at %d, got %d", r.original.number, r.target.number)
		}
		// now check the grow
		r = resizes[1]
		if r.target.start != resizes[0].target.end+1 {
			t.Errorf("resize start = %d, want %d", r.target.start, resizes[0].target.end+1)
		}
		if r.target.end != r.target.start+targetSize-1 {
			t.Errorf("resize end = %d, want %d", r.target.end, r.target.start+targetSize-1)
		}
	})
}

func TestSortAndCombineUsableBlocks(t *testing.T) {
	blocks := []usableBlock{
		{start: 30, end: 39},
		{start: 10, end: 19},
		{start: 20, end: 29},
		{start: 50, end: 59},
		{start: 60, end: 69},
	}
	combined := sortAndCombineUsableBlocks(blocks)
	want := []usableBlock{
		{start: 10, end: 39},
		{start: 50, end: 69},
	}
	if diff := deep.Equal(combined, want); diff != nil {
		t.Errorf("sortAndCombineUsableBlocks() = %v", diff)
	}
}
