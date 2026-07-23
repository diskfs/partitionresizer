package partitionresizer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// resize() pipeline step boundaries, in execution order. A value of N means
// "stop after the Nth step has written to disk", simulating a crash at that
// point.
const (
	stepShrinkFilesystems = iota + 1
	stepShrinkPartitions
	stepCreatePartitions
	stepCopyFilesystems
	stepUpdatePartitions // idempotent finalize: relabel/reindex + delete original
)

// runResizeStepsUpTo replays resize()'s pipeline against a freshly planned
// resizes slice, stopping after the step given by stopAfter. It simulates the
// process being interrupted immediately after that step has persisted its
// changes, so that a subsequent Apply() must resume from a partially-modified
// disk. The plan is computed exactly as Apply() does, from the current on-disk
// state.
func runResizeStepsUpTo(t *testing.T, path string, desired []PartitionSpec, shrink *ShrinkSpec, stopAfter int, formatTargetsNoCopy, writeExtraFile bool) {
	t.Helper()
	backend, err := file.OpenFromPath(path, false)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	// close (flush) the partial handle before Apply() reopens the path
	defer func() { _ = backend.Close() }()

	d, err := diskfs.OpenBackend(backend)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get partition table: %v", err)
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		t.Fatalf("expected GPT table")
	}
	resizes, err := planApply(d, table, desired, shrink)
	if err != nil {
		t.Fatalf("planApply: %v", err)
	}

	steps := []struct {
		name string
		fn   func() error
	}{
		{"shrinkFilesystems", func() error { return shrinkFilesystems(d, resizes, false) }},
		{"shrinkPartitions", func() error { return shrinkPartitions(d, resizes) }},
		{"createPartitions", func() error { return createPartitions(d, resizes) }},
		{"copyFilesystems", func() error { return copyFilesystems(d, resizes) }},
		{"updatePartitions", func() error { return updatePartitions(d, resizes, true) }},
	}
	for i := 0; i < stopAfter && i < len(steps); i++ {
		if err := steps[i].fn(); err != nil {
			t.Fatalf("partial step %q failed: %v", steps[i].name, err)
		}
	}

	// formatTargetsNoCopy simulates a crash *inside* copyFilesystems: the
	// target filesystem has been created (CreateFilesystem) but no data has
	// been copied into it yet. This is the case that forces the resume path to
	// run CreateFilesystem over an already-formatted partition.
	if formatTargetsNoCopy {
		for _, r := range resizes {
			if r.original.start == r.target.start {
				continue
			}
			src, err := d.GetFilesystem(r.original.number)
			if err != nil {
				continue // raw-copy types (squashfs/unknown): nothing to pre-create
			}
			switch src.Type() {
			case filesystem.TypeExt4, filesystem.TypeFat32:
				newFS, err := d.CreateFilesystem(disk.FilesystemSpec{
					Partition:   r.target.number,
					FSType:      src.Type(),
					VolumeLabel: src.Label(),
				})
				if err != nil {
					t.Fatalf("pre-create target fs on partition %d: %v", r.target.number, err)
				}
				if writeExtraFile {
					// Leave a stale file behind so the target is non-empty and
					// does NOT match the source: CompareFS must report the
					// difference and the resume must reformat+recopy (the stale
					// file must be gone in the final result).
					fh, err := newFS.OpenFile("/resume-junk.bin", os.O_CREATE|os.O_RDWR)
					if err != nil {
						t.Fatalf("open junk file on target %d: %v", r.target.number, err)
					}
					if _, err := fh.Write([]byte("stale data from an interrupted run; must be discarded")); err != nil {
						t.Fatalf("write junk file on target %d: %v", r.target.number, err)
					}
					if err := fh.Close(); err != nil {
						t.Fatalf("close junk file on target %d: %v", r.target.number, err)
					}
				}
			}
		}
	}
}

// TestRunResumeAfterInterruption verifies restart-safety: the tool is
// interrupted after each step of the resize pipeline, then re-run, and the
// final disk must match the result of an uninterrupted run.
//
// This exercises the user-driven "the machine rebooted mid-resize, run it
// again" operation, which has no coverage today (the only resume guard,
// createPartitions' "alternate partition already exists" branch, is never hit
// by a test). It is the functional counterpart to those uncovered blocks.
func TestRunResumeAfterInterruption(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test (real shrink/copy of a multi-GB fixture)")
	}
	grow := []PartitionSpec{
		growByLabel("parta", 2*GB),
		growByLabel("partb", 2*GB),
		growByLabel("ESP", 1*GB),
	}

	cases := []struct {
		name      string
		stopAfter int
		// formatTargetsNoCopy simulates a crash inside copyFilesystems (target
		// filesystem created, data not yet copied).
		formatTargetsNoCopy bool
		// writeExtraFile additionally leaves a stale file in the created target
		// fs, so it is non-empty and does not match the source.
		writeExtraFile bool
	}{
		{name: "afterShrinkFilesystems", stopAfter: stepShrinkFilesystems},
		{name: "afterShrinkPartitions", stopAfter: stepShrinkPartitions},
		{
			name:      "afterCreatePartitions",
			stopAfter: stepCreatePartitions,
		},
		{
			// crash mid-copy: target fs created but empty, so resume must
			// CreateFilesystem over an already-formatted partition, then copy.
			name:                "midCopyTargetFsCreated",
			stopAfter:           stepCreatePartitions,
			formatTargetsNoCopy: true,
		},
		{
			// crash mid-copy with a non-empty, mismatched target (a stale file
			// from a prior run): CompareFS reports the difference, so resume
			// reformats over the populated fs and recopies; the stale file is
			// gone in the final result.
			name:                "midCopyTargetFsHasStaleFile",
			stopAfter:           stepCreatePartitions,
			formatTargetsNoCopy: true,
			writeExtraFile:      true,
		},
		{
			name:      "afterCopyFilesystems",
			stopAfter: stepCopyFilesystems,
		},
		{
			// the whole resize completed, then the tool was re-run: the
			// idempotent updatePartitions plus the "already at target size"
			// short-circuit in planApply must make this a no-op.
			name:      "afterUpdatePartitions",
			stopAfter: stepUpdatePartitions,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			tmpFile := filepath.Join(tmpDir, "diskfull.img")
			if err := testCopyFile(diskfullImg, tmpFile); err != nil {
				t.Fatalf("failed to copy disk image: %v", err)
			}
			origShrinkSize, origNumber := readOriginalLayout(t, tmpFile)

			// simulate a crash partway through the resize
			runResizeStepsUpTo(t, tmpFile, grow, shrinkToFitLabel("shrinker"), tc.stopAfter, tc.formatTargetsNoCopy, tc.writeExtraFile)

			// resume: a fresh Apply() must finish the resize correctly
			if err := Apply(tmpFile, grow, shrinkToFitLabel("shrinker"), false, false); err != nil {
				t.Fatalf("resume Apply failed: %v", err)
			}

			assertResizedLayout(t, tmpFile, origShrinkSize, origNumber)
		})
	}
}

// readOriginalLayout records the shrinker partition size and the original
// partition numbers from a pristine disk image, for later comparison.
func readOriginalLayout(t *testing.T, path string) (shrinkSize int64, numbers map[string]int) {
	t.Helper()
	backend, err := file.OpenFromPath(path, true)
	if err != nil {
		t.Fatalf("open original backend: %v", err)
	}
	defer func() { _ = backend.Close() }()
	d, err := diskfs.OpenBackend(backend)
	if err != nil {
		t.Fatalf("open original disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get original partition table: %v", err)
	}
	table := tableRaw.(*gpt.Table)
	numbers = map[string]int{}
	for _, p := range table.Partitions {
		if p.Type == gpt.Unused {
			continue
		}
		numbers[p.Name] = int(p.Index)
		if p.Name == "shrinker" {
			shrinkSize = int64(p.GetSize())
		}
	}
	if shrinkSize == 0 {
		t.Fatal("could not find shrinker partition in original disk")
	}
	return shrinkSize, numbers
}

// assertResizedLayout checks that the disk reflects a completed resize:
// shrinker shrunk by the total grow, the three grown partitions at their
// requested sizes, ESP still FAT32, and every partition keeping its original
// number. This is the same end state asserted by TestApplyResize's
// uninterrupted path.
func assertResizedLayout(t *testing.T, path string, origShrinkSize int64, origNumber map[string]int) {
	t.Helper()
	backend, err := file.OpenFromPath(path, true)
	if err != nil {
		t.Fatalf("open resized backend: %v", err)
	}
	defer func() { _ = backend.Close() }()
	d, err := diskfs.OpenBackend(backend)
	if err != nil {
		t.Fatalf("open resized disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get resized partition table: %v", err)
	}
	table := tableRaw.(*gpt.Table)

	var active []*gpt.Partition
	for _, p := range table.Partitions {
		if p.Type != gpt.Unused {
			active = append(active, p)
		}
	}
	if len(active) != 4 {
		t.Fatalf("expected 4 active partitions, got %d: %+v", len(active), active)
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
				t.Errorf("shrinker size = %d, want %d", size, expectShrink)
			}
		case "parta", "partb":
			if size != int64(2*GB) {
				t.Errorf("%s size = %d, want %d", name, size, 2*GB)
			}
		case "ESP":
			if size != int64(1*GB) {
				t.Errorf("ESP size = %d, want %d", size, 1*GB)
			}
			fs, err := d.GetFilesystem(int(p.Index))
			if err != nil {
				t.Errorf("get ESP filesystem: %v", err)
			} else if fs.Type() != filesystem.TypeFat32 {
				t.Errorf("ESP filesystem type = %v, want FAT32", fs.Type())
			}
		default:
			t.Errorf("unexpected active partition %q", name)
		}
		if int(p.Index) != origNumber[name] {
			t.Errorf("%s number = %d, want %d (preserved)", name, p.Index, origNumber[name])
		}
		seen[name] = true
	}
	for _, n := range []string{"shrinker", "parta", "partb", "ESP"} {
		if !seen[n] {
			t.Errorf("missing active partition %q", n)
		}
	}
}

// TestRunAbortsOnFsckFailure verifies that when the shrink partition's ext4
// filesystem is corrupt, the e2fsck run inside the shrink step fails and the
// whole resize aborts (with fixErrors=false) rather than shrinking a broken
// filesystem. This is the user-driven "the source filesystem is damaged"
// case: the tool must refuse, not proceed.
func TestRunAbortsOnFsckFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test (real shrink/copy of a multi-GB fixture)")
	}
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "diskfull.img")
	if err := testCopyFile(diskfullImg, tmpFile); err != nil {
		t.Fatalf("failed to copy disk image: %v", err)
	}

	shrinkStart := partitionStartByLabel(t, tmpFile, "shrinker")

	// Corrupt ext4 metadata well past the primary superblock (+1024) and group
	// descriptors, so GetFilesystem still recognizes the filesystem as ext4 but
	// e2fsck -f finds errors. 4 MiB of garbage at +2 MiB lands in the early
	// inode tables/bitmaps.
	corruptRegion(t, tmpFile, shrinkStart+2*1024*1024, 4*1024*1024)

	grow := []PartitionSpec{
		growByLabel("parta", 2*GB),
		growByLabel("partb", 2*GB),
		growByLabel("ESP", 1*GB),
	}

	// fixErrors=false: e2fsck -n must refuse the corrupt fs and the resize must
	// abort before touching the partition layout.
	err := Apply(tmpFile, grow, shrinkToFitLabel("shrinker"), false, false)
	if err == nil {
		t.Fatal("expected Apply to fail on a corrupt shrink filesystem, got nil")
	}
	t.Logf("Apply correctly aborted on corrupt shrink filesystem: %v", err)
}

// partitionStartByLabel returns the byte offset of the named GPT partition.
func partitionStartByLabel(t *testing.T, path, label string) int64 {
	t.Helper()
	backend, err := file.OpenFromPath(path, true)
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	defer func() { _ = backend.Close() }()
	d, err := diskfs.OpenBackend(backend)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get partition table: %v", err)
	}
	table := tableRaw.(*gpt.Table)
	for _, p := range table.Partitions {
		if p.Name == label {
			return p.GetStart()
		}
	}
	t.Fatalf("partition %q not found", label)
	return 0
}

// corruptRegion overwrites length bytes at offset in the file with a fixed
// non-zero pattern.
func corruptRegion(t *testing.T, path string, offset, length int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open for corruption: %v", err)
	}
	defer func() { _ = f.Close() }()
	junk := make([]byte, length)
	for i := range junk {
		junk[i] = 0xAB
	}
	if _, err := f.WriteAt(junk, offset); err != nil {
		t.Fatalf("write corruption: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync corruption: %v", err)
	}
}
