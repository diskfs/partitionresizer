package partitionresizer

import (
	"fmt"
	"log"

	"github.com/diskfs/go-diskfs"
	"github.com/diskfs/go-diskfs/backend/file"
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
	disks, err := findDisks(disk, "")
	if err != nil {
		return fmt.Errorf("failed to find disks: %v", err)
	}
	filteredDisks, err := filterDisksByPartitions(disks, partIdentifiers)
	if err != nil {
		return fmt.Errorf("failed to filter disks by partiton: %v", err)
	}
	if len(filteredDisks) == 0 {
		return fmt.Errorf("no disks found matching specified partitions")
	}
	if len(filteredDisks) > 1 {
		return fmt.Errorf("multiple disks found matching specified partitions: %+v", filteredDisks)
	}
	matchedDisk := filteredDisks[0]
	diskPartitionData := disks[matchedDisk]
	log.Printf("Using disk: %s via path %s", matchedDisk, disk)

	// now we have the desired disk, either passed explicitly or found by discovery

	backend, err := file.OpenFromPath(disk, false)
	if err != nil {
		return err
	}
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
	// plan what changes we will make
	resizes, err := planResizes(d, table, diskPartitionData, growPartitions, shrinkPartition)
	if err != nil {
		return err
	}
	if dryRun {
		log.Printf("Dry run specified, not performing resizes %+v", resizes)
		return nil
	}
	log.Printf("Will perform resizes %+v", resizes)
	return resize(d, resizes)
}
