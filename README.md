# Partition Resizer

This is a tool to resize GPT disk partitions.

It assumes the following:

* The disk image uses GPT partitioning.
* Any space to be recovered is from an ext4 filesystem on a partition.
* The ext4 filesystem is not mounted when resizing.
* The partitions have a specific naming/labeling scheme.

It does **not** require resizing of the ext4 partition, if there is sufficient
available space on the disk to create newer, larger partitions.

## Partition structure

The new partition sizes are the following:

* ESP - EFI System Partition, FAT32, grow to 2GB
* IMGA - EVE Image Partition, squashfs, grow to 4GB
* IMGB - EVE Image Partition, squashfs, grow to 4GB
* PERSIST - User data, ext4, shrink to free up 10GB at the end of the disk, if necessary

If the disk containing the existing ESP/IMGA/IMGB partitions already has at least 10GB free at the end of the disk, then PERSIST filesystem is untouched. This is because of one of the following:

* PERSIST filesystem is smaller than its partition (unlikely) such that partition has at least 10GB unallocated to the filesystem, in which case the partition is simply shrunk
* unallocated space on the disk of at least 10GB, either because there is unpartitioned space of at least 10GB after the PERSIST partition, or because PERSIST is on another disk entirely, in which case no action affects PERSIST filesystem or partition at all

## Process

The automated process is as follows:

1. Ensure PERSIST is offline; the other partitions do not need to be offline, although it is better.
1. Determine the disk that contains ESP/IMGA/IMGB, henceforth "root disk".
1. Determine if there is sufficient unpartitioned space at the end of the root disk; if so, skip the next step.
1. Free up space:
   1. Determine the location and size of PERSIST partition and filesystem.
   1. Determine if PERSIST filesystem can be shrunk enough to free up space; if not, exit with error.
   1. Shrink PERSIST filesystem.
   1. Shrink PERSIST partition.
1. Create new ESP, IMGA and IMGB partitions of the required sizes at the end of the root disk.
1. Copy existing ESP, IMGA and IMGB partition contents to the new partitions.
   * ESP: create new FAT32 filesystem on new ESP partition, copy contents.
   * IMGA/IMGB: copy partition contents using `dd`.
1. Update GPT partition table to mark new partitions, and unmark old partitions.
1. Sync and exit.
