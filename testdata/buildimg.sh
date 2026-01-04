#!/bin/sh
set -e
mkdir -p dist
cat << "EOF" | docker run -i --rm -v $PWD/dist:/data -w /data --privileged alpine:3.22
set -e
set -x

DISK_IMG=disk.img

# install the tools we need

# create a 300MB disk
apk --update add e2fsprogs e2fsprogs-extra parted losetup dosfstools mtools sleuthkit

# create a blank disk image
dd if=/dev/zero of=$DISK_IMG bs=1M count=500

# create two partitions, one each for fat32 and ext4
parted --script $DISK_IMG \
  mklabel gpt \
  mkpart primary fat32 1MiB 30MiB \
  mkpart primary ext4 30MiB 130MiB

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

# create the fat32 filesystem
# mkfs.vfat does not support `-E offset=`, so we need to work around
FAT_IMG=fat.img
set -- $(part_info "1"); START="$1"; SIZE="$2"
dd if=/dev/zero of=$FAT_IMG bs=$MB count=$(( SIZE / MB ))
mkfs.vfat -v -F 32 $FAT_IMG
fatlabel $FAT_IMG resizer
mount $FAT_IMG /mnt
(cd /mnt
mkdir foo
mkdir foo/bar
mkdir lower83
echo 'This is a short file' > SHORT.txt
dd if=/dev/zero 'of=A large name file with spaces' bs=1024 count=2
dd if=/dev/zero 'of=longer_name_without' bs=1024 count=6
dd if=/dev/zero 'of=Large Name with spaces and numbers 7.dat' bs=1024 count=7
dd if=/dev/zero 'of=foo/bar/some_long_embedded_name' bs=1024 count=7
echo low > lower83/LOW.low
echo upp > lower83/UPP.upp
echo Lower > lower83/lower.low
echo Upper > lower83/lower.UPP
)
umount /mnt
# do not forget to copy it back into the disk image
dd if=$FAT_IMG of=$DISK_IMG bs=512 seek=$(( START / 512 )) conv=notrunc
rm $FAT_IMG

# create the ext4 filesystem
EXT4_BLOCKSIZE=4096
set -- $(part_info "2"); START="$1"; SIZE="$2"
mkfs.ext4 -b $EXT4_BLOCKSIZE -E offset=$START $DISK_IMG $(( SIZE / EXT4_BLOCKSIZE ))
mount -o loop,offset=$START $DISK_IMG /mnt
(cd /mnt
mkdir foo
mkdir foo/bar
echo "This is a short file" > shortfile.txt
dd if=/dev/zero of=two-k-file.dat bs=1024 count=2
dd if=/dev/zero of=six-k-file.dat bs=1024 count=6
dd if=/dev/zero of=seven-k-file.dat bs=1024 count=7
dd if=/dev/zero of=ten-meg-file.dat bs=1M count=10
echo "This is a subdir file" > foo/subdirfile.txt
# `set +x` and then `set -x` because otherwie the logs are overloaded with creating 10000 directories
set +x
i=0; until [ $i -gt 10000 ]; do mkdir foo/dir${i}; i=$(( $i+1 )); done
set -x
# create a file with known content
dd if=/dev/random of=random.dat bs=1024 count=20
# symlink to a file and to a dead-end
ln -s random.dat symlink.dat
ln -s /random.dat absolutesymlink
ln -s nonexistent deadlink
ln -s /some/really/long/path/that/does/not/exist/and/does/not/fit/in/symlink deadlonglink # the target here is >60 chars and so will not fit within the inode
# hardlink
ln random.dat hardlink.dat
)
umount /mnt

EOF
