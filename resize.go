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
func resize(d *disk.Disk, resizes []partitionResizeTarget) error {
	// do any shrinks first
	if err := shrinkFilesystems(d, resizes); err != nil {
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
	partitions := table.Partitions
	for _, r := range resizes {
		if r.original.size == r.target.size && r.original.start == r.target.start {
			log.Printf("skipping creation of partition %s, no size or location change", r.original.label)
			continue
		}
		log.Printf("resizing partition %s: original %+v, target %+v", r.original.label, r.original, r.target)
		// get existing partition info
		orig := partitions[r.original.number-1]
		// create the new partition
		newPart := gpt.Partition{
			Start:      uint64(r.target.start),
			Size:       uint64(r.target.size),
			Type:       orig.Type,
			Name:       orig.Name,
			GUID:       orig.GUID,
			Attributes: orig.Attributes,
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

func shrinkFilesystems(d *disk.Disk, resizes []partitionResizeTarget) error {
	for _, r := range resizes {
		if r.original.size <= r.target.size {
			log.Printf("partition %d does not require shrinking, skipping", r.original.number)
			continue
		}
		log.Printf("shrinking filesystem %d %s from %d to %d bytes", r.original.number, r.original.label, r.original.size, r.target.size)
		// verify ext4 fs on shrink partition
		fs, err := d.GetFilesystem(r.original.number)
		if err != nil {
			return fmt.Errorf("failed to get filesystem for shrink partition: %v", err)
		}
		if fs.Type() != filesystem.TypeExt4 {
			return fmt.Errorf("unsupported filesystem type for shrinking: %v", fs.Type())
		}

		// perform the shrink
		if err := shrinkFilesystem(r.target, r.original.size-r.target.size); err != nil {
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
		table.Partitions[r.original.number-1].Size = uint64(r.target.size)
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
