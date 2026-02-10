package partitionresizer

import (
	"errors"
	"fmt"
	"log"

	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/diskfs/go-diskfs/sync"
)

type copyData struct {
	count int64
	err   error
}

// resize performs the actual resize operations on the given disk
func resize(d *disk.Disk, resizes []partitionResizeTarget, fixErrors bool) error {
	// do any shrinks first
	// this is idempotent. If I have a 500MB partition with a 500MB filesystem,
	// and shrink it to 400MB. If I stop, and then run it again, it will just say
	// it already is 400MB and move on.
	if err := shrinkFilesystems(d, resizes, fixErrors); err != nil {
		return err
	}
	// next shrink partitions
	// This is idempotent as well. I tell the GPT partition table what size
	// I want, and it will just set it again if it's already that size.
	if err := shrinkPartitions(d, resizes); err != nil {
		return err
	}

	// next create new partitions
	// It is important that they have different UUID, Type GUID, and predictable
	// but different names, so that we can identify them later for copying data.
	// Should it stop and then reboot, we want the original partitions to still be there.
	// They should have their original UUID and Label, so there is no conflict.
	// We also want the new partitions to have unique Type GUIDs and Names,
	// in case something relies on that to boot. For example, EFI System Partition.
	if err := createPartitions(d, resizes); err != nil {
		return err
	}

	// next copy filesystems
	// After the copy is done, verify the contents.
	if err := copyFilesystems(d, resizes); err != nil {
		return err
	}

	// swap the partitions, specifically the labels, Type GUIDs, and UUIDs, as well as attributes flags
	if err := swapPartitions(d, resizes); err != nil {
		return err
	}

	// remove the old partitions
	if err := removePartitions(d, resizes); err != nil {
		return err
	}

	return nil
}

// createPartitions creates new partitions as per the resize targets, taking
// all of the characteristics from the original partitions except for start/end/size.
func createPartitions(d *disk.Disk, resizes []partitionResizeTarget) error {
	// first create the new partitions in the partition table and write it
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		return err
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	partitions := table.Partitions
	indexMap := map[int]*gpt.Partition{}
	for _, p := range partitions {
		indexMap[p.Index] = p
	}
	labelMap := map[string]bool{}
	for _, p := range partitions {
		labelMap[p.Name] = true
	}
	for _, r := range resizes {
		// no change in start, just copy over, it already was handled
		if r.original.start == r.target.start {
			log.Printf("partition %d %s: no location change, no need to create additional partition", r.original.number, r.original.label)
			continue
		}
		log.Printf("creating new partition %s: original %+v, target %+v", r.original.label, r.original, r.target)
		// get existing partition info
		p, ok := indexMap[r.original.number]
		if !ok {
			return fmt.Errorf("original partition %d not found in partition table", r.original.number)
		}
		altName := getAlternateLabel(p.Name)
		// see if it already exists
		if labelMap[altName] {
			log.Printf("alternate partition name %s already exists, assuming partition was already created", altName)
			continue
		}
		// create the new partition
		newPart := gpt.Partition{
			Start:      uint64(r.target.start / int64(table.LogicalSectorSize)),
			Size:       uint64(r.target.size),
			Type:       gpt.LinuxFilesystem, // set to Linux Filesystem type to avoid conflicts
			Name:       altName,
			Attributes: p.Attributes,
			Index:      r.target.number,
			// explicitly leave GUID blank so it autogenerates a new one
		}
		partitions = append(partitions, &newPart)
	}
	// write the updated partition table; we rely on the GPT implementation to sort out the ordering
	table.Partitions = partitions
	if err := d.Partition(table); err != nil {
		return fmt.Errorf("failed to write updated partition table: %v", err)
	}
	return nil
}

func copyFilesystems(d *disk.Disk, resizes []partitionResizeTarget) error {
	// it depends on the filesystem type:
	// - squashfs, ext4, unknown: raw data copy
	// - fat32: use filesystem copy
	for _, r := range resizes {
		if r.original.start == r.target.start {
			log.Printf("partition %d %s: no location change, no need to copy filesystem", r.original.number, r.original.label)
			continue
		}
		log.Printf("copying data from original partition %d to new partition %d", r.original.number, r.target.number)
		fs, err := d.GetFilesystem(r.original.number)
		switch {
		case err != nil && !errors.Is(err, &disk.UnknownFilesystemError{}):
			return fmt.Errorf("failed to get filesystem for partition %s: %v", r.original.label, err)
		case err != nil || fs.Type() == filesystem.TypeSquashfs:
			log.Printf("partition %d -> %d: performing raw data copy for filesystem type %v", r.original.number, r.target.number, fs.Type())
			if err := sync.CopyPartitionRaw(d, r.original.number, r.target.number); err != nil {
				return fmt.Errorf("failed to copy raw data for partition %s: %v", r.original.label, err)
			}
		case fs.Type() == filesystem.TypeExt4:
			newFS, err := d.CreateFilesystem(disk.FilesystemSpec{
				Partition:   r.target.number,
				FSType:      filesystem.TypeExt4,
				VolumeLabel: fs.Label(),
			})
			if err != nil {
				return fmt.Errorf("failed to create ext4 filesystem for new partition %s: %v", r.original.label, err)
			}
			// use filesystem copy
			if err := sync.CopyFileSystem(fs, newFS); err != nil {
				return fmt.Errorf("failed to copy ext4 filesystem data for partition %s: %v", r.original.label, err)
			}
			if err := sync.CompareFS(fs, newFS); err != nil {
				return fmt.Errorf("verification failed for partition %s: %v", r.original.label, err)
			}
			log.Printf("partition %d -> %d: filesystem %v copy verified", r.original.number, r.target.number, fs.Type())
		case fs.Type() == filesystem.TypeFat32:
			// create a new filesystem on the new partition
			newFS, err := d.CreateFilesystem(disk.FilesystemSpec{
				Partition:   r.target.number,
				FSType:      filesystem.TypeFat32,
				VolumeLabel: fs.Label(),
			})
			if err != nil {
				return fmt.Errorf("failed to create FAT32 filesystem for new partition %s: %v", r.original.label, err)
			}
			// use filesystem copy
			if err := sync.CopyFileSystem(fs, newFS); err != nil {
				return fmt.Errorf("failed to copy FAT32 filesystem data for partition %s: %v", r.original.label, err)
			}
			log.Printf("partition %d -> %d: filesystem %v copied file content", r.original.number, r.target.number, fs.Type())
			if err := sync.CompareFS(fs, newFS); err != nil {
				return fmt.Errorf("verification failed for partition %s: %v", r.original.label, err)
			}
			log.Printf("partition %d -> %d: filesystem %v copy verified", r.original.number, r.target.number, fs.Type())
		default:
			return fmt.Errorf("unsupported filesystem type %v for partition %s", fs.Type(), r.original.label)
		}

	}
	return nil
}

// remove partitions removes the original partitions after data has been copied
func removePartitions(d *disk.Disk, resizes []partitionResizeTarget) error {
	// first create the new partitions in the partition table and write it
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		return err
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	toRemove := make(map[int]bool)
	for _, r := range resizes {
		if r.original.number == r.target.number {
			log.Printf("partition %d %s: no change in partition number, no need to remove old partition", r.original.number, r.original.label)
			continue
		}
		log.Printf("removing old partition %d", r.original.number)
		// mark this partition for removal
		toRemove[r.original.number] = true
	}
	// remove any marked for removal
	for _, p := range table.Partitions {
		if toRemove[p.Index] {
			log.Printf("removing partition %d from partition table", p.Index)
			p.Type = gpt.Unused
		}
	}
	// write the updated partition table
	if err := d.Partition(table); err != nil {
		return fmt.Errorf("failed to write updated partition table: %v", err)
	}
	return nil
}

// swapPartitions swaps the labels, Type GUIDs, and UUIDs of the original and target partitions,
// as well as any attributes flags.
func swapPartitions(d *disk.Disk, resizes []partitionResizeTarget) error {
	// first create the new partitions in the partition table and write it
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		return err
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	indexToPosition := make(map[int]int)
	for i, p := range table.Partitions {
		indexToPosition[p.Index] = i
	}
	for _, r := range resizes {
		if r.original.number == r.target.number {
			log.Printf("partition %d %s: no change in partition number, no need to swap partitions", r.original.number, r.original.label)
			continue
		}
		log.Printf("swapping values on partitions original %d -> %d ", r.original.number, r.target.number)
		// mark this partition for removal
		original := table.Partitions[indexToPosition[r.original.number]]
		target := table.Partitions[indexToPosition[r.target.number]]
		originalName := original.Name
		originalType := original.Type
		originalGUID := original.GUID
		originalAttributes := original.Attributes

		// swap values
		original.Name = target.Name
		original.Type = target.Type
		original.GUID = target.GUID
		original.Attributes = target.Attributes

		target.Name = originalName
		target.Type = originalType
		target.GUID = originalGUID
		target.Attributes = originalAttributes
	}
	// write the updated partition table
	if err := d.Partition(table); err != nil {
		return fmt.Errorf("failed to write updated partition table: %v", err)
	}
	return nil
}

func shrinkFilesystems(d *disk.Disk, resizes []partitionResizeTarget, fixErrors bool) error {
	for _, r := range resizes {
		if r.original.size <= r.target.size {
			log.Printf("filesystem on partition %d does not require shrinking, skipping", r.original.number)
			continue
		}
		log.Printf("shrinking filesystem on partition %d label '%s' from %d to %d bytes / %d to %d MB", r.original.number, r.original.label, r.original.size, r.target.size, r.original.size/MB, r.target.size/MB)
		// verify ext4 fs on shrink partition
		fs, err := d.GetFilesystem(r.original.number)
		if err != nil {
			return fmt.Errorf("failed to get filesystem for shrink partition: %v", err)
		}
		if fs.Type() != filesystem.TypeExt4 {
			return fmt.Errorf("unsupported filesystem type for shrinking: %v", fs.Type())
		}

		// perform the shrink
		// note that resize will leave it alone if it already is the desired size
		p := d.Backend.Path()
		if p == "" {
			return fmt.Errorf("cannot shrink filesystem: disk backend has no path")
		}
		delta := r.target.size - r.original.size
		if err := resizeFilesystem(p, r.original, delta, fixErrors); err != nil {
			return err
		}
	}
	return nil
}

func shrinkPartitions(d *disk.Disk, resizes []partitionResizeTarget) error {
	table, ok := d.Table.(*gpt.Table)
	var resizeCount int
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	for _, r := range resizes {
		if r.original.size <= r.target.size {
			log.Printf("partition %d does not require shrinking, skipping", r.original.number)
			continue
		}
		log.Printf("Resizing partition %d to %d bytes", r.original.number, r.target.size)
		// Update GPT entry for the shrink partition (indexed by number-1)
		// set the new desired size
		table.Partitions[r.original.number-1].Size = uint64(r.target.size)
		// set the end to 0, so that it will be recalculated
		table.Partitions[r.original.number-1].End = 0
		resizeCount++
	}
	if resizeCount == 0 {
		return nil
	}
	if err := d.Partition(table); err != nil {
		return fmt.Errorf("failed to write partition table after shrinking: %v", err)
	}
	return nil
}
