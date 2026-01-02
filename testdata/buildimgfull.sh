#!/bin/sh
set -e
mkdir -p dist
cat << "EOF" | docker run -i --rm -v $PWD/dist:/data -w /data --privileged alpine:3.22
set -e
set -x

# install the tools we need
apk --update add e2fsprogs e2fsprogs-extra parted losetup dosfstools mtools sleuthkit losetup

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

# Helper: get START and SIZE (bytes) for partition number N
part_info() {
  n="$1"
  # parted -m output: NR:STARTB:ENDB:SIZESB:FS:NAME:FLAGS
  line="$(parted -m -s diskfull.img unit B print | awk -F: -v n="$n" '$1==n {print $0}')"
  [ -n "$line" ] || { echo "Partition $n not found" >&2; exit 1; }
  start="$(echo "$line" | cut -d: -f2 | sed 's/B$//')"
  end="$(echo "$line"   | cut -d: -f3 | sed 's/B$//')"
  # parted reports end inclusive; size in bytes is (end - start + 1)
  size="$(( end - start + 1 ))"
  echo "$start $size"
}

set -- $(part_info 1); START="$1"; SIZE="$2"
# create FAT32 filesystem on ESP and populate with 10 random files/directories
LOOPDEV=$(losetup -f --show -o "$START" --sizelimit "$SIZE" diskfull.img)
mkfs.vfat -F 32 "$LOOPDEV"
fatlabel "$LOOPDEV" ESP
mount "$LOOPDEV" /mnt
for i in $(seq 1 10); do
  mkdir -p /mnt/dir${i}
  dd if=/dev/urandom of=/mnt/dir${i}/rand${i}.dat bs=1024 count=1
done
umount /mnt
losetup -d "$LOOPDEV"

# create ext4 filesystem on shrinker, parta and partb partitions and populate with 100 random files/directories
for n in 2 3 4; do
  set -- $(part_info "$n"); START="$1"; SIZE="$2"
  LOOPDEV=$(losetup -f --show -o "$START" --sizelimit "$SIZE" diskfull.img)
  mkfs.ext4 "$LOOPDEV"
  mount "$LOOPDEV" /mnt
  for i in $(seq 1 100); do
    mkdir -p /mnt/dir${i}
    dd if=/dev/urandom of=/mnt/dir${i}/rand${i}.dat bs=1024 count=1
  done
  umount /mnt
  e2fsck -f -y "$LOOPDEV"
  losetup -d "$LOOPDEV"
done

# Validation
echo "=== Validation: partition offsets ==="
parted -m -s diskfull.img unit B print

echo "=== Validation: filesystem signatures ==="
# Reattach for validation and inspect magic/superblocks
for n in 1 2 3 4; do
  set -- $(part_info "$n"); START="$1"; SIZE="$2"
  LOOPDEV="$(losetup -f --show -o "$START" --sizelimit "$SIZE" diskfull.img)"
  echo "--- p$n ($LOOPDEV) ---"
  blkid "$LOOPDEV" || true
  if [ "$n" -eq 1 ]; then
    # FAT check: show label if available
    fsck.vfat -n "$LOOPDEV" || true
  else
    dumpe2fs -h "$LOOPDEV" | head -n 30 || true
    e2fsck -n "$LOOPDEV" || true
  fi
  losetup -d "$LOOPDEV"
done
EOF