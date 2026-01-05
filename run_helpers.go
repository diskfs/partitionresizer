package partitionresizer

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"

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
		return execResize2fs(device, newSizeMB, fixErrors)
	case disk.DeviceTypeFile:
		// copy the partition, then resize it, then copy it back into the original disk image
		tmpFile, err2 := os.CreateTemp("", "partresizer-shrinkfs-")
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
