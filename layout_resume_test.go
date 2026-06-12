package partitionresizer

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// Scaled-down sample layout used by the resume tests. It mirrors the partition
// shape of an A/B-image appliance disk -- the layout EVE-OS uses, which
// motivated this tool: a FAT32 ESP, two read-only squashfs image slots, a small
// config partition, and an ext4 data partition that fills the rest at a
// non-contiguous index. Every size is scaled down from the real ~64 GB device
// so the tests run in seconds while exercising the same partition types and
// code paths:
//
//	ESP    FAT32, "tiny"            48 MB   index 1
//	IMGA   squashfs image          100 MB  index 2
//	IMGB   squashfs image          100 MB  index 3
//	CONFIG placeholder ("1MB")       1 MB   index 4   (untouched by the resize)
//	P9     ext4, fills the rest    900 MB  index 9
//
// FAT32 cannot be represented at 1 MB (it needs tens of MB of clusters), and
// partitionresizer never touches CONFIG, so CONFIG is left as a 1 MB
// placeholder partition.
const (
	sectorMB      = MB / 512 // sectors per MB at 512-byte sectors
	defaultDiskMB = 1152
	espStartMB    = 1
	espMB         = 48
	imgaMB        = 100
	imgbMB        = 100
	configMB      = 1
	defaultP9MB   = 900

	espIndex    = 1
	imgaIndex   = 2
	imgbIndex   = 3
	configIndex = 4
	p9Index     = 9
)

// sampleLayout records the built disk and the content fingerprints needed to
// verify that a resize preserves data.
type sampleLayout struct {
	path        string
	diskMB      int64
	p9MB        int64
	espStart    uint64 // sectors
	imgaStart   uint64
	imgbStart   uint64
	configStart uint64
	p9Start     uint64
	imgaSum     [32]byte // sha256 of the IMGA partition image region
	imgbSum     [32]byte
}

// buildSampleLayout writes the scaled sample disk image into a temp file and
// returns its description. ESP and P9 get real FAT32/ext4 filesystems with
// marker files; IMGA/IMGB get real squashfs images written raw (built with
// mksquashfs, as the real read-only image slots are), since go-diskfs cannot create squashfs and ext4 on
// the same 512-byte-sector disk.
func buildSampleLayout(t *testing.T) sampleLayout {
	return buildSampleLayoutSized(t, defaultDiskMB, defaultP9MB)
}

// buildSampleLayoutSized is buildSampleLayout with an explicit disk size and P9
// size, for tests that need a larger persist partition (e.g. a combined
// shrink+grow in one Run, where go-diskfs rounds the shrink up to a whole GB).
func buildSampleLayoutSized(t *testing.T, diskMB, p9MB int64) sampleLayout {
	t.Helper()
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not available")
	}

	dir := t.TempDir()
	imgPath := filepath.Join(dir, "disk.img")

	espStart := uint64(espStartMB * sectorMB)
	imgaStart := espStart + uint64(espMB*sectorMB)
	imgbStart := imgaStart + uint64(imgaMB*sectorMB)
	configStart := imgbStart + uint64(imgbMB*sectorMB)
	p9Start := configStart + uint64(configMB*sectorMB)

	f, err := os.Create(imgPath)
	if err != nil {
		t.Fatalf("create disk image: %v", err)
	}
	if err := f.Truncate(diskMB * MB); err != nil {
		t.Fatalf("truncate disk image: %v", err)
	}
	_ = f.Close()

	f, err = os.OpenFile(imgPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("reopen disk image: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, false), diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}

	table := &gpt.Table{
		Partitions: []*gpt.Partition{
			{Index: espIndex, Start: espStart, Size: espMB * MB, Type: gpt.EFISystemPartition, Name: "ESP"},
			{Index: imgaIndex, Start: imgaStart, Size: imgaMB * MB, Type: gpt.LinuxFilesystem, Name: "IMGA"},
			{Index: imgbIndex, Start: imgbStart, Size: imgbMB * MB, Type: gpt.LinuxFilesystem, Name: "IMGB"},
			{Index: configIndex, Start: configStart, Size: configMB * MB, Type: gpt.LinuxFilesystem, Name: "CONFIG"},
			{Index: p9Index, Start: p9Start, Size: uint64(p9MB) * MB, Type: gpt.LinuxFilesystem, Name: "P9"},
		},
	}
	if err := d.Partition(table); err != nil {
		t.Fatalf("write partition table: %v", err)
	}

	// ESP: real FAT32 with a marker file
	espFS, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: espIndex, FSType: filesystem.TypeFat32, VolumeLabel: "ESP"})
	if err != nil {
		t.Fatalf("create ESP fat32: %v", err)
	}
	writeMarker(t, espFS, "/esp-marker.txt", "esp content")

	// P9: real ext4 with a marker file
	p9FS, err := d.CreateFilesystem(disk.FilesystemSpec{Partition: p9Index, FSType: filesystem.TypeExt4, VolumeLabel: "P9"})
	if err != nil {
		t.Fatalf("create P9 ext4: %v", err)
	}
	writeMarker(t, p9FS, "/p9-marker.txt", "persist content")

	// IMGA/IMGB: real squashfs images built with mksquashfs and written raw
	writeSquashfsImage(t, d, imgaStart, "IMGA")
	writeSquashfsImage(t, d, imgbStart, "IMGB")
	if err := f.Sync(); err != nil {
		t.Fatalf("sync disk image: %v", err)
	}

	return sampleLayout{
		path:        imgPath,
		diskMB:      diskMB,
		p9MB:        p9MB,
		espStart:    espStart,
		imgaStart:   imgaStart,
		imgbStart:   imgbStart,
		configStart: configStart,
		p9Start:     p9Start,
		// fingerprint the leading 1 MB of each IMG region (squashfs blob plus
		// trailing zeros) so a raw relocation can be checked for fidelity
		imgaSum: partitionRegionSum(t, imgPath, imgaStart, 1*MB),
		imgbSum: partitionRegionSum(t, imgPath, imgbStart, 1*MB),
	}
}

func writeMarker(t *testing.T, fs filesystem.FileSystem, name, content string) {
	t.Helper()
	fh, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	if _, err := fh.Write([]byte(content)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := fh.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

// writeSquashfsImage builds a small squashfs with mksquashfs and writes it raw
// at the given partition start; returns the sha256 of the written image bytes
// so the test can confirm a raw copy preserved it.
func writeSquashfsImage(t *testing.T, d *disk.Disk, startSector uint64, label string) [32]byte {
	t.Helper()
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "marker"), []byte(label+" squashfs payload"), 0o644); err != nil {
		t.Fatalf("write squashfs source: %v", err)
	}
	sqfs := filepath.Join(t.TempDir(), label+".sqfs")
	out, err := exec.Command("mksquashfs", srcDir, sqfs, "-noappend", "-no-progress", "-quiet").CombinedOutput()
	if err != nil {
		t.Fatalf("mksquashfs: %v\n%s", err, out)
	}
	data, err := os.ReadFile(sqfs)
	if err != nil {
		t.Fatalf("read squashfs: %v", err)
	}
	w, err := d.Backend.Writable()
	if err != nil {
		t.Fatalf("writable backend: %v", err)
	}
	if _, err := w.WriteAt(data, int64(startSector)*512); err != nil {
		t.Fatalf("write squashfs image: %v", err)
	}
	return sha256.Sum256(data)
}

// partitionRegionSum hashes the leading len bytes of the partition that begins
// at startSector, for raw-copy content verification.
func partitionRegionSum(t *testing.T, path string, startSector uint64, length int) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open for hashing: %v", err)
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, length)
	if _, err := f.ReadAt(buf, int64(startSector)*512); err != nil {
		t.Fatalf("read region: %v", err)
	}
	return sha256.Sum256(buf)
}

// gptByName reads the partition table at path and returns name->partition.
func gptByName(t *testing.T, path string) map[string]*gpt.Partition {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, true))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	tr, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get table: %v", err)
	}
	m := make(map[string]*gpt.Partition)
	for _, p := range tr.(*gpt.Table).Partitions {
		if p.Type == gpt.Unused {
			continue
		}
		m[p.Name] = p
	}
	return m
}

// TestSampleLayoutSmoke is a non-interrupted end-to-end check of the scaled sample
// layout: build it, shrink P9 (Case 1), then grow ESP/IMGA/IMGB into the freed
// space with the updatePartitions finalize (Case 2), asserting the layout and
// that IMG content survives the raw relocation.
func TestSampleLayoutSmoke(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)

	// the 1 MB region fingerprint captures the (tiny) squashfs blob plus zeros
	const hashLen = 1 * MB

	// confirm filesystem detection on the source partitions
	report := describeLayout(t, fx.path)
	t.Logf("fixture filesystems:\n%s", report)

	// Case 1: shrink P9 by 600 MB (free space at the end of the disk)
	p9Shrink := []PartitionChange{NewPartitionChange(IdentifierByLabel, "P9", (defaultP9MB-600)*MB)}
	if err := Run(fx.path, nil, p9Shrink, false, false, false); err != nil {
		t.Fatalf("Case 1 (shrink P9) failed: %v", err)
	}
	after1 := gptByName(t, fx.path)
	if got := int64(after1["P9"].GetSize()); got != (defaultP9MB-600)*MB {
		t.Errorf("after Case 1: P9 size = %d, want %d", got, (defaultP9MB-600)*MB)
	}

	// Case 2: grow ESP/IMGA/IMGB into the freed space; preserveNumbers keeps
	// their original indices/labels via updatePartitions
	grow := []PartitionChange{
		NewPartitionChange(IdentifierByLabel, "ESP", 96*MB),
		NewPartitionChange(IdentifierByLabel, "IMGA", 200*MB),
		NewPartitionChange(IdentifierByLabel, "IMGB", 200*MB),
	}
	if err := Run(fx.path, nil, grow, false, false, true); err != nil {
		t.Fatalf("Case 2 (grow + updatePartitions) failed: %v", err)
	}

	after2 := gptByName(t, fx.path)
	// ESP/IMGA/IMGB keep their labels and indices, now at the end of the disk
	for _, name := range []string{"ESP", "IMGA", "IMGB", "CONFIG", "P9"} {
		if _, ok := after2[name]; !ok {
			t.Errorf("after Case 2: partition %q missing", name)
		}
	}
	if after2["ESP"].Index != espIndex || after2["IMGA"].Index != imgaIndex || after2["IMGB"].Index != imgbIndex {
		t.Errorf("after Case 2: indices not preserved: ESP=%d IMGA=%d IMGB=%d", after2["ESP"].Index, after2["IMGA"].Index, after2["IMGB"].Index)
	}
	// the grown ESP/IMGA/IMGB must now start after the (shrunken) P9
	if after2["IMGA"].Start <= after2["P9"].Start {
		t.Errorf("after Case 2: IMGA (start %d) should be relocated past P9 (start %d)", after2["IMGA"].Start, after2["P9"].Start)
	}
	if int64(after2["IMGA"].GetSize()) != 200*MB || int64(after2["IMGB"].GetSize()) != 200*MB || int64(after2["ESP"].GetSize()) != 96*MB {
		t.Errorf("after Case 2: grown sizes wrong: ESP=%d IMGA=%d IMGB=%d", after2["ESP"].GetSize(), after2["IMGA"].GetSize(), after2["IMGB"].GetSize())
	}
	// IMG content survived the raw relocation (compare the relocated region to
	// the build-time fingerprint of the source region)
	if got := partitionRegionSum(t, fx.path, after2["IMGA"].Start, hashLen); got != fx.imgaSum {
		t.Errorf("IMGA content not preserved across relocation")
	}
	if got := partitionRegionSum(t, fx.path, after2["IMGB"].Start, hashLen); got != fx.imgbSum {
		t.Errorf("IMGB content not preserved across relocation")
	}
}

func describeLayout(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, true))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	var s string
	for _, idx := range []int{espIndex, imgaIndex, imgbIndex, p9Index} {
		fs, err := d.GetFilesystem(idx)
		if err != nil {
			s += fmt.Sprintf("  partition %d: GetFilesystem error: %v\n", idx, err)
			continue
		}
		s += fmt.Sprintf("  partition %d: %v\n", idx, fs.Type())
	}
	return s
}

// readPartitionFile reads a file from the filesystem on the given partition.
func readPartitionFile(t *testing.T, path string, partIndex int, name string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, true))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	fs, err := d.GetFilesystem(partIndex)
	if err != nil {
		t.Fatalf("get filesystem on partition %d: %v", partIndex, err)
	}
	fh, err := fs.OpenFile(name, os.O_RDONLY)
	if err != nil {
		t.Fatalf("open %s on partition %d: %v", name, partIndex, err)
	}
	defer func() { _ = fh.Close() }()
	data, err := io.ReadAll(fh)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

// shrinkP9 is the Case 1 operation: shrink the P9 ext4 partition in place by
// 600 MB, freeing space at the end of the disk. It is expressed as a
// PartitionChange to a smaller size (calculateResizes treats target < original
// as a shrink in place).
var shrinkP9 = []PartitionChange{NewPartitionChange(IdentifierByLabel, "P9", (defaultP9MB-600)*MB)}

// growImages is the Case 2 operation: grow ESP/IMGA/IMGB into the freed space.
var growImages = []PartitionChange{
	NewPartitionChange(IdentifierByLabel, "ESP", 96*MB),
	NewPartitionChange(IdentifierByLabel, "IMGA", 200*MB),
	NewPartitionChange(IdentifierByLabel, "IMGB", 200*MB),
}

// dummyShrink is an unused shrink identifier required by runResizeStepsUpTo's
// signature; both cases drive their shrink/grow entirely through the grow
// list (P9-in-place for Case 1, free-space grows for Case 2), so planResizes
// never consults shrinkPartition.
func dummyShrink() PartitionIdentifier { return NewPartitionIdentifier(IdentifierByLabel, "P9") }

// TestRunResumeShrink is Case 1 through the interrupt/resume harness: on the
// scaled sample layout, interrupt the in-place P9 shrink after each step, re-run to
// completion, and confirm P9 is shrunk (with its content and the other
// partitions intact). Exercises the non-contiguous P9 index (index 9) that the
// shrinkPartitions fix addresses.
func TestRunResumeShrink(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	for _, stopAfter := range []int{stepShrinkFilesystems, stepShrinkPartitions, stepUpdatePartitions} {
		t.Run(fmt.Sprintf("stopAfter=%d", stopAfter), func(t *testing.T) {
			fx := buildSampleLayout(t)
			runResizeStepsUpTo(t, fx.path, dummyShrink(), shrinkP9, false, stopAfter, false, false)
			if err := Run(fx.path, nil, shrinkP9, false, false, false); err != nil {
				t.Fatalf("resume Run failed: %v", err)
			}
			after := gptByName(t, fx.path)
			if got := int64(after["P9"].GetSize()); got != (defaultP9MB-600)*MB {
				t.Errorf("P9 size = %d, want %d", got, (defaultP9MB-600)*MB)
			}
			// P9 data preserved, other partitions untouched
			if got := readPartitionFile(t, fx.path, p9Index, "/p9-marker.txt"); got != "persist content" {
				t.Errorf("P9 marker = %q, want %q", got, "persist content")
			}
			for _, name := range []string{"ESP", "IMGA", "IMGB", "CONFIG"} {
				if _, ok := after[name]; !ok {
					t.Errorf("partition %q missing after shrink", name)
				}
			}
			if int64(after["ESP"].GetSize()) != espMB*MB || after["IMGA"].Start != fx.imgaStart {
				t.Errorf("non-P9 partitions changed: ESP size=%d IMGA start=%d", after["ESP"].GetSize(), after["IMGA"].Start)
			}
		})
	}
}

// TestRunResumeGrow is Case 2 through the interrupt/resume harness: starting
// from the Case 1 result (P9 already shrunk), interrupt the grow of
// ESP/IMGA/IMGB after each step, re-run to completion, and confirm the grown
// partitions are relocated to the end of the disk under their original
// labels/indices with content intact, while P9/CONFIG are untouched.
func TestRunResumeGrow(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	// build the fixture and run Case 1 once to produce the post-shrink base disk
	base := buildSampleLayout(t)
	if err := Run(base.path, nil, shrinkP9, false, false, false); err != nil {
		t.Fatalf("Case 1 (shrink) setup failed: %v", err)
	}

	for _, stopAfter := range []int{stepCreatePartitions, stepCopyFilesystems, stepUpdatePartitions} {
		t.Run(fmt.Sprintf("stopAfter=%d", stopAfter), func(t *testing.T) {
			diskCopy := filepath.Join(t.TempDir(), "disk.img")
			if err := testCopyFile(base.path, diskCopy); err != nil {
				t.Fatalf("copy base disk: %v", err)
			}
			runResizeStepsUpTo(t, diskCopy, dummyShrink(), growImages, true, stopAfter, false, false)
			if err := Run(diskCopy, nil, growImages, false, false, true); err != nil {
				t.Fatalf("resume Run failed: %v", err)
			}

			after := gptByName(t, diskCopy)
			// grown partitions keep label+index, relocated past P9, at target size
			if after["ESP"].Index != espIndex || after["IMGA"].Index != imgaIndex || after["IMGB"].Index != imgbIndex {
				t.Errorf("indices not preserved: ESP=%d IMGA=%d IMGB=%d", after["ESP"].Index, after["IMGA"].Index, after["IMGB"].Index)
			}
			if after["IMGA"].Start <= after["P9"].Start || after["IMGB"].Start <= after["P9"].Start || after["ESP"].Start <= after["P9"].Start {
				t.Errorf("grown partitions not relocated past P9: ESP=%d IMGA=%d IMGB=%d P9=%d",
					after["ESP"].Start, after["IMGA"].Start, after["IMGB"].Start, after["P9"].Start)
			}
			if int64(after["ESP"].GetSize()) != 96*MB || int64(after["IMGA"].GetSize()) != 200*MB || int64(after["IMGB"].GetSize()) != 200*MB {
				t.Errorf("grown sizes wrong: ESP=%d IMGA=%d IMGB=%d", after["ESP"].GetSize(), after["IMGA"].GetSize(), after["IMGB"].GetSize())
			}
			// content preserved: IMG raw images and the ESP marker
			if got := partitionRegionSum(t, diskCopy, after["IMGA"].Start, 1*MB); got != base.imgaSum {
				t.Errorf("IMGA content not preserved")
			}
			if got := partitionRegionSum(t, diskCopy, after["IMGB"].Start, 1*MB); got != base.imgbSum {
				t.Errorf("IMGB content not preserved")
			}
			if got := readPartitionFile(t, diskCopy, espIndex, "/esp-marker.txt"); got != "esp content" {
				t.Errorf("ESP marker = %q, want %q", got, "esp content")
			}
			// P9 untouched (still shrunk, content intact)
			if got := int64(after["P9"].GetSize()); got != (defaultP9MB-600)*MB {
				t.Errorf("P9 size changed: %d", got)
			}
			if got := readPartitionFile(t, diskCopy, p9Index, "/p9-marker.txt"); got != "persist content" {
				t.Errorf("P9 marker = %q", got)
			}
		})
	}
}

// gptDump renders the GPT partition table at path, sorted by on-disk start,
// in MB with free-space gaps called out — for human inspection of how a resize
// rearranges the disk.
func gptDump(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("stat disk: %v", err)
	}
	diskMB := fi.Size() / MB
	d, err := diskfs.OpenBackend(file.New(f, true))
	if err != nil {
		t.Fatalf("open backend: %v", err)
	}
	tr, err := d.GetPartitionTable()
	if err != nil {
		t.Fatalf("get table: %v", err)
	}
	type row struct {
		idx         int
		name        string
		start, size int64
	}
	var rows []row
	for _, p := range tr.(*gpt.Table).Partitions {
		if p.Type == gpt.Unused {
			continue
		}
		rows = append(rows, row{p.Index, p.Name, p.GetStart(), int64(p.GetSize())})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].start < rows[j].start })

	var b strings.Builder
	fmt.Fprintf(&b, "    %-3s %-10s %10s %10s %10s\n", "#", "name", "startMB", "endMB", "sizeMB")
	var prevEndMB int64
	for _, r := range rows {
		startMB := r.start / MB
		endMB := (r.start + r.size) / MB
		sizeMB := r.size / MB
		if gap := startMB - prevEndMB; gap > 0 {
			fmt.Fprintf(&b, "    %-3s %-10s %10s %10s %10d  <- free\n", "", "(free)", "", "", gap)
		}
		fmt.Fprintf(&b, "    %-3d %-10s %10d %10d %10d\n", r.idx, r.name, startMB, endMB, sizeMB)
		prevEndMB = endMB
	}
	if tail := diskMB - prevEndMB; tail > 0 {
		fmt.Fprintf(&b, "    %-3s %-10s %10s %10s %10d  <- free\n", "", "(free)", "", "", tail)
	}
	return b.String()
}

// TestLayoutStagesDump logs the GPT partition table at the four stages of the resize
// scenario, by actually driving the resize. Run with -v to see the tables.
func TestLayoutStagesDump(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	t.Logf("Stage 1 - initial layout (disk %d MB):\n%s", defaultDiskMB, gptDump(t, fx.path))

	// Stage 2: Case 1 shrinks P9 in place, freeing space at the end
	if err := Run(fx.path, nil, shrinkP9, false, false, false); err != nil {
		t.Fatalf("Case 1 (shrink P9): %v", err)
	}
	t.Logf("Stage 2 - after P9 ext4 shrink (freed 600 MB at end):\n%s", gptDump(t, fx.path))

	// Stage 3: Case 2 up to and including copyFilesystems -- the *_resized2
	// partitions have been created in the freed space and filled, originals
	// still present
	runResizeStepsUpTo(t, fx.path, dummyShrink(), growImages, true, stepCopyFilesystems, false, false)
	t.Logf("Stage 3 - ESP/IMGA/IMGB_resized2 created and content copied (pre-updatePartitions):\n%s", gptDump(t, fx.path))

	// Stage 4: resume to completion -- the single updatePartitions relabels and
	// reindexes the *_resized2 partitions to the originals and removes the old
	if err := Run(fx.path, nil, growImages, false, false, true); err != nil {
		t.Fatalf("Case 2 resume to completion: %v", err)
	}
	t.Logf("Stage 4 - after updatePartitions (final):\n%s", gptDump(t, fx.path))
}
