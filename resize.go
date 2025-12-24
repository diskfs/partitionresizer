package partitionresizer

import (
	"errors"
	"fmt"
	"io"
	"log"

	"github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

type copyData struct {
	count int64
	err   error
}

// resize performs the actual resize operations on the given disk
func resize(resizes []partitionResizeTarget, d *disk.Disk, dryRun bool) error {
	// loop through each resize, create the new partition, and copy the data over

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
		log.Printf("resizing partition %s: original %+v, target %+v", r.original.label, r.original, r.target)
		if dryRun {
			log.Printf("dry run enabled, skipping resize of partition %s", r.original.label)
			continue
		}
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

	// second, do the copy
	// it depends on the filesystem type:
	// - squashfs, ext4, unknown: raw data copy
	// - fat32: use filesystem copy
	for _, r := range resizes {
		if dryRun {
			log.Printf("dry run enabled, skipping data copy for partition %s", r.original.label)
			continue
		}
		log.Printf("copying data for partition %s from original to new partition", r.original.label)
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
			if err != nil {
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
