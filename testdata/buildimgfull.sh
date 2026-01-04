#!/bin/sh
set -e
mkdir -p dist
cat << "EOF" | docker run -i --rm -v $PWD/dist:/data -w /data --privileged alpine:3.22
set -e
set -x

DISK_IMG=diskfull.img

# install the tools we need
apk --update add e2fsprogs e2fsprogs-extra parted losetup dosfstools mtools sleuthkit

# create a blank 10GB disk image
dd if=/dev/zero of=$DISK_IMG bs=1M count=10240

# partition layout: ESP 50MB, parta 500MB, partb 500MB, shrinker rest
parted --script $DISK_IMG mklabel gpt
parted --script $DISK_IMG mkpart primary fat32 1MiB 51MiB
parted --script $DISK_IMG mkpart primary ext4 51MiB 551MiB
parted --script $DISK_IMG mkpart primary ext4 551MiB 1051MiB
parted --script $DISK_IMG mkpart primary ext4 1051MiB 100%
parted --script $DISK_IMG name 1 ESP
parted --script $DISK_IMG name 2 parta
parted --script $DISK_IMG name 3 partb
parted --script $DISK_IMG name 4 shrinker

# Helper: get START and SIZE (bytes) for partition number N
part_info() {
  n="$1"
  # parted -m output: NR:STARTB:ENDB:SIZESB:FS:NAME:FLAGS
  line="$(parted -m -s $DISK_IMG unit B print | awk -F: -v n="$n" '$1==n {print $0}')"
  [ -n "$line" ] || { echo "Partition $n not found" >&2; exit 1; }
  start="$(echo "$line" | cut -d: -f2 | sed 's/B$//')"
  end="$(echo "$line"   | cut -d: -f3 | sed 's/B$//')"
  # parted reports end inclusive; size in bytes is (end - start + 1)
  size="$(( end - start + 1 ))"
  echo "$start $size"
}

MB=$(( 1024 * 1024 ))

set -- $(part_info 1); START="$1"; SIZE="$2"
FAT_IMG=fat.img
set -- $(part_info "1"); START="$1"; SIZE="$2"
dd if=/dev/zero of=$FAT_IMG bs=$MB count=$(( SIZE / MB ))
mkfs.vfat -v -F 32 $FAT_IMG
fatlabel "$FAT_IMG" ESP
mount "$FAT_IMG" /mnt
for i in $(seq 1 10); do
  mkdir -p /mnt/dir${i}
  dd if=/dev/urandom of=/mnt/dir${i}/rand${i}.dat bs=1024 count=1
done
umount /mnt
fsck.vfat -n "$FAT_IMG" || true
# do not forget to copy it back into the disk image
dd if=$FAT_IMG of=$DISK_IMG bs=512 seek=$(( START / 512 )) conv=notrunc
rm $FAT_IMG

# create ext4 filesystem on shrinker, parta and partb partitions and populate with 100 random files/directories
EXT4_BLOCKSIZE=4096
for n in 2 3 4; do
  set -- $(part_info "$n"); START="$1"; SIZE="$2"
  mkfs.ext4 -b $EXT4_BLOCKSIZE -E offset=$START $DISK_IMG $(( SIZE / EXT4_BLOCKSIZE ))
  mount -o loop,offset=$START $DISK_IMG /mnt
  for i in $(seq 1 100); do
    mkdir -p /mnt/dir${i}
    dd if=/dev/urandom of=/mnt/dir${i}/rand${i}.dat bs=1024 count=1
  done
  umount /mnt
  e2fsck -f -y "$DISK_IMG?offset=$START"
done


echo "=== Validation: partition ==="
parted -m -s $DISK_IMG unit B print

echo "=== Validation: filesystem signatures ==="
for n in 2 3 4; do
  # Reattach for validation and inspect magic/superblocks
  set -- $(part_info "$n"); START="$1"; SIZE="$2"
  blkid "$DISK_IMG" || true
  if [ "$n" -eq 1 ]; then
    echo "FAT32 partition: not checked"
    # FAT check: show label if available
    # unfortunately, fsck.vfat does not support offset=, so we need to pass
    #fsck.vfat -n "$DISK_IMG" || true
  else
    dumpe2fs -h "$DISK_IMG?offset=$START" | head -n 30 || true
    e2fsck -f -n "$DISK_IMG?offset=$START" || true
  fi
done
EOF