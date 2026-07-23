package partitionresizer

import (
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

// TestChaosKillApplyCreate runs the real resizer binary doing an Apply that
// shrinks one partition, grows three, and creates a fourth (an empty FAT32 at a
// reserved number), and SIGKILLs it at random moments, re-running until it
// completes. It exercises resume of the create path -- including a kill after
// the filesystem is laid down by offset but before its GPT entry is published in
// the final write -- and asserts the final disk is correct and that the
// untouched partitions are never corrupted.
//
// Opt-in like TestChaosKill: set RESIZER_CHAOS=1, or CHAOS_GPT_DELAY (which also
// builds -tags chaos and delays around GPT-sector writes so kills land inside
// the single final table write).
func TestChaosKillApplyCreate(t *testing.T) {
	if testing.Short() || (os.Getenv("RESIZER_CHAOS") == "" && os.Getenv("CHAOS_GPT_DELAY") == "") {
		t.Skip("opt-in chaos soak test; set RESIZER_CHAOS=1 to run")
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not available")
	}

	bin := filepath.Join(t.TempDir(), "resizer")
	buildArgs := []string{"build", "-o", bin}
	gptDelay := os.Getenv("CHAOS_GPT_DELAY")
	if gptDelay != "" {
		buildArgs = append(buildArgs, "-tags", "chaos")
		t.Setenv("RESIZER_GPT_WRITE_DELAY", gptDelay)
	}
	buildArgs = append(buildArgs, "./cmd/resizer")
	if out, err := exec.Command("go", buildArgs...).CombinedOutput(); err != nil {
		t.Fatalf("build resizer: %v\n%s", err, out)
	}

	base := buildSampleLayout(t)
	before := gptByName(t, base.path)
	cfgLen := int(configMB * MB)
	cfgSum := partitionRegionSum(t, base.path, base.configStart, cfgLen)
	espType := string(gpt.EFISystemPartition)
	linType := string(gpt.LinuxFilesystem)

	applyArgs := []string{
		"--shrink", "uuid:" + before["P3"].GUID + ":300M",
		"--partition", "guid=" + before["EFI System"].GUID + ",minsize=96M,label=EFI System,type=" + espType + ",index=1",
		"--partition", "guid=" + before["IMGA"].GUID + ",minsize=200M,label=IMGA,type=" + linType + ",index=2",
		"--partition", "guid=" + before["IMGB"].GUID + ",minsize=200M,label=IMGB,type=" + linType + ",index=3",
		"--partition", "guid=" + espBGUID + ",minsize=20M,label=EFI System,type=" + espType + ",index=7,fs=fat32",
	}

	seed := time.Now().UnixNano()
	if s := os.Getenv("CHAOS_SEED"); s != "" {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			seed = v
		}
	}
	t.Logf("CHAOS_SEED=%d", seed)
	rng := rand.New(rand.NewSource(seed))
	trials := 5
	if gptDelay != "" {
		trials = 2
	}
	for trial := 0; trial < trials; trial++ {
		dir := t.TempDir()
		disk := filepath.Join(dir, "disk.img")
		if err := testCopyFile(base.path, disk); err != nil {
			t.Fatalf("copy base disk: %v", err)
		}
		scratch := filepath.Join(dir, "tmp")
		if err := os.MkdirAll(scratch, 0o755); err != nil {
			t.Fatalf("mkdir scratch: %v", err)
		}

		k := runPhaseWithRandomKills(t, bin, scratch, append(append([]string{}, applyArgs...), disk), rng)

		// ESP-B created at #7 as FAT32 with the reserved GUID.
		espb := readPartition(t, disk, 7)
		if !strings.EqualFold(espb.GUID, espBGUID) {
			t.Errorf("trial %d: ESP-B GUID = %s, want %s", trial, espb.GUID, espBGUID)
		}
		if espb.Name != "EFI System" || espb.GetSize() < 20*MB {
			t.Errorf("trial %d: ESP-B wrong: name=%q size=%d", trial, espb.Name, espb.GetSize())
		}
		if fs := getFilesystem(t, disk, 7); fs.Type() != filesystem.TypeFat32 {
			t.Errorf("trial %d: ESP-B fs = %v, want FAT32", trial, fs.Type())
		}
		// Grows preserved numbers, sizes and content; P3 shrunk; CONFIG intact.
		espa := readPartition(t, disk, 1)
		imga := readPartition(t, disk, 2)
		imgb := readPartition(t, disk, 3)
		p3 := readPartition(t, disk, p3Index)
		if espa.GetSize() < 96*MB || imga.GetSize() < 200*MB || imgb.GetSize() < 200*MB {
			t.Errorf("trial %d: grown sizes wrong: ESP=%d IMGA=%d IMGB=%d", trial, espa.GetSize(), imga.GetSize(), imgb.GetSize())
		}
		if partitionRegionSum(t, disk, imga.Start, 1*MB) != base.imgaSum ||
			partitionRegionSum(t, disk, imgb.Start, 1*MB) != base.imgbSum {
			t.Errorf("trial %d: IMG content not preserved", trial)
		}
		if int64(p3.GetSize()) != (defaultP3MB-600)*MB {
			t.Errorf("trial %d: P3 size = %d, want %d", trial, p3.GetSize(), (defaultP3MB-600)*MB)
		}
		if partitionRegionSum(t, disk, base.configStart, cfgLen) != cfgSum {
			t.Errorf("trial %d: CONFIG corrupted by interrupted apply", trial)
		}
		if got := readPartitionFile(t, disk, p3Index, "/p3-marker.txt"); got != "persist content" {
			t.Errorf("trial %d: P3 data lost: marker = %q", trial, got)
		}
		if got := readPartitionFile(t, disk, 1, "/esp-marker.txt"); got != "esp content" {
			t.Errorf("trial %d: ESP-A content lost: marker = %q", trial, got)
		}
		t.Logf("trial %d converged after %d kill(s)", trial, k)
	}
}
