# Partition Resizer

This is a tool to resize GPT disk partitions and their filesystems. It can grow multiple partitions,
primarily by copying the partitions to new, larger partitions in available free space on the disk.

If insufficient free space is available, and you give it an optional shrink partition that is ext4,
it will shrink the ext4 filesystem and its partition to find space, if it can.

It assumes the following:

* The disk image uses GPT partitioning.
* Any space to be recovered is from an ext4 filesystem on a partition.
* The ext4 filesystem is not mounted when resizing.
* The partitions have a specific naming/labeling scheme.

It does **not** require resizing of the ext4 partition, if there is sufficient
available space on the disk to create newer, larger partitions.

## Filesystems

It has the following handling for filesystems:

* Growing FAT32: create a new FAT32 filesystem on the new partition, copy contents.
* Growing squashfs: copy partition contents using `dd`.
* Shrinking ext4: use `resize2fs` to shrink the filesystem, then shrink the partition.

## Dependencies

The only external dependency is `resize2fs`, which is part of the `e2fsprogs-extras` package on Linux,
and brew formula `e2fsprogs` on macOS.

If you do not need to resize an ext4 filesystem, you do not need to have this installed.

## Block devices

resizer works with both disk image files and block devices. When working with block devices,
if it needs to resize an ext4 filesystem, it will copy the partition to a temporary file,
shrink the temporary file's filesystem, then copy it back to the block device, and then shrink that
partition.
