package partitionresizer

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

func Run(disk string, shrinkPartition *PartitionIdentifier, growPartitions []PartitionChange, dryRun bool) error {
	// we always work solely with partition UUIDs internally, so convert any other identifiers to UUIDs
	// see if a disk was specified
	// no disk specified, try to discover
	var err error
	var partIdentifiers []PartitionIdentifier
	if shrinkPartition != nil {
		partIdentifiers = append(partIdentifiers, *shrinkPartition)
	}
	for _, gp := range growPartitions {
		partIdentifiers = append(partIdentifiers, gp)
	}
	disks, err := findDisks(disk)
	if err != nil {
		return fmt.Errorf("failed to find disks: %v", err)
	}
	matchedDisks, err := filterDisksByPartitions(disks, partIdentifiers)
	if err != nil {
		return fmt.Errorf("failed to filter disks by partiton: %v", err)
	}
	if len(matchedDisks) == 0 {
		return fmt.Errorf("no disks found matching specified partitions")
	}
	if len(matchedDisks) > 1 {
		return fmt.Errorf("multiple disks found matching specified partitions: %+v", matchedDisks)
	}
	disk = matchedDisks[0]
	diskPartitionData := disks[disk]
	log.Printf("Using disk: %s", disk)

	// now we have the desired disk, either passed explicitly or found by discovery

	// get a handle on the disk
	f, err := os.Open(disk)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	backend := file.New(f, true)
	d, err := diskfs.OpenBackend(backend)
	if err != nil {
		return err
	}

	// get the table and partition information
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		return err
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	parts := table.Partitions
	partitionResizeTargets, err := partitionChangesToResizeTarget(table, diskPartitionData, growPartitions)
	if err != nil {
		return err
	}

	// convert the growPartitions to original and target info, first without shrinking anything
	resizes, err := calculateResizes(d.Size, parts, partitionResizeTargets)
	if err != nil {
		if !errors.Is(err, &InsufficientSpaceError{}) {
			return err
		}
		// next try shrinking the shrink partition
		if shrinkPartition == nil {
			return fmt.Errorf("insufficient space to perform requested partition grows, and no shrink partition specified")
		}
		// find the total space we need
		var totalGrow int64
		for _, gp := range partitionResizeTargets {
			totalGrow += gp.target.size
		}
		// round up the space to grow to a multiple of GB
		gb := int64(1024 * 1024 * 1024)
		if totalGrow%gb != 0 {
			totalGrow = ((totalGrow / gb) + 1) * gb
		}

		// find the shrink partition
		shrinkData, err := partitionIdentifiersToData(table, diskPartitionData, []PartitionIdentifier{*shrinkPartition})
		if err != nil {
			return err
		}
		if len(shrinkData) != 1 {
			return fmt.Errorf("could not find shrink partition data")
		}
		// try to get the filesystem for the shrink partition, see if we can shrink it
		fs, err := d.GetFilesystem(shrinkData[0].number)
		if err != nil {
			return fmt.Errorf("failed to get filesystem for shrink partition: %v", err)
		}
		if fs.Type() != filesystem.TypeExt4 {
			return fmt.Errorf("unsupported filesystem type for shrinking: %v", fs.Type())
		}
		// try to shrink the filesystem
		newSize := shrinkData[0].size - totalGrow
		// convert it into MB
		newSizeMB := newSize / (1024 * 1024)
		log.Printf("Resizing filesystem on partition %d to %d MB to make space for growing other partitions", shrinkData[0].number, newSizeMB)
		partDevice := filepath.Join("/dev", shrinkData[0].name)

		cmd := exec.Command("resize2fs", partDevice, fmt.Sprintf("%dM", newSizeMB))
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("failed to run resize2fs on %s: %w", partDevice, err)
		}
		// now update the partition table to reflect the smaller partition
		log.Printf("Resizing partition %d to %d bytes", shrinkData[0].number, newSize)
		parts[shrinkData[0].number].Size = uint64(newSize)
		if err := d.Partition(table); err != nil {
			return fmt.Errorf("failed to write partition table after shrinking: %v", err)
		}
		resizes, err = calculateResizes(d.Size, table.Partitions, partitionResizeTargets)
		if err != nil {
			return fmt.Errorf("failed to calculate resizes after shrinking: %v", err)
		}
	}
	// if dryRun, do nothing
	if dryRun {
		log.Printf("Dry run specified, not performing resizes %+v", resizes)
		return nil
	}
	log.Printf("Will perform resizes %+v", resizes)
	// perform the resizes
	if err := resize(resizes, d, dryRun); err != nil {
		log.Fatalf("Failed to perform resizes: %v", err)
	}
	log.Printf("Resizing completed successfully")
	return nil
}
