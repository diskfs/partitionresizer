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

// isUnknownFilesystem reports whether err is a *disk.UnknownFilesystemError.
// Use errors.As so the per-instance partition field doesn't matter
// (which it would with errors.Is against a zero-valued instance).
func isUnknownFilesystem(err error) bool {
	var u *disk.UnknownFilesystemError
	return errors.As(err, &u)
}

// resize performs the actual resize operations on the given disk.
// When preserveNumbers is set, a relocated partition is renumbered back to its
// original partition number after the copy, so that consumers referencing a
// partition by number (e.g. boot loaders) continue to find it.
func resize(d *disk.Disk, resizes []partitionResizeTarget, fixErrors, preserveNumbers bool) error {
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

	// remove the old partitions, optionally renumbering the relocated partitions
	// back to their original partition numbers
	if preserveNumbers {
		if err := removeAndRenumberPartitions(d, resizes); err != nil {
			return err
		}
	} else {
		if err := removePartitions(d, resizes); err != nil {
			return err
		}
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
		case err != nil && !isUnknownFilesystem(err):
			return fmt.Errorf("failed to get filesystem for partition %s: %v", r.original.label, err)
		case err != nil || fs.Type() == filesystem.TypeSquashfs:
			log.Printf("partition %d -> %d: performing raw data copy", r.original.number, r.target.number)
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

// removeAndRenumberPartitions removes the original partitions and reassigns each
// relocated target partition's GPT slot index to the original partition's number, so
// the resized partition keeps the same partition number it had before. This is the
// preserve-numbers counterpart to removePartitions, and must run after the data
// has been copied and the identities swapped onto the target partitions. Removal and
// renumbering are done in a single GPT table write so the device never persists an
// intermediate state where the original numbers are gone but the relocated slots have
// not yet been renumbered.
//
// The renumbered entry stays at its new on-disk offset, so the resulting GPT entries
// end up out of disk-offset order. That is permitted by the GPT specification and is
// invisible to consumers that locate a partition by its number (e.g. a boot loader
// referencing (hd0,gptN)); no common tool treats it as an error, though some offer an
// optional manual sort to restore offset order.
func removeAndRenumberPartitions(d *disk.Disk, resizes []partitionResizeTarget) error {
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		return err
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	// map partition number -> position in the slice, captured before any mutation so
	// that the lookups below are unaffected by the index reassignments we make.
	indexToPosition := make(map[int]int)
	for i, p := range table.Partitions {
		indexToPosition[p.Index] = i
	}
	// slice positions of the original partitions to drop, keyed by position rather
	// than partition number: once we reassign a target's Index to the original number,
	// keying removal on Index would also match (and wrongly drop) the renumbered target.
	removePositions := make(map[int]bool)
	for _, r := range resizes {
		if r.original.number == r.target.number {
			log.Printf("partition %d %s: no change in partition number, no need to renumber", r.original.number, r.original.label)
			continue
		}
		origPos, ok := indexToPosition[r.original.number]
		if !ok {
			return fmt.Errorf("original partition %d not found in partition table", r.original.number)
		}
		targetPos, ok := indexToPosition[r.target.number]
		if !ok {
			return fmt.Errorf("target partition %d not found in partition table", r.target.number)
		}
		log.Printf("renumbering partition %d -> %d (label %s) and removing original slot", r.target.number, r.original.number, r.original.label)
		table.Partitions[targetPos].Index = r.original.number
		removePositions[origPos] = true
	}
	// rebuild the slice, dropping the vacated original slots so their numbers are free
	partitions := make([]*gpt.Partition, 0, len(table.Partitions))
	for i, p := range table.Partitions {
		if removePositions[i] {
			continue
		}
		partitions = append(partitions, p)
	}
	table.Partitions = partitions
	if err := d.Partition(table); err != nil {
		return fmt.Errorf("failed to write renumbered partition table: %v", err)
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
	// Look up partitions by their GPT Index, not by slice position.
	// table.Partitions is compacted (only active entries), so the old
	// table.Partitions[number-1] assumed a contiguous 1..N numbering and
	// indexed the wrong entry -- or panicked -- on any non-contiguous layout
	// (e.g. EVE's persist partition at index 9).
	byIndex := make(map[int]*gpt.Partition)
	for _, p := range table.Partitions {
		byIndex[p.Index] = p
	}
	for _, r := range resizes {
		if r.original.size <= r.target.size {
			log.Printf("partition %d does not require shrinking, skipping", r.original.number)
			continue
		}
		p, ok := byIndex[r.original.number]
		if !ok {
			return fmt.Errorf("partition %d not found in partition table", r.original.number)
		}
		log.Printf("Resizing partition %d to %d bytes", r.original.number, r.target.size)
		// set the new desired size; set End to 0 so it is recalculated
		p.Size = uint64(r.target.size)
		p.End = 0
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
