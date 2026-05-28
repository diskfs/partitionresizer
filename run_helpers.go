package partitionresizer

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

const (
	partTmpFilename = "partresizer-shrinkfs-XXXXXXXX"
)

// execResize2fs is the function used to invoke resize2fs. partDevice may be a block device pointing to the actual
// filesystem partition, or an image file with the filesystem at byte 0.
var execResize2fs = func(partDevice string, newSizeMB int64, fixErrors bool) error {
	fixFlag := "-n"
	if fixErrors {
		fixFlag = "-y"
	}
	cmd := exec.Command("e2fsck", "-f", fixFlag, partDevice)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("e2fsck failed: %w", err)
	}

	cmd = exec.Command("resize2fs", partDevice, fmt.Sprintf("%dM", newSizeMB))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("resize2fs failed: %w", err)
	}
	return nil
}

// resizeFilesystem resizes an ext4 filesystem, given a full path to the device and partition data
// Should account for it being a disk image with multiple partitions if needed, i.e. not just an entire disk,
// using the information in filesystemData.
// filesystemData is expected to be the *current* partition data, i.e. before resizing,
// while delta is the expected delta in size.
func resizeFilesystem(
	device string,
	filesystemData partitionData,
	delta int64,
	fixErrors bool,
) error {
	newSize := filesystemData.size + delta
	newSizeMB := newSize / (1024 * 1024)
	log.Printf(
		"Resizing filesystem on partition %d to %d MB",
		filesystemData.number, newSizeMB,
	)
	f, err := os.Open(device)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	deviceType, err := disk.DetermineDeviceType(f)
	if err != nil {
		return err
	}
	switch deviceType {
	case disk.DeviceTypeBlockDevice:
		// resize2fs takes the *partition* device, not the whole-disk
		// device, so we resolve "/dev/sda" + partition number 9 to
		// "/dev/sda9" (or whatever the kernel calls that slot —
		// "/dev/nvme0n1p9", "/dev/mmcblk0p9", etc.) via sysfs.
		partDevice, err := partitionDevicePath(device, filesystemData.number, "")
		if err != nil {
			return fmt.Errorf("cannot find partition device for %s partition %d: %w", device, filesystemData.number, err)
		}
		return execResize2fs(partDevice, newSizeMB, fixErrors)
	case disk.DeviceTypeFile:
		// copy the partition, then resize it, then copy it back into the original disk image
		tmpFile, err2 := os.CreateTemp("", partTmpFilename)
		if err2 != nil {
			return err2
		}
		_ = tmpFile.Close()
		defer func() {
			_ = os.RemoveAll(tmpFile.Name())
		}()
		// copy the file over
		if err = CopyRange(device, tmpFile.Name(), filesystemData.start, 0, filesystemData.size, 0); err != nil {
			return fmt.Errorf("copy to temp file: %w", err)
		}
		if err = execResize2fs(tmpFile.Name(), newSizeMB, fixErrors); err != nil {
			return err
		}
		err = CopyRange(tmpFile.Name(), device, 0, filesystemData.start, newSize, 0)
	case disk.DeviceTypeUnknown:
		err = fmt.Errorf("unknown device type for %s", device)
	}
	return err
}

// planResizes computes the resize plan, including both growing the relevant partitions as well as
// optionally performing an ext4 shrink, if there is insufficient space initially.
// Returns the final plan or an error.
func planResizes(
	d *disk.Disk,
	table *gpt.Table,
	diskPartitionData []partitionData,
	growPartitions []PartitionChange,
	shrinkPartition *PartitionIdentifier,
) (
	[]partitionResizeTarget,
	error,
) {
	// map PartitionChange to partitionResizeTarget
	prTargets, err := partitionChangesToResizeTarget(table, diskPartitionData, growPartitions)
	if err != nil {
		return nil, err
	}

	// try to calculate without shrinking
	resizes, err := calculateResizes(d.Size, table.Partitions, prTargets)
	if err == nil {
		return resizes, nil
	}
	var spaceErr *InsufficientSpaceError
	if !errors.As(err, &spaceErr) {
		return nil, err
	}

	// need to shrink: ensure shrinkPartition provided
	if shrinkPartition == nil {
		return nil, fmt.Errorf("insufficient space to perform requested partition grows, and no shrink partition specified")
	}

	// compute total space to grow (rounded up to next GB)
	var totalGrow int64
	for _, gp := range prTargets {
		totalGrow += gp.target.size
	}
	if totalGrow%GB != 0 {
		totalGrow = ((totalGrow / GB) + 1) * GB
	}

	// locate shrink partition data
	shrinkDataList, err := partitionIdentifiersToData(table, diskPartitionData, []PartitionIdentifier{*shrinkPartition})
	if err != nil {
		return nil, err
	}
	if len(shrinkDataList) != 1 {
		return nil, fmt.Errorf("could not find shrink partition data")
	}
	shrinkData := shrinkDataList[0]

	// mark the shrink as first for the resize
	target := shrinkData
	target.size = shrinkData.size - totalGrow
	target.end = shrinkData.end - totalGrow
	shrink := partitionResizeTarget{
		original: shrinkData,
		target:   target,
	}
	prTargetsWithShrink := []partitionResizeTarget{shrink}
	prTargetsWithShrink = append(prTargetsWithShrink, prTargets...)

	// recalculate resizes with shrinking
	return calculateResizes(d.Size, table.Partitions, prTargetsWithShrink)
}

// partitionDevicePath maps a whole-disk path (e.g. "/dev/sda") and a
// partition number to the partition's device path (e.g. "/dev/sda9",
// "/dev/nvme0n1p9", "/dev/mmcblk0p9").
//
// Naming conventions for partition device nodes differ by disk type,
// so we look up the kernel partition name via sysfs rather than
// hardcoding the convention: each /sys/class/block/<disk>/<part>/
// directory holds a "partition" file containing the partition number
// and a directory named after the kernel partition name.
//
// If syspath is empty, /sys is used. Returns an error if no matching
// partition is found under sysfs.
func partitionDevicePath(diskPath string, partNumber int, syspath string) (string, error) {
	if syspath == "" {
		syspath = sysDefaultPath
	}
	diskBase := filepath.Base(diskPath)
	diskSysDir := filepath.Join(syspath, "class", "block", diskBase)
	entries, err := os.ReadDir(diskSysDir)
	if err != nil {
		return "", fmt.Errorf("read sysfs dir %s: %w", diskSysDir, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		partFile := filepath.Join(diskSysDir, e.Name(), "partition")
		raw, err := os.ReadFile(partFile)
		if err != nil {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			continue
		}
		if n == partNumber {
			return filepath.Join("/dev", e.Name()), nil
		}
	}
	return "", fmt.Errorf("partition %d not found under %s", partNumber, diskSysDir)
}
