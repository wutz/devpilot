#!/usr/bin/env bash
# Copyright 2026 wutz.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Generates the ext4 image fixtures used by internal/ext4 tests.
#
# Runs a privileged Debian container that creates small ext4 images with the
# real mkfs.ext4, loop-mounts them, populates them and writes golden manifests
# (taken from the kernel's view of the MOUNTED filesystem) next to the gzipped
# images in internal/ext4/testdata.
#
# Images produced:
#   clean.img.gz     default e2fsprogs feature set + mmp, cleanly unmounted
#   dirty.img.gz     copied while still mounted after fsync(2): needs_recovery
#                    is set and the latest metadata only exists in the journal
#   features.img.gz  inline_data + ea_inode
#   legacy.img.gz    no extents (indirect block maps), no 64bit, no
#                    metadata_csum, meta_bg, 128-byte inodes
#
# Usage: hack/ext4-fixtures.sh   (needs docker with --privileged support)

set -euo pipefail

if [[ "${1:-}" != "--inner" ]]; then
	repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
	out="$repo/internal/ext4/testdata"
	mkdir -p "$out"
	image="${EXT4_FIXTURE_IMAGE:-debian:bookworm-slim}"
	exec docker run --rm --privileged \
		-v "$out:/out" \
		-v "$repo/hack/ext4-fixtures.sh:/fixtures.sh:ro" \
		"$image" bash /fixtures.sh --inner
fi

# ---------------------------------------------------------------------------
# Inside the container.
# ---------------------------------------------------------------------------

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends e2fsprogs attr acl python3 gzip >/dev/null
echo "== $(mke2fs -V 2>&1 | head -1)"
echo "== kernel $(uname -r)"

work=/work
mkdir -p "$work"
cd "$work"

cat >/tmp/fx.py <<'PY'
import base64, calendar, hashlib, json, os, socket, stat, subprocess, sys

def ts(y, mo, d, h, mi, s, ns):
    return calendar.timegm((y, mo, d, h, mi, s, 0, 0, 0)) * 10**9 + ns

def write(path, data, mode=None):
    with open(path, 'wb') as f:
        f.write(data)
    if mode is not None:
        os.chmod(path, mode)

def pattern(n):
    p = bytes(range(251))
    return (p * (n // 251 + 1))[:n]

def blockpat(i, size=4096):
    tag = b'blk%06d|' % i
    return (tag * (size // len(tag) + 1))[:size]

def fsync_path(path):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)

def common(root, htree_n):
    j = lambda *p: os.path.join(root, *p)
    os.makedirs(j('dir1/dir2/dir3'))
    write(j('dir1/dir2/dir3/deep.txt'), b'deep\n')
    write(j('empty'), b'')
    write(j('hello.txt'), b'hello world\n')
    os.chown(j('hello.txt'), 1000, 1001)
    os.chmod(j('hello.txt'), 0o640)
    os.utime(j('hello.txt'), ns=(ts(2021, 3, 4, 5, 6, 7, 987654321), ts(2021, 3, 4, 5, 6, 7, 123456789)))
    write(j('big.bin'), pattern(5 << 20))
    # symlinks
    os.symlink('hello.txt', j('sym-short'))
    os.symlink('dir1/dir2/dir3/' + 'x' * 80, j('sym-long'))
    # hardlinks
    write(j('linked.txt'), b'linked content\n')
    os.link(j('linked.txt'), j('dir1/linked-hard.txt'))
    # devices, fifo, socket
    os.mkdir(j('devs'))
    os.mknod(j('devs/whiteout'), stat.S_IFCHR | 0o000, os.makedev(0, 0))
    os.mknod(j('devs/null'), stat.S_IFCHR | 0o666, os.makedev(1, 3))
    os.mknod(j('devs/sda1'), stat.S_IFBLK | 0o660, os.makedev(8, 1))
    os.mknod(j('devs/bigdev'), stat.S_IFCHR | 0o600, os.makedev(300, 70000))
    os.mkfifo(j('devs/fifo'), 0o644)
    s = socket.socket(socket.AF_UNIX)
    s.bind(j('devs/sock'))
    s.close()
    # overlay opaque markers
    os.mkdir(j('opaque'))
    os.setxattr(j('opaque'), 'trusted.overlay.opaque', b'y')
    write(j('opaque/inner.txt'), b'inner\n')
    os.mknod(j('opaque/gone'), stat.S_IFCHR, os.makedev(0, 0))
    os.mkdir(j('uopaque'))
    os.setxattr(j('uopaque'), 'user.overlay.opaque', b'y')
    # xattrs (one value large enough to need the external xattr block)
    write(j('xattrs.txt'), b'has xattrs\n')
    os.setxattr(j('xattrs.txt'), 'user.small', b'v')
    os.setxattr(j('xattrs.txt'), 'user.medium', b'm' * 100)
    os.setxattr(j('xattrs.txt'), 'user.big', pattern(3000))
    os.setxattr(j('xattrs.txt'), 'trusted.t', b'trusted-value')
    # POSIX ACLs
    write(j('acl.txt'), b'acl\n')
    subprocess.check_call(['setfacl', '-m', 'u:1234:r-x,g:2345:rw-', j('acl.txt')])
    os.mkdir(j('acl-dir'))
    subprocess.check_call(['setfacl', '-d', '-m', 'u:1234:rwx', j('acl-dir')])
    # special permission bits, non-root owners
    write(j('suid'), b'#!/bin/true\n')
    os.chown(j('suid'), 1000, 1000)
    os.chmod(j('suid'), 0o4755)
    os.mkdir(j('sgid-dir'))
    os.chown(j('sgid-dir'), 0, 1001)
    os.chmod(j('sgid-dir'), 0o2775)
    os.mkdir(j('sticky-dir'))
    os.chmod(j('sticky-dir'), 0o1777)
    write(j('future.txt'), b'future\n')
    os.utime(j('future.txt'), ns=(ts(2100, 1, 1, 0, 0, 0, 500000000),) * 2)
    write(j('past.txt'), b'past\n')
    os.utime(j('past.txt'), ns=(ts(1960, 6, 1, 12, 0, 0, 250000000),) * 2)
    # big directory -> htree (dir_index)
    os.mkdir(j('htree'))
    for i in range(htree_n):
        write(j('htree', 'entry-with-a-longer-name-%05d' % i), b'')

def populate_clean(root):
    common(root, 3000)
    j = lambda *p: os.path.join(root, *p)
    # sparse file: holes, data, an unwritten (fallocated) range partly written
    with open(j('sparse.bin'), 'wb') as f:
        f.truncate(8 << 20)
        f.seek(0); f.write(b'A' * 4096)
        os.posix_fallocate(f.fileno(), 1 << 20, 1 << 20)
        f.seek((1 << 20) + (512 << 10)); f.write(b'B' * 4096)
        f.seek(3 << 20); f.write(b'C' * 65536)
        f.seek((8 << 20) - 4096); f.write(b'D' * 4096)
    # fully fragmented file: every other block written -> >1360 extents,
    # which forces an extent tree of depth 2
    with open(j('frag.bin'), 'wb') as f:
        for i in range(2000):
            f.seek(2 * i * 4096)
            f.write(blockpat(i))
    # fallocate past EOF (KEEP_SIZE) must not show up in reads
    write(j('keepsize.bin'), b'K' * 5000)
    subprocess.check_call(['fallocate', '-n', '-o', '8192', '-l', '65536', j('keepsize.bin')])

def populate_legacy(root):
    common(root, 600)
    j = lambda *p: os.path.join(root, *p)
    # direct, single, double and triple indirect blocks (4K blocks: triple
    # indirect starts at logical block 12+1024+1048576)
    with open(j('sparse-huge.bin'), 'wb') as f:
        f.write(b'A' * 4096)
        f.seek(40 << 10); f.write(b'B' * 16384)          # direct -> single
        f.seek(100 << 20); f.write(b'C' * 8192)          # double
        f.seek((4 << 30) + (16 << 20)); f.write(b'D' * 4096)  # triple
    with open(j('frag.bin'), 'wb') as f:
        for i in range(300):
            f.seek(2 * i * 4096)
            f.write(blockpat(i))

def populate_features(root):
    j = lambda *p: os.path.join(root, *p)
    write(j('empty'), b'')
    write(j('tiny.txt'), b'tiny file\n')
    write(j('fits60.txt'), b'x' * 59)
    write(j('spill.txt'), pattern(100))
    write(j('notinline.txt'), pattern(5000))
    os.mkdir(j('smalldir'))
    for n in ('a', 'bb', 'ccc'):
        write(j('smalldir', n), n.encode())
    os.mkdir(j('growdir'))
    for i in range(40):
        write(j('growdir', 'name-%02d' % i), b'%d' % i)
    os.symlink('tiny.txt', j('sym-short'))
    os.symlink('/some/where/' + 'y' * 70, j('sym-long'))
    write(j('ea.txt'), b'ea inode\n')
    os.setxattr(j('ea.txt'), 'user.huge', pattern(10000))
    os.setxattr(j('ea.txt'), 'user.small', b'small')
    os.mkdir(j('opaque'))
    os.setxattr(j('opaque'), 'trusted.overlay.opaque', b'y')
    os.mknod(j('whiteout'), stat.S_IFCHR, os.makedev(0, 0))

def populate_dirty_base(root):
    j = lambda *p: os.path.join(root, *p)
    os.mkdir(j('a'))
    write(j('a/base1.txt'), b'base1\n')
    write(j('base2.txt'), b'base2 - will be deleted\n')
    write(j('base3.txt'), b'base3 - will be renamed\n')
    write(j('base-big.bin'), pattern(1 << 20))
    os.mkdir(j('keep'))
    write(j('keep/k.txt'), b'keep\n')

def fsync_tree(root):
    for dp, dns, fns in os.walk(root):
        for n in fns:
            p = os.path.join(dp, n)
            st = os.lstat(p)
            if stat.S_ISREG(st.st_mode):
                fsync_path(p)
        fsync_path(dp)

def dirty_changes(root):
    j = lambda *p: os.path.join(root, *p)
    # transaction 1: new tree
    os.makedirs(j('new/sub'))
    for i in range(50):
        write(j('new', 'file-%02d' % i), b'new file %d\n' % i * (i + 1))
    write(j('new/sub/data.bin'), pattern(300000))
    os.symlink('../a/base1.txt', j('new/link'))
    os.mknod(j('new/wh'), stat.S_IFCHR, os.makedev(0, 0))
    os.setxattr(j('new'), 'trusted.overlay.opaque', b'y')
    os.mkdir(j('doomed'))
    for i in range(400):
        write(j('doomed', 'some-long-file-name-to-fill-blocks-%04d' % i), b'')
    fsync_tree(root)
    # transaction 2: modify existing files
    with open(j('a/base1.txt'), 'ab') as f:
        f.write(b'appended after mount\n')
    os.setxattr(j('keep'), 'user.added', b'later')
    os.chmod(j('keep/k.txt'), 0o600)
    os.chown(j('keep/k.txt'), 4242, 4343)
    fsync_tree(root)
    # transaction 3: unlink, rename, rm -r of a multi-block directory
    # (frees directory blocks -> revoke records)
    os.unlink(j('base2.txt'))
    os.rename(j('base3.txt'), j('renamed3.txt'))
    for n in os.listdir(j('doomed')):
        os.unlink(j('doomed', n))
    os.rmdir(j('doomed'))
    fsync_tree(root)
    # transaction 4: new data that may reuse the freed blocks
    write(j('after-revoke.bin'), pattern(2 << 20)[::-1])
    os.utime(j('after-revoke.bin'), ns=(ts(2030, 5, 6, 7, 8, 9, 111111111),) * 2)
    fsync_tree(root)

def manifest(root, out):
    entries = []
    for dp, dns, fns in os.walk(root):
        dns.sort()
        for n in [None] + sorted(dns + fns):
            p = dp if n is None else os.path.join(dp, n)
            if n is None and dp != root:
                continue
            rel = os.path.relpath(p, root)
            st = os.lstat(p)
            e = {
                'path': rel, 'ino': st.st_ino, 'mode': st.st_mode,
                'uid': st.st_uid, 'gid': st.st_gid, 'size': st.st_size,
                'nlink': st.st_nlink, 'mtime_ns': st.st_mtime_ns,
            }
            if stat.S_ISREG(st.st_mode):
                h = hashlib.sha256()
                with open(p, 'rb') as f:
                    while True:
                        b = f.read(1 << 20)
                        if not b:
                            break
                        h.update(b)
                e['sha256'] = h.hexdigest()
            if stat.S_ISLNK(st.st_mode):
                e['link'] = os.readlink(p)
            if stat.S_ISCHR(st.st_mode) or stat.S_ISBLK(st.st_mode):
                e['rdev_major'] = os.major(st.st_rdev)
                e['rdev_minor'] = os.minor(st.st_rdev)
            xs = {}
            for name in os.listxattr(p, follow_symlinks=False):
                xs[name] = base64.b64encode(os.getxattr(p, name, follow_symlinks=False)).decode()
            if xs:
                e['xattrs'] = xs
            entries.append(e)
    with open(out, 'w') as f:
        f.write('{"entries": [\n')
        f.write(',\n'.join(json.dumps(e, sort_keys=True, separators=(',', ':')) for e in entries))
        f.write('\n]}\n')

cmds = {'clean': populate_clean, 'legacy': populate_legacy, 'features': populate_features,
        'dirty-base': populate_dirty_base, 'dirty-changes': dirty_changes}
if sys.argv[1] == 'manifest':
    manifest(sys.argv[2], sys.argv[3])
else:
    cmds[sys.argv[1]](sys.argv[2])
PY

# mkimg NAME SIZE UUID FEATURES [extra mkfs args...]
mkimg() {
	local name=$1 size=$2 uuid=$3 features=$4
	shift 4
	rm -f "$name.img"
	truncate -s "$size" "$name.img"
	mkfs.ext4 -q -F -T default -b 4096 -I 256 -J size=4 -U "$uuid" \
		-O "$features" \
		-E "mmp_update_interval=5,lazy_itable_init=0,lazy_journal_init=0,hash_seed=$uuid" \
		"$@" "$name.img"
}

mnt() { # IMG DIR [opts]
	mkdir -p "$2"
	mount -t ext4 -o "loop${3:+,$3}" "$1" "$2"
}

finish() { # NAME : verify consistency and export
	local name=$1
	e2fsck -fn "$name.img" >"$name.fsck" 2>&1 || { cat "$name.fsck"; echo "e2fsck failed for $name"; exit 1; }
	gzip -9 -n -c "$name.img" >"/out/$name.img.gz"
	echo "== $name: $(dumpe2fs -h "$name.img" 2>/dev/null | grep -i 'features')"
}

# --- clean ------------------------------------------------------------------
mkimg clean 64M 2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a01 mmp -g 2048 -N 8192
mnt clean.img /mnt/clean noatime
python3 /tmp/fx.py clean /mnt/clean
sync
python3 /tmp/fx.py manifest /mnt/clean /out/clean.manifest.json
debugfs -R 'stat /frag.bin' clean.img 2>/dev/null | grep -iE 'extents|depth|^\(' | head -5 || true
umount /mnt/clean
finish clean
echo "== clean frag.bin extent tree: $(debugfs -R 'ex /frag.bin' clean.img 2>/dev/null | awk 'NR>1{print $1}' | sort -u | tr '\n' ' ')"

# --- legacy (indirect blocks, meta_bg, no 64bit/csum, 128-byte inodes) -----
mkimg legacy 32M 2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a02 \
	'mmp,^extent,^64bit,^metadata_csum,^flex_bg,^huge_file,^dir_nlink,^extra_isize,^orphan_file,^resize_inode,meta_bg' \
	-I 128 -g 2048 -N 4096
mnt legacy.img /mnt/legacy noatime
python3 /tmp/fx.py legacy /mnt/legacy
sync
python3 /tmp/fx.py manifest /mnt/legacy /out/legacy.manifest.json
umount /mnt/legacy
finish legacy

# --- features (inline_data, ea_inode) ---------------------------------------
mkimg features 32M 2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a03 mmp,inline_data,ea_inode -N 2048
mnt features.img /mnt/features noatime
python3 /tmp/fx.py features /mnt/features
sync
python3 /tmp/fx.py manifest /mnt/features /out/features.manifest.json
umount /mnt/features
finish features
debugfs -R 'stat /tiny.txt' features.img 2>/dev/null | grep -i flags || true

# --- dirty (copied while mounted, needs journal replay) ---------------------
# Keep the committed metadata buffers from being checkpointed in place before
# the copy: stop periodic writeback while the image is live. sync(1) would also
# flush the block device and checkpoint everything, so the changes are made
# durable with fsync(2) only (jbd2 commit, no in-place metadata writeback).
old_wb=$(cat /proc/sys/vm/dirty_writeback_centisecs)
old_exp=$(cat /proc/sys/vm/dirty_expire_centisecs)
restore_vm() {
	echo "$old_wb" >/proc/sys/vm/dirty_writeback_centisecs || true
	echo "$old_exp" >/proc/sys/vm/dirty_expire_centisecs || true
}
trap restore_vm EXIT

mkdirty() { # NAME UUID FEATURES [extra mkfs args...]
	local name=$1 uuid=$2 features=$3
	shift 3
	mkimg "$name" 32M "$uuid" "$features" -N 2048 "$@"
	mnt "$name.img" "/mnt/$name" noatime
	python3 /tmp/fx.py dirty-base "/mnt/$name"
	umount "/mnt/$name"

	echo 0 >/proc/sys/vm/dirty_writeback_centisecs || echo "warning: could not disable periodic writeback"
	echo 360000 >/proc/sys/vm/dirty_expire_centisecs || true
	mnt "$name.img" "/mnt/$name" noatime,commit=600
	python3 /tmp/fx.py dirty-changes "/mnt/$name"
	python3 /tmp/fx.py manifest "/mnt/$name" "/out/$name.manifest.json"
	cp --sparse=always "$name.img" "$name-snap.img"
	umount "/mnt/$name"
	restore_vm
	mv "$name-snap.img" "$name.img"

	dumpe2fs -h "$name.img" 2>/dev/null | grep -q needs_recovery || { echo "$name.img: needs_recovery NOT set"; exit 1; }
	echo "== $name: $(dumpe2fs -h "$name.img" 2>/dev/null | grep -i 'features')"
	echo "== $name: journal: $(dumpe2fs -h "$name.img" 2>/dev/null | grep -iE 'journal (start|sequence)' | tr '\n' ' ')"
	echo "== $name: without replay, ls /new -> $(debugfs -R 'ls /new' "$name.img" 2>&1 | tail -1)"
	gzip -9 -n -c "$name.img" >"/out/$name.img.gz"
	cp "$name.img" "$name-replayed.img"
	local rc=0
	e2fsck -fy "$name-replayed.img" >"$name-replayed.fsck" 2>&1 || rc=$?
	cat "$name-replayed.fsck"
	if [[ $rc -ge 4 ]]; then echo "e2fsck of replayed $name image failed ($rc)"; exit 1; fi
	echo "== $name: after e2fsck replay, ls /new -> $(debugfs -R 'ls /new' "$name-replayed.img" 2>&1 | head -2 | tr '\n' ' ')"
}

mkdirty dirty 2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a04 mmp
mkdirty dirty-legacy 2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a05 \
	'mmp,^extent,^64bit,^metadata_csum,^flex_bg,^huge_file,^dir_nlink,^extra_isize,^orphan_file' -I 128

# --- tiny (1K blocks, no journal): small enough to seed the fuzzer ---------
rm -f tiny.img
truncate -s 256K tiny.img
mkfs.ext4 -q -F -T default -b 1024 -I 256 -N 32 -U 2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a06 \
	-O '^has_journal,^resize_inode' -E lazy_itable_init=0,hash_seed=2c4fbd3e-6a51-4e8c-9a3c-6c2f3c0d1a06 tiny.img
mnt tiny.img /mnt/tiny noatime
mkdir -p /mnt/tiny/d/e
printf 'hello\n' >/mnt/tiny/d/e/f.txt
head -c 3000 /dev/zero | tr '\0' 'z' >/mnt/tiny/z.txt
ln -s d/e/f.txt /mnt/tiny/s
ln -s "/$(head -c 70 /dev/zero | tr '\0' 'L')" /mnt/tiny/long
mknod /mnt/tiny/d/wh c 0 0
setfattr -n trusted.overlay.opaque -v y /mnt/tiny/d
setfattr -n user.k -v "$(head -c 200 /dev/zero | tr '\0' 'v')" /mnt/tiny/z.txt
python3 /tmp/fx.py manifest /mnt/tiny /out/tiny.manifest.json
umount /mnt/tiny
finish tiny

ls -l /out
