package partitionresizer

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
)

// wholeDiskSum streams a sha256 over the entire disk image.
func wholeDiskSum(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash disk: %v", err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// --- #3: untouched partitions stay byte-identical ----------------------------

// TestUntouchedPartitionsByteIntegrity verifies that the partitions a grow
// must not touch -- persist (P3, user data) and CONFIG -- are byte-for-byte
// unchanged across Case 2, and that P3's filesystem data survives.
func TestUntouchedPartitionsByteIntegrity(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	if err := Apply(fx.path, nil, shrinkP3, false, false); err != nil {
		t.Fatalf("Case 1 (shrink P3): %v", err)
	}
	p3Len := int((defaultP3MB - 600) * MB)
	cfgLen := int(configMB * MB)
	p3Before := partitionRegionSum(t, fx.path, fx.p3Start, p3Len)
	cfgBefore := partitionRegionSum(t, fx.path, fx.configStart, cfgLen)

	if err := Apply(fx.path, growImages, nil, false, false); err != nil {
		t.Fatalf("Case 2 (grow): %v", err)
	}

	if partitionRegionSum(t, fx.path, fx.p3Start, p3Len) != p3Before {
		t.Error("P3 (persist) region changed during the grow")
	}
	if partitionRegionSum(t, fx.path, fx.configStart, cfgLen) != cfgBefore {
		t.Error("CONFIG region changed during the grow")
	}
	if got := readPartitionFile(t, fx.path, p3Index, "/p3-marker.txt"); got != "persist content" {
		t.Errorf("P3 data lost: marker = %q", got)
	}
}

// --- #4: a failure leaves the disk unmodified --------------------------------

// TestInsufficientSpaceIsAtomic verifies that a grow which cannot fit (no
// free space and no shrink partition) fails without modifying the disk, so a
// reboot inherits the original state, not a half-applied one.
func TestInsufficientSpaceIsAtomic(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	before := wholeDiskSum(t, fx.path)

	// full disk, grows that need ~496 MB, no shrink partition supplied
	err := Apply(fx.path, growImages, nil, false, false)
	if err == nil {
		t.Fatal("expected an insufficient-space error, got nil")
	}
	if wholeDiskSum(t, fx.path) != before {
		t.Error("disk was modified despite the planning failure (not atomic)")
	}
	t.Logf("aborted cleanly: %v", err)
}

// --- #5: refuse to shrink below the filesystem's used size -------------------

// TestShrinkBelowUsedAborts verifies that asking to shrink P3 below its ext4
// used size makes e2fsck/resize2fs refuse, and the resize aborts with the
// partition and its data untouched.
func TestShrinkBelowUsedAborts(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	p3Before := partitionRegionSum(t, fx.path, fx.p3Start, int(defaultP3MB*MB))

	// 16 MB is well below P3's used size; resize2fs must refuse
	err := Apply(fx.path, nil, shrinkToSizeLabel("P3", 16*MB), false, false)
	if err == nil {
		t.Fatal("expected resize2fs to refuse shrinking below used size, got nil")
	}
	after := gptByName(t, fx.path)
	if got := int64(after["P3"].GetSize()); got != defaultP3MB*MB {
		t.Errorf("P3 size changed to %d despite the failed shrink (want %d)", got, defaultP3MB*MB)
	}
	if partitionRegionSum(t, fx.path, fx.p3Start, int(defaultP3MB*MB)) != p3Before {
		t.Error("P3 region changed despite the failed shrink")
	}
	if got := readPartitionFile(t, fx.path, p3Index, "/p3-marker.txt"); got != "persist content" {
		t.Errorf("P3 data lost: marker = %q", got)
	}
	t.Logf("aborted cleanly: %v", err)
}

// --- #6: combined shrink + grow in a single Run ------------------------------

// TestCombinedShrinkGrow exercises the natural one-shot usage: a single
// Run that shrinks P3 to make room and grows ESP/IMGA/IMGB. go-diskfs rounds
// the shrink up to a whole GB, so this uses a larger P3 (1400 MB) than the
// default fixture.
func TestCombinedShrinkGrow(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	const p3MB = 1400
	const diskMB = p3MB + 252 // front partitions (~250 MB) + margin
	fx := buildSampleLayoutSized(t, diskMB, p3MB)

	if err := Apply(fx.path, growImages, shrinkToFitLabel("P3"), false, false); err != nil {
		t.Fatalf("combined shrink+grow Apply: %v", err)
	}

	after := gptByName(t, fx.path)
	// P3 shrunk by the GB-rounded total grow (496 MB -> 1 GB)
	if got := int64(after["P3"].GetSize()); got != (p3MB-1024)*MB {
		t.Errorf("P3 size = %d, want %d", got, (p3MB-1024)*MB)
	}
	if after["EFI System"].Index != espIndex || after["IMGA"].Index != imgaIndex || after["IMGB"].Index != imgbIndex {
		t.Errorf("indices not preserved: ESP=%d IMGA=%d IMGB=%d", after["EFI System"].Index, after["IMGA"].Index, after["IMGB"].Index)
	}
	if after["IMGA"].Start <= after["P3"].Start {
		t.Errorf("IMGA (%d) not relocated past P3 (%d)", after["IMGA"].Start, after["P3"].Start)
	}
	if int64(after["EFI System"].GetSize()) != 96*MB || int64(after["IMGA"].GetSize()) != 200*MB || int64(after["IMGB"].GetSize()) != 200*MB {
		t.Errorf("grown sizes wrong: ESP=%d IMGA=%d IMGB=%d", after["EFI System"].GetSize(), after["IMGA"].GetSize(), after["IMGB"].GetSize())
	}
	if got := partitionRegionSum(t, fx.path, after["IMGA"].Start, 1*MB); got != fx.imgaSum {
		t.Error("IMGA content not preserved")
	}
}

// --- #7: realistic P3 content survives the shrink ----------------------------

// TestShrinkPreservesP3Content fills P3 with many files (so resize2fs has
// real data to relocate during the shrink), shrinks it, then verifies every
// file is intact.
func TestShrinkPreservesP3Content(t *testing.T) {
	if testing.Short() {
		t.Skip("slow end-to-end resize test")
	}
	fx := buildSampleLayout(t)
	want := populateP3(t, fx.path, 30)

	// shrink P3 from 900 MB to 200 MB (above used; forces block relocation)
	if err := Apply(fx.path, nil, shrinkToSizeLabel("P3", 200*MB), false, false); err != nil {
		t.Fatalf("shrink P3: %v", err)
	}
	after := gptByName(t, fx.path)
	if got := int64(after["P3"].GetSize()); got != 200*MB {
		t.Errorf("P3 size = %d, want %d", got, 200*MB)
	}
	for name, content := range want {
		if got := readPartitionFile(t, fx.path, p3Index, name); got != content {
			t.Errorf("P3 file %s content mismatch after shrink (len got=%d want=%d)", name, len(got), len(content))
		}
	}
}

// populateP3 writes n distinct files into the P3 filesystem and returns their
// expected contents.
func populateP3(t *testing.T, path string, n int) map[string]string {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	defer func() { _ = f.Close() }()
	d, err := diskfs.OpenBackend(file.New(f, false), diskfs.WithOpenMode(diskfs.ReadWrite))
	if err != nil {
		t.Fatalf("open disk: %v", err)
	}
	fs, err := d.GetFilesystem(p3Index)
	if err != nil {
		t.Fatalf("get P3 filesystem: %v", err)
	}
	want := make(map[string]string, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("/p3file-%02d.dat", i)
		// a few KB of deterministic, per-file content
		content := fmt.Sprintf("file %d:", i)
		for len(content) < 4096 {
			content += fmt.Sprintf("%d-%x;", i, len(content))
		}
		fh, err := fs.OpenFile(name, os.O_CREATE|os.O_RDWR)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fh.Write([]byte(content)); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := fh.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
		want[name] = content
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	return want
}

// --- #1 + #2: SIGKILL chaos --------------------------------------------------

// TestChaosKill runs the real resizer binary doing the sample-layout grow and SIGKILLs
// it at random moments, re-running until it completes. This exercises resume
// from arbitrary interruption points -- including partial GPT writes and
// half-copied partition content -- across repeated kills, and asserts that the
// final disk is correct and that persist/CONFIG are never corrupted.
//
// SIGKILL models a process crash (writes already issued survive); it is not a
// full power-loss model (no torn sectors / lost un-fsync'd writes).
func TestChaosKill(t *testing.T) {
	// Opt-in soak/stress test, not a CI gate: it runs many full resizes under
	// random SIGKILLs and is meant to run for minutes-to-hours, so `go test
	// ./...` (and -short) skip it by default. Enable with RESIZER_CHAOS=1, or
	// with CHAOS_GPT_DELAY (which additionally turns on the GPT-write-delay hook).
	if testing.Short() || (os.Getenv("RESIZER_CHAOS") == "" && os.Getenv("CHAOS_GPT_DELAY") == "") {
		t.Skip("opt-in chaos soak test; set RESIZER_CHAOS=1 to run")
	}
	if _, err := exec.LookPath("mksquashfs"); err != nil {
		t.Skip("mksquashfs not available")
	}
	// build the resizer binary. If CHAOS_GPT_DELAY is set, build with -tags
	// chaos and have the resizer delay around GPT-sector writes, so kills land
	// between the backup/primary GPT writes (e.g. inside updatePartitions, a
	// single fast table write that random-timed kills otherwise never catch).
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

	// fresh base (NOT pre-shrunk): the chaos performs the full two-step
	// resize -- shrink P3, then grow ESP/IMGA/IMGB -- so kills can land in the
	// shrink steps (resize2fs) as well as create/copy/update.
	base := buildSampleLayout(t)
	cfgLen := int(configMB * MB)
	cfgSum := partitionRegionSum(t, base.path, base.configStart, cfgLen)

	shrinkArgs := []string{"--shrink", "label:P3:300M"} // Case 1: shrink P3 in place
	growArgs := []string{                               // Case 2: grow into the freed space
		"--partition", "match=label:EFI System,minsize=96M",
		"--partition", "match=label:IMGA,minsize=200M",
		"--partition", "match=label:IMGB,minsize=200M",
	}
	// Seed from CHAOS_SEED if set (so a failing run is reproducible), else from
	// the clock so repeated runs explore different kill timings.
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
		trials = 2 // GPT-write delays make each trial much slower
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

		// Phase 1: shrink P3, then Phase 2: grow -- each driven through random
		// SIGKILLs and re-run to completion.
		ks := runPhaseWithRandomKills(t, bin, scratch, append(append([]string{}, shrinkArgs...), disk), rng)
		kg := runPhaseWithRandomKills(t, bin, scratch, append(append([]string{}, growArgs...), disk), rng)

		after := gptByName(t, disk)
		if got := int64(after["P3"].GetSize()); got != (defaultP3MB-600)*MB {
			t.Errorf("trial %d: P3 size = %d, want %d", trial, got, (defaultP3MB-600)*MB)
		}
		if after["EFI System"].Index != espIndex || after["IMGA"].Index != imgaIndex || after["IMGB"].Index != imgbIndex {
			t.Errorf("trial %d: indices not preserved", trial)
		}
		if int64(after["IMGA"].GetSize()) != 200*MB || int64(after["IMGB"].GetSize()) != 200*MB || int64(after["EFI System"].GetSize()) != 96*MB {
			t.Errorf("trial %d: grown sizes wrong", trial)
		}
		if partitionRegionSum(t, disk, after["IMGA"].Start, 1*MB) != base.imgaSum ||
			partitionRegionSum(t, disk, after["IMGB"].Start, 1*MB) != base.imgbSum {
			t.Errorf("trial %d: IMG content not preserved", trial)
		}
		if partitionRegionSum(t, disk, base.configStart, cfgLen) != cfgSum {
			t.Errorf("trial %d: CONFIG corrupted by interrupted resize", trial)
		}
		if got := readPartitionFile(t, disk, p3Index, "/p3-marker.txt"); got != "persist content" {
			t.Errorf("trial %d: P3 data lost: marker = %q", trial, got)
		}
		t.Logf("trial %d converged: %d shrink-kill(s), %d grow-kill(s)", trial, ks, kg)
	}
}

// runPhaseWithRandomKills spawns the resizer for one phase, SIGKILLs its whole
// process group after a random delay (so any resize2fs child dies too), and
// re-runs until it completes; returns the number of kills. TMPDIR is pointed at
// scratch and cleaned after each kill so a killed shrink can't leak its ~GB temp
// copy. A non-zero exit that we did NOT cause is a real failure (resume must
// always be able to finish).
func runPhaseWithRandomKills(t *testing.T, bin, scratch string, args []string, rng *rand.Rand) int {
	t.Helper()
	// SIGKILL a random number of times at varied points, then run once
	// un-killed; that final run must complete (the idempotency assertion).
	killsWanted := rng.Intn(4) + 1
	kills := 0
	for attempt := 0; attempt < 80; attempt++ {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "TMPDIR="+scratch)
		// own process group, so a SIGKILL can take down any resize2fs child too
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatalf("start resizer: %v", err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		if kills >= killsWanted {
			// guaranteed completion run
			if werr := <-done; werr != nil {
				t.Fatalf("resume run failed after %d kill(s) (resume not safe): %v\nstderr:\n%s", kills, werr, stderr.String())
			}
			return kills
		}
		// Spread the kill across the operation. With GPT-write delays active the
		// pipeline takes tens of seconds, so widen the range; otherwise early
		// kills only ever land in the first (delayed) step and never reach
		// updatePartitions.
		maxKillMs := 2500
		if os.Getenv("CHAOS_GPT_DELAY") != "" {
			maxKillMs = 60000
		}
		delay := time.Duration(rng.Intn(maxKillMs)+50) * time.Millisecond
		select {
		case werr := <-done:
			if werr == nil {
				return kills // finished before we killed it
			}
			t.Fatalf("resizer exited with error without being killed (resume not safe): %v\nstderr:\n%s", werr, stderr.String())
		case <-time.After(delay):
			// kill the whole process group (negative pid) so resize2fs dies too
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
			kills++
			// record how far the killed run got, so the long run can show
			// whether kills land at every intermediate pipeline step
			step := chaosStepFromLog(stderr.String())
			// Inspect the on-disk GPT immediately after the kill, before the next
			// resume run heals it. A mid-updatePartitions kill can leave the
			// primary and backup tables (each header + 32 entry sectors)
			// disagreeing, or an entry array half-written so its CRC fails; this
			// records exactly that. The disk image is the last positional arg.
			diskPath := args[len(args)-1]
			t.Logf("KILL_STEP=%s %s", step, gptIntegrity(diskPath))
			// Optional (CHAOS_COPY_STATE): also classify each grow target's data
			// region so a copyFilesystems kill is shown as empty/partial/complete,
			// not just inferred from the step marker. Off by default.
			if os.Getenv("CHAOS_COPY_STATE") != "" {
				t.Logf("KILL_STEP=%s %s", step, copyDestState(diskPath))
			}
			// drop any partial resize2fs temp / create-filesystem temp the killed
			// run left behind
			for _, pat := range []string{"partresizer-shrinkfs-*", "createfs*"} {
				matches, _ := filepath.Glob(filepath.Join(scratch, pat))
				for _, m := range matches {
					_ = os.RemoveAll(m)
				}
			}
		}
	}
	t.Fatalf("resize did not converge within the attempt cap")
	return kills
}

// chaosStepFromLog reports the furthest pipeline step the resizer had reached,
// from its log output, so we can see where a SIGKILL interrupted it. Steps are
// checked in pipeline order; the last one present is the furthest reached.
func chaosStepFromLog(s string) string {
	markers := []struct{ sub, step string }{
		{"Resizing filesystem on partition", "shrinkFilesystems"},
		{"Resizing partition", "shrinkPartitions"},
		{"creating new partition", "createPartitions"},
		{"copying data from", "copyFilesystems"},
		{"finalizing partition", "updatePartitions"},
	}
	step := "preflight"
	for _, m := range markers {
		if strings.Contains(s, m.sub) {
			step = m.step
		}
	}
	return step
}
