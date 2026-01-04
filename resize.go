package partitionresizer

import (
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
	"github.com/diskfs/go-diskfs/partition/part"
)

type copyData struct {
	count int64
	err   error
}

// resize performs the actual resize operations on the given disk
func resize(d *disk.Disk, resizes []partitionResizeTarget, fixErrors bool) error {
	// do any shrinks first
	if err := shrinkFilesystems(d, resizes, fixErrors); err != nil {
		return err
	}
	if err := shrinkPartitions(d, resizes); err != nil {
		return err
	}

	// next create new partitions
	if err := createPartitions(d, resizes); err != nil {
		return err
	}

	if err := copyFilesystems(d, resizes); err != nil {
		return err
	}

	var oldPartitions []int
	for _, r := range resizes {
		oldPartitions = append(oldPartitions, r.original.number)
	}
	if err := removePartitions(d, oldPartitions); err != nil {
		return err
	}

	return nil
}

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
	var partitions []*gpt.Partition
	// the logic here is as follows.
	// 1- Go through each existing partition
	// 2- If it is being grown/moved, create a new partition entry with the new start/size, add it to the table instead of the existing one
	// 3- If it is being shrunk/unchanged, just copy the existing partition entry to the new table
	// The key is to be sure we do something with every single existing partition, so we do not lose any in the new table
	indexMap := map[int]partitionResizeTarget{}
	for _, r := range resizes {
		indexMap[r.original.number] = r
	}
	for _, p := range table.Partitions {
		r, ok := indexMap[int(p.Index)]
		if !ok {
			// not being resized, just copy over
			partitions = append(partitions, p)
			continue
		}
		// no change in start, just copy over, it already was handled
		if r.original.start == r.target.start {
			log.Printf("skipping creation of partition %s, no size or location change", r.original.label)
			partitions = append(partitions, p)
			continue
		}
		log.Printf("resizing partition %s: original %+v, target %+v", r.original.label, r.original, r.target)
		// get existing partition info
		// create the new partition
		newPart := gpt.Partition{
			Start:      uint64(r.target.start / int64(table.LogicalSectorSize)),
			Size:       uint64(r.target.size),
			Type:       p.Type,
			Name:       p.Name,
			GUID:       p.GUID,
			Attributes: p.Attributes,
			Index:      r.target.number,
		}
		partitions = append(partitions, &newPart)
	}
	// write the updated partition table
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
		log.Printf("copying data from original partition %d to new partition %d", r.original.number, r.target.number)
		fs, err := d.GetFilesystem(r.original.number)
		switch {
		case err != nil && !errors.Is(err, &disk.UnknownFilesystemError{}):
			return fmt.Errorf("failed to get filesystem for partition %s: %v", r.original.label, err)
		case err != nil || fs.Type() == filesystem.TypeSquashfs || fs.Type() == filesystem.TypeExt4:
			// copy raw data using a pipe so reads feed writes concurrently
			pr, pw := io.Pipe()
			ch := make(chan copyData, 1)

			go func() {
				defer func() { _ = pw.Close() }()
				read, err := d.ReadPartitionContents(r.original.number, pw)
				ch <- copyData{count: read, err: err}
			}()

			written, err := d.WritePartitionContents(r.target.number, pr)
			var ierr *part.IncompletePartitionWriteError
			if err != nil && !errors.As(err, &ierr) {
				return fmt.Errorf("failed to write raw data for partition %s: %v", r.original.label, err)
			}

			readData := <-ch
			if readData.err != nil {
				return fmt.Errorf("failed to read raw data for partition %s: %v", r.original.label, readData.err)
			}
			if readData.count != written {
				return fmt.Errorf("mismatched read/write sizes for partition %s: read %d bytes, wrote %d bytes", r.original.label, readData.count, written)
			}
			log.Printf("partition %d -> %d: filesystem %v copied byte for byte, %d bytes copied", r.original.number, r.target.number, fs.Type(), written)
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
			if err := CopyFileSystem(fs, newFS); err != nil {
				return fmt.Errorf("failed to copy FAT32 filesystem data for partition %s: %v", r.original.label, err)
			}
			log.Printf("partition %d -> %d: filesystem %v copied file content", r.original.number, r.target.number, fs.Type())
		default:
			return fmt.Errorf("unsupported filesystem type %v for partition %s", fs.Type(), r.original.label)
		}

	}
	return nil
}

func removePartitions(d *disk.Disk, partitions []int) error {
	// first create the new partitions in the partition table and write it
	tableRaw, err := d.GetPartitionTable()
	if err != nil {
		return err
	}
	table, ok := tableRaw.(*gpt.Table)
	if !ok {
		return fmt.Errorf("unsupported partition table type, only GPT is supported")
	}
	for _, partitionNumber := range partitions {
		log.Printf("removing old partition %d", partitionNumber)
		// get existing partition info
		table.Partitions[partitionNumber-1].Type = gpt.Unused
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
