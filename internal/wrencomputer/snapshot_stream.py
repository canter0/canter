"""Private sandbox file snapshots. No provider credentials or whole-archive spool.

Restoration is for a fresh, unexposed allocation; failure leaves partial files
which the control plane must discard. Callers quiesce jobs before exporting.
This is a file backup, not RAM or a database transaction snapshot.
"""
import decimal
import gzip
import hashlib
import json
import os
import posixpath
import stat
import struct
import sys
import tarfile
import zlib

FORMAT = "canter-sandbox-files-v2"
MAGIC = b"CNSNAP2\n"
FRAME_BYTES = 128 * 1024
MAX_ENTRIES = 1_000_000
MAX_DEPTH = 256
MAX_PATH = 4096
HARDLINK_MEMORY = 64 * 1024 * 1024
WORKSPACE_ROOT = "/workspace"
HOME_ROOT = "/home/wren"
DIR_FLAGS = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC


def limits(options):
    raw, entries = options["maxUnpackedBytes"], options["maxEntries"]
    if type(raw) is not int or not 1 <= raw <= 1 << 40:
        raise ValueError("invalid snapshot size limit")
    if type(entries) is not int or not 2 <= entries <= MAX_ENTRIES:
        raise ValueError("invalid snapshot entry limit")
    if type(options["includeBrowser"]) is not bool:
        raise ValueError("invalid browser policy")
    return raw, entries, raw + raw // 100 + entries * 16384 + (1 << 20)


def protected(name, browser):
    prefixes = ["home/.ssh", "home/.canter", "workspace/.canter"]
    if not browser:
        prefixes += ["home/.browser", "workspace/.browser"]
    return any(name == prefix or name.startswith(prefix + "/") for prefix in prefixes)


def canonical(name, browser):
    encoded = name.encode("utf-8", "strict")
    parts = name.split("/")
    return (0 < len(encoded) <= MAX_PATH and "\0" not in name
            and posixpath.normpath(name) == name and len(parts) <= MAX_DEPTH
            and parts[0] in ("home", "workspace") and not protected(name, browser))


def version(value):
    # Reads change atime, which is deliberately not part of this check.
    return (value.st_dev, value.st_ino, value.st_size, value.st_mtime_ns,
            value.st_ctime_ns, stat.S_IMODE(value.st_mode), value.st_nlink)


def same_file(before, after):
    if version(before) != version(after):
        raise ValueError("a snapshot source changed while being read")


def timestamp_ns(member):
    value = member.pax_headers.get("mtime", str(member.mtime))
    number = decimal.Decimal(value)
    if not number.is_finite():
        raise ValueError("invalid snapshot timestamp")
    scaled = number * 1_000_000_000
    if not -(1 << 63) <= scaled < (1 << 63):
        raise ValueError("snapshot timestamp is out of range")
    return int(scaled)


def info(name, value, kind):
    if not -(1 << 63) <= value.st_mtime_ns < (1 << 63):
        raise ValueError("snapshot timestamp is out of range")
    member = tarfile.TarInfo(name)
    member.type = kind
    member.mode = stat.S_IMODE(value.st_mode) & (0o1777 if kind == tarfile.DIRTYPE else 0o777)
    member.uid = member.gid = 0
    member.mtime = value.st_mtime_ns // 1_000_000_000
    # Decimal preserves nanoseconds without a float round trip.
    member.pax_headers["mtime"] = str(decimal.Decimal(value.st_mtime_ns) / 1_000_000_000)
    return member


class FramedOutput:
    def __init__(self, output, maximum):
        self.output, self.maximum = output, maximum
        self.pending = bytearray()
        self.count = 0
        self.hash = hashlib.sha256()
        output.write(MAGIC)

    def write(self, data):
        if self.count + len(data) > self.maximum:
            raise ValueError("snapshot compressed limit exceeded")
        self.count += len(data)
        self.hash.update(data)
        position = 0
        while position < len(data):
            take = min(FRAME_BYTES - len(self.pending), len(data) - position)
            self.pending.extend(data[position:position + take])
            position += take
            if len(self.pending) == FRAME_BYTES:
                self.flush()
        return len(data)

    def flush(self):
        if self.pending:
            self.output.write(struct.pack(">BI", 1, len(self.pending)))
            self.output.write(self.pending)
            self.pending.clear()
        self.output.flush()

    def finish(self, receipt):
        self.flush()
        payload = json.dumps(receipt, separators=(",", ":")).encode("utf-8")
        if len(payload) > 16384:
            raise ValueError("snapshot receipt too large")
        self.output.write(struct.pack(">BI", 2, len(payload)))
        self.output.write(payload)
        self.output.flush()


def export_snapshot(workspace, home, options, output):
    maximum, entry_limit, compressed_limit = limits(options)
    framed = FramedOutput(output, compressed_limit)
    result = dict(format=FORMAT, includeBrowser=options["includeBrowser"],
                  rawBytes=0, entries=0, runtimeSkipped=0)
    hardlinks = {}
    hardlink_bytes = 0

    def visit(archive, parent, leaf, name, device):
        nonlocal hardlink_bytes
        if protected(name, options["includeBrowser"]):
            return
        if not canonical(name, options["includeBrowser"]):
            raise ValueError("unsupported snapshot path")
        if result["entries"] + result["runtimeSkipped"] >= entry_limit:
            raise ValueError("snapshot entry limit exceeded")
        current = os.stat(leaf, dir_fd=parent, follow_symlinks=False)
        if current.st_dev != device:
            raise ValueError("mounted filesystems need a separate backup")
        if stat.S_ISFIFO(current.st_mode) or stat.S_ISSOCK(current.st_mode):
            result["runtimeSkipped"] += 1
            return
        result["entries"] += 1
        if stat.S_ISDIR(current.st_mode):
            fd = os.open(leaf, DIR_FLAGS, dir_fd=parent)
            try:
                same_file(current, os.fstat(fd))
                archive.addfile(info(name, current, tarfile.DIRTYPE))
                # Keep names only, not DirEntry objects containing full paths.
                with os.scandir(fd) as children:
                    names = []
                    for child in children:
                        if len(names) >= entry_limit:
                            raise ValueError("snapshot directory entry limit exceeded")
                        names.append(child.name)
                names.sort()
                for child in names:
                    visit(archive, fd, child, name + "/" + child, device)
                same_file(current, os.fstat(fd))
                same_file(current, os.stat(leaf, dir_fd=parent, follow_symlinks=False))
            finally:
                os.close(fd)
        elif stat.S_ISREG(current.st_mode):
            identity = (current.st_dev, current.st_ino)
            if identity in hardlinks:
                target, original = hardlinks[identity]
                if original != version(current):
                    raise ValueError("a hardlinked source changed")
                member = info(name, current, tarfile.LNKTYPE)
                member.linkname = target
                archive.addfile(member)
                same_file(current, os.stat(leaf, dir_fd=parent, follow_symlinks=False))
                return
            if current.st_size > maximum - result["rawBytes"]:
                raise ValueError("snapshot contents exceed their limit")
            fd = os.open(leaf, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK | os.O_CLOEXEC, dir_fd=parent)
            try:
                opened = os.fstat(fd)
                if not stat.S_ISREG(opened.st_mode):
                    raise ValueError("snapshot file type changed")
                same_file(current, opened)
                member = info(name, current, tarfile.REGTYPE)
                member.size = current.st_size
                with os.fdopen(os.dup(fd), "rb") as source:
                    archive.addfile(member, source)
                same_file(current, os.fstat(fd))
                same_file(current, os.stat(leaf, dir_fd=parent, follow_symlinks=False))
            finally:
                os.close(fd)
            result["rawBytes"] += current.st_size
            if current.st_nlink > 1:
                # Conservative accounting bounds Python object/string overhead.
                hardlink_bytes += 4 * len(name) + 1024
                if hardlink_bytes > HARDLINK_MEMORY:
                    raise ValueError("snapshot hardlink metadata limit exceeded")
                hardlinks[identity] = (name, version(current))
        elif stat.S_ISLNK(current.st_mode):
            target = os.readlink(leaf, dir_fd=parent)
            if not target or len(target.encode("utf-8", "strict")) > MAX_PATH or "\0" in target:
                raise ValueError("unsupported snapshot symlink")
            member = info(name, current, tarfile.SYMTYPE)
            member.linkname = target
            archive.addfile(member)
            same_file(current, os.stat(leaf, dir_fd=parent, follow_symlinks=False))
        else:
            raise ValueError("device files cannot be saved in a sandbox snapshot")

    with gzip.GzipFile(fileobj=framed, mode="wb", compresslevel=6, mtime=0) as compressed:
        with tarfile.open(fileobj=compressed, mode="w|", format=tarfile.PAX_FORMAT) as archive:
            for name, source in (("home", home), ("workspace", workspace)):
                fd = os.open(source, DIR_FLAGS)
                try:
                    visit(archive, fd, ".", name, os.fstat(fd).st_dev)
                finally:
                    os.close(fd)
    result["compressedBytes"] = framed.count
    result["archiveSha256"] = framed.hash.hexdigest()
    framed.finish(result)
    return result


class CompressedInput:
    def __init__(self, source, size):
        self.source, self.remaining = source, size
        self.count = 0
        self.hash = hashlib.sha256()

    def read(self, size):
        if not self.remaining:
            return b""
        data = self.source.read(min(size, self.remaining, FRAME_BYTES))
        if not data:
            raise ValueError("truncated snapshot input")
        self.remaining -= len(data)
        self.count += len(data)
        self.hash.update(data)
        return data


class Inflate:
    """One gzip member, bounded output per read and no concatenated members."""
    def __init__(self, source, maximum):
        self.source, self.maximum = source, maximum
        self.zip = zlib.decompressobj(16 + zlib.MAX_WBITS)
        self.pending = b""
        self.count = 0
        self.done = False

    def read(self, size):
        if size < 0 or size > FRAME_BYTES:
            size = FRAME_BYTES
        output = bytearray()
        while len(output) < size and not self.done:
            data = self.pending or self.source.read(FRAME_BYTES)
            chunk = self.zip.decompress(data, size - len(output))
            output.extend(chunk)
            self.pending = self.zip.unconsumed_tail
            if self.zip.eof:
                if self.zip.unused_data or self.pending or self.source.read(1):
                    raise ValueError("snapshot has trailing compressed data")
                self.done = True
            elif not data and not chunk:
                raise ValueError("snapshot gzip is incomplete")
        self.count += len(output)
        if self.count > self.maximum:
            raise ValueError("snapshot metadata limit exceeded")
        return bytes(output)


def remove_tree(parent, leaf):
    value = os.stat(leaf, dir_fd=parent, follow_symlinks=False)
    if stat.S_ISDIR(value.st_mode):
        fd = os.open(leaf, DIR_FLAGS, dir_fd=parent)
        try:
            with os.scandir(fd) as entries:
                names = [entry.name for entry in entries]
            for name in names:
                remove_tree(fd, name)
        finally:
            os.close(fd)
        os.rmdir(leaf, dir_fd=parent)
    else:
        os.unlink(leaf, dir_fd=parent)


def open_parent(roots, name):
    parts = name.split("/")
    if len(parts) < 2:
        raise ValueError("root is not a link target")
    fd = os.dup(roots[parts[0]])
    try:
        for part in parts[1:-1]:
            child = os.open(part, DIR_FLAGS, dir_fd=fd)
            os.close(fd)
            fd = child
        return fd, parts[-1]
    except BaseException:
        os.close(fd)
        raise


def restore_snapshot(workspace, home, options, expected, source, output):
    maximum, entry_limit, compressed_limit = limits(options)
    if (expected.get("format") != FORMAT or expected.get("includeBrowser") != options["includeBrowser"]
            or type(expected.get("compressedBytes")) is not int
            or not 1 <= expected["compressedBytes"] <= compressed_limit
            or type(expected.get("entries")) is not int or not 2 <= expected["entries"] <= entry_limit
            or type(expected.get("rawBytes")) is not int or not 0 <= expected["rawBytes"] <= maximum
            or type(expected.get("runtimeSkipped")) is not int
            or not 0 <= expected["runtimeSkipped"] <= entry_limit - expected["entries"]):
        raise ValueError("invalid snapshot receipt")
    digest = expected.get("archiveSha256", "")
    if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
        raise ValueError("invalid snapshot digest")
    framed = FramedOutput(output, compressed_limit)
    compressed = CompressedInput(source, expected["compressedBytes"])
    inflated = Inflate(compressed, maximum + entry_limit * 16384 + 10240)
    roots, directories = {}, []
    entries = raw_bytes = 0
    seen_roots = set()
    previous = None

    def finish_directory():
        name, fd, mode, modified = directories.pop()
        try:
            # The home root's SSH-safe permissions belong to bootstrap.
            if name != "home":
                os.fchmod(fd, mode)
            os.utime(fd, ns=(modified, modified))
            os.fsync(fd)
        finally:
            os.close(fd)

    try:
        for name, location in (("home", home), ("workspace", workspace)):
            fd = os.open(location, DIR_FLAGS)
            roots[name] = fd
            with os.scandir(fd) as children:
                names = [child.name for child in children]
            for child in names:
                if not protected(name + "/" + child, options["includeBrowser"]):
                    remove_tree(fd, child)
        with tarfile.open(fileobj=inflated, mode="r|") as archive:
            for member in archive:
                name = member.name.removesuffix("/") if member.isdir() else member.name
                parts = tuple(name.split("/"))
                if not canonical(name, options["includeBrowser"]) or (previous is not None and parts <= previous):
                    raise ValueError("invalid or unordered snapshot path")
                previous = parts
                entries += 1
                if entries > entry_limit:
                    raise ValueError("snapshot entry limit exceeded")
                if any(key not in ("path", "linkpath", "size", "mtime", "atime", "ctime") for key in member.pax_headers):
                    raise ValueError("unsupported snapshot metadata")
                if member.sparse is not None:
                    raise ValueError("sparse tar extensions are not supported")
                mask = 0o1777 if member.isdir() else 0o777
                if member.mode < 0 or member.mode & ~mask or member.uid != 0 or member.gid != 0 or member.size < 0:
                    raise ValueError("unsafe snapshot metadata")
                modified = timestamp_ns(member)
                while directories and not name.startswith(directories[-1][0] + "/"):
                    finish_directory()
                if len(parts) == 1:
                    if not member.isdir() or member.size:
                        raise ValueError("invalid snapshot root")
                    seen_roots.add(name)
                    directories.append((name, os.dup(roots[name]), member.mode, modified))
                    continue
                if not directories or directories[-1][0] != posixpath.dirname(name):
                    raise ValueError("snapshot parent is not a directory")
                parent, leaf = directories[-1][1], parts[-1]
                if member.isdir():
                    if member.size:
                        raise ValueError("snapshot directory contains data")
                    os.mkdir(leaf, 0o700, dir_fd=parent)
                    directories.append((name, os.open(leaf, DIR_FLAGS, dir_fd=parent), member.mode, modified))
                elif member.isreg():
                    if member.size > maximum - raw_bytes:
                        raise ValueError("snapshot contents exceed their limit")
                    fd = os.open(leaf, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=parent)
                    try:
                        content = archive.extractfile(member)
                        remaining = member.size
                        while remaining:
                            data = content.read(min(remaining, FRAME_BYTES))
                            if not data:
                                raise ValueError("truncated snapshot member")
                            remaining -= len(data)
                            if data.count(0) == len(data):
                                os.lseek(fd, len(data), os.SEEK_CUR)
                            else:
                                pending = memoryview(data)
                                while pending:
                                    written = os.write(fd, pending)
                                    if written <= 0:
                                        raise ValueError("snapshot write made no progress")
                                    pending = pending[written:]
                        os.ftruncate(fd, member.size)
                        os.fchmod(fd, member.mode)
                        os.utime(fd, ns=(modified, modified))
                        os.fsync(fd)
                    finally:
                        os.close(fd)
                    raw_bytes += member.size
                elif member.islnk():
                    if member.size or not canonical(member.linkname, options["includeBrowser"]) or tuple(member.linkname.split("/")) >= parts:
                        raise ValueError("invalid snapshot hardlink")
                    target_parent, target_name = open_parent(roots, member.linkname)
                    try:
                        before = os.stat(target_name, dir_fd=target_parent, follow_symlinks=False)
                        if not stat.S_ISREG(before.st_mode) or stat.S_IMODE(before.st_mode) != member.mode or before.st_mtime_ns != modified:
                            raise ValueError("snapshot hardlink target is invalid")
                        os.link(target_name, leaf, src_dir_fd=target_parent, dst_dir_fd=parent, follow_symlinks=False)
                        after = os.stat(leaf, dir_fd=parent, follow_symlinks=False)
                        if before.st_dev != after.st_dev or before.st_ino != after.st_ino:
                            raise ValueError("snapshot hardlink target changed")
                    finally:
                        os.close(target_parent)
                elif member.issym():
                    target = member.linkname
                    if member.size or not target or len(target.encode("utf-8", "strict")) > MAX_PATH or "\0" in target:
                        raise ValueError("invalid snapshot symlink")
                    os.symlink(target, leaf, dir_fd=parent)
                    os.utime(leaf, ns=(modified, modified), dir_fd=parent, follow_symlinks=False)
                else:
                    raise ValueError("unsupported snapshot member")
            padding_bytes = 0
            while True:
                data = archive.fileobj.read(FRAME_BYTES)
                padding_bytes += len(data)
                if padding_bytes > 10240 or data.strip(b"\0"):
                    raise ValueError("snapshot contains trailing tar data")
                if not data:
                    break
            if padding_bytes < 512:
                raise ValueError("snapshot tar end is incomplete")
        while directories:
            finish_directory()
        if seen_roots != {"home", "workspace"} or entries != expected["entries"] or raw_bytes != expected["rawBytes"]:
            raise ValueError("snapshot restore contents do not match receipt")
        if compressed.remaining or not inflated.done or compressed.hash.hexdigest() != digest:
            raise ValueError("snapshot restore digest does not match receipt")
        if source.read(1):
            raise ValueError("snapshot input has trailing bytes")
        framed.finish(dict(expected))
        return dict(expected)
    finally:
        for _, fd, _, _ in directories:
            os.close(fd)
        for fd in roots.values():
            os.close(fd)


def run_request(request, source, destination):
    if request["operation"] == "export":
        if source.read(1):
            raise ValueError("export request has trailing data")
        export_snapshot(WORKSPACE_ROOT, HOME_ROOT, request["options"], destination)
    elif request["operation"] == "restore":
        restore_snapshot(WORKSPACE_ROOT, HOME_ROOT, request["options"], request["expected"], source, destination)
    else:
        raise ValueError("invalid snapshot operation")


def main():
    line = sys.stdin.buffer.readline(16385)
    if len(line) > 16384 or not line.endswith(b"\n"):
        raise ValueError("invalid snapshot request")
    run_request(json.loads(line), sys.stdin.buffer, sys.stdout.buffer)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print("Snapshot failed: " + str(error)[:2048], file=sys.stderr)
        sys.exit(1)
