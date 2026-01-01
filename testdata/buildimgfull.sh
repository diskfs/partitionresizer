#!/bin/sh
set -e
mkdir -p dist
cat << "EOF" | docker run -i --rm -v $PWD/dist:/data -w /data --privileged alpine:3.22
set -e
set -x

# install the tools we need
apk --update add e2fsprogs e2fsprogs-extra parted losetup dosfstools mtools sleuthkit

# create a blank 10GB disk image
dd if=/dev/zero of=diskfull.img bs=1M count=10240

# partition layout: ESP 50MB, parta 500MB, partb 500MB, shrinker rest
parted --script diskfull.img mklabel gpt
parted --script diskfull.img mkpart primary fat32 1MiB 51MiB
parted --script diskfull.img mkpart primary ext4 51MiB 551MiB
parted --script diskfull.img mkpart primary ext4 551MiB 1051MiB
parted --script diskfull.img mkpart primary ext4 1051MiB 100%
parted --script diskfull.img name 1 ESP
parted --script diskfull.img name 2 parta
parted --script diskfull.img name 3 partb
parted --script diskfull.img name 4 shrinker

# create FAT32 filesystem on ESP and populate with 10 random files/directories
LOOPDEV=$(losetup -f --show -o $((1 * 1024 * 1024)) diskfull.img)
mkfs.vfat -F 32 "$LOOPDEV"
fatlabel "$LOOPDEV" ESP
mount "$LOOPDEV" /mnt
for i in $(seq 1 10); do
  mkdir -p /mnt/dir${i}
  dd if=/dev/urandom of=/mnt/dir${i}/rand${i}.dat bs=1024 count=1
done
umount /mnt
losetup -d "$LOOPDEV"

# create ext4 filesystem on shrinker partition and populate with 100 random files/directories
START=$(parted -s diskfull.img unit B print | awk '/^ 4/ {print $2}' | sed 's/B$//')
LOOPDEV=$(losetup -f --show -o $START diskfull.img)
mkfs.ext4 "$LOOPDEV"
mount "$LOOPDEV" /mnt
for i in $(seq 1 100); do
  mkdir -p /mnt/dir${i}
  dd if=/dev/urandom of=/mnt/dir${i}/rand${i}.dat bs=1024 count=1
done
umount /mnt
losetup -d "$LOOPDEV"
EOF