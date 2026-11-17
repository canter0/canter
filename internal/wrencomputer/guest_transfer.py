# Embedded into the sandbox helper before its command-line entry point.
from contextlib import contextmanager


TRANSFER_PRIVATE = {".git", ".canter", ".browser", ".ssh"}
TRANSFER_BYTES = 256 << 20
TRANSFER_FILES = 10000


def transfer_private(name):
    return name.lower() in TRANSFER_PRIVATE or name.startswith(".canter-transfer-")


def transfer_sensitive(name):
    lower = name.lower()
    return (lower in {".env", ".netrc", ".npmrc", ".pypirc", "credentials.json", "id_rsa", "id_ed25519"}
            or lower.startswith(".env.") or lower.endswith((".pem", ".key", ".p12", ".pfx")))


def transfer_parts(raw):
    if raw == "/workspace":
        return []
    if not isinstance(raw, str) or not raw.startswith("/workspace/") or len(raw.encode()) > 4096:
        raise Failure("invalid_path", "Choose a normalized directory under /workspace.")
    parts = raw[len("/workspace/"):].split("/")
    if any(not name or name in {".", ".."} or transfer_private(name)
           or "\0" in name or "\\" in name or len(name.encode()) > 255 for name in parts):
        raise Failure("invalid_path", "Choose a normalized directory under /workspace.")
    return parts


@contextmanager
def transfer_dir(parts, create=False):
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW
    fd = os.open(ROOT, flags)
    try:
        for name in parts:
            if create:
                try:
                    os.mkdir(name, mode=0o755, dir_fd=fd)
                except FileExistsError:
                    pass
            child = os.open(name, flags, dir_fd=fd)
            os.close(fd)
            fd = child
        yield fd
    finally:
        os.close(fd)


def transfer_stat(fd, name):
    import stat
    try:
        value = os.stat(name, dir_fd=fd, follow_symlinks=False)
    except FileNotFoundError:
        return "missing", None
    if stat.S_ISDIR(value.st_mode):
        return "directory", value
    if not stat.S_ISREG(value.st_mode) or value.st_nlink != 1:
        return "unsupported", value
    return file_version(value), value


def transfer_leaf(parts):
    if not parts:
        raise Failure("invalid_path", "Choose a path below /workspace.")
    return parts[-1]


def transfer_probe_names(data):
    root = transfer_parts(data.get("path"))
    entries = data.get("entries")
    if not isinstance(entries, list) or len(entries) > TRANSFER_FILES:
        raise Failure("invalid_request", "A transfer supports at most 10,000 entries.")
    result = []
    for entry in entries:
        parts = root if entry["path"] == "." and entry.get("directory") is True else root + transfer_parts("/workspace/" + entry["path"])
        try:
            with transfer_dir(parts[:-1]) as parent:
                version, _ = transfer_stat(parent, transfer_leaf(parts))
        except FileNotFoundError:
            version = "missing"
        except NotADirectoryError:
            version = "unsupported"
        result.append(version)
    return result


def transfer_open(parent, leaf):
    import stat
    fd = os.open(leaf, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK, dir_fd=parent)
    stream = os.fdopen(fd, "rb")
    try:
        current = os.fstat(fd)
        if not stat.S_ISREG(current.st_mode) or current.st_nlink != 1:
            raise Failure("invalid_file", "Only regular single-link files can be transferred.")
        return stream, current
    except BaseException:
        stream.close()
        raise


def transfer_manifest(data):
    root = transfer_parts(data.get("path"))
    allow_sensitive = data.get("includeSensitive") is True
    entries, excluded = [], []
    count, total = 0, 0

    def walk(fd, prefix, depth):
        nonlocal count, total
        if depth > 64:
            raise Failure("too_many_files", "Directory nesting exceeds the transfer limit.")
        with os.scandir(fd) as listing:
            for child in listing:
                count += 1
                if count > TRANSFER_FILES:
                    raise Failure("too_many_files", "A transfer supports at most 10,000 entries.")
                name = child.name
                relative = prefix + name
                try:
                    relative.encode("utf-8")
                except UnicodeEncodeError:
                    raise Failure("invalid_path", "Transfer paths must be valid UTF-8.")
                if transfer_private(name):
                    excluded.append({"path": relative, "reason": "platform or Git metadata"})
                    continue
                transfer_parts("/workspace/" + relative)
                if not allow_sensitive and transfer_sensitive(name):
                    excluded.append({"path": relative, "reason": "sensitive filename"})
                    continue
                version, value = transfer_stat(fd, name)
                if version == "directory":
                    entries.append({"path": relative, "directory": True})
                    sub = os.open(name, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW, dir_fd=fd)
                    try:
                        walk(sub, relative + "/", depth + 1)
                    finally:
                        os.close(sub)
                elif version in {"unsupported", "missing"}:
                    excluded.append({"path": relative, "reason": "unsupported or changed entry"})
                else:
                    stream, opened = transfer_open(fd, name)
                    with stream:
                        if version != file_version(opened):
                            raise Failure("conflict", "The source changed while planning.")
                        total += opened.st_size
                        if total > TRANSFER_BYTES:
                            raise Failure("too_large", "A transfer supports at most 256 MiB.")
                        digest = hashlib.sha256()
                        remaining = opened.st_size
                        while remaining:
                            block = stream.read(min(remaining, 1 << 20))
                            if not block:
                                raise Failure("conflict", "The source changed while planning.")
                            digest.update(block)
                            remaining -= len(block)
                        if file_version(os.fstat(stream.fileno())) != version:
                            raise Failure("conflict", "The source changed while planning.")
                    entries.append({"path": relative, "size": opened.st_size,
                                    "sha256": digest.hexdigest(), "version": version,
                                    "executable": bool(opened.st_mode & 0o111)})

    with transfer_dir(root) as fd:
        walk(fd, "", 0)
    return {"entries": sorted(entries, key=lambda e: e["path"]),
            "exclusions": sorted(excluded, key=lambda e: e["path"])}


def transfer_read(data):
    parts = transfer_parts(data.get("path"))
    offset, limit = data["offset"], data["limit"]
    if not isinstance(offset, int) or not isinstance(limit, int) or offset < 0 or not 0 < limit <= UPLOAD_CHUNK:
        raise Failure("invalid_request", "Invalid transfer range.")
    with transfer_dir(parts[:-1]) as parent:
        stream, before = transfer_open(parent, transfer_leaf(parts))
        with stream:
            version = file_version(before)
            if data["version"] != version or offset > before.st_size:
                raise Failure("conflict", "The source changed. Create a new transfer plan.")
            stream.seek(offset)
            contents = stream.read(min(limit, before.st_size - offset))
            if version != file_version(os.fstat(stream.fileno())):
                raise Failure("conflict", "The source changed while reading.")
    return {"data": base64.b64encode(contents).decode(), "size": before.st_size, "version": version,
            "offset": offset, "nextOffset": offset + len(contents), "eof": offset + len(contents) == before.st_size}


def transfer_commit(data):
    identifier = data["id"]
    base = upload_path(identifier)
    parts = transfer_parts(data["path"])
    leaf = transfer_leaf(parts)
    expected = data["version"]
    with (base / "lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        meta, offset = upload_state(identifier)
        if meta["path"] != data["path"] or meta["size"] != data["size"] or meta["sha256"] != data["sha256"]:
            raise Failure("conflict", "The staging receipt does not match the plan.")
        if meta.get("complete"):
            return upload_receipt(identifier, meta, meta["size"], True)
        if offset != meta["size"]:
            raise Failure("conflict", "File staging is incomplete.")
        with transfer_dir(parts[:-1], create=True) as parent:
            temporary = ".canter-transfer-" + secrets.token_hex(16)
            descriptor = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600, dir_fd=parent)
            try:
                digest = hashlib.sha256()
                with os.fdopen(descriptor, "wb") as out, (base / "payload").open("rb") as payload:
                    while block := payload.read(1 << 20):
                        out.write(block)
                        digest.update(block)
                    os.fchmod(out.fileno(), 0o755 if data.get("executable") else 0o644)
                    out.flush()
                    os.fsync(out.fileno())
                if digest.hexdigest() != meta["sha256"]:
                    raise Failure("conflict", "The staged file does not match the source hash.")
                current, _ = transfer_stat(parent, leaf)
                if current != expected or current in {"unsupported", "directory"}:
                    raise Failure("conflict", "The destination changed. Create a new transfer plan.")
                if expected == "missing":
                    # The link operation atomically refuses a file created since the check.
                    os.link(temporary, leaf, src_dir_fd=parent, dst_dir_fd=parent, follow_symlinks=False)
                else:
                    os.replace(temporary, leaf, src_dir_fd=parent, dst_dir_fd=parent)
                os.fsync(parent)
            finally:
                try:
                    os.unlink(temporary, dir_fd=parent)
                except FileNotFoundError:
                    pass
        meta["complete"] = True
        meta["actualSha256"] = meta["sha256"]
        meta["updatedUnix"] = time.time()
        save(base / "meta.json", meta)
        (base / "payload").unlink(missing_ok=True)
        return upload_receipt(identifier, meta, offset, True)


def transfer_mkdir(data):
    parts = transfer_parts(data["path"])
    with transfer_dir(parts, create=True):
        return {"complete": True}


base_operation = operation


def operation(name, data):
    if name == "probe":
        result = base_operation(name, data)
        result["transferProtocol"] = int(SANDBOX_MARKER.exists())
        return result
    if name.startswith("transfer_"):
        if not SANDBOX_MARKER.exists():
            raise Failure("invalid_request", "Directory transfers require the sandbox helper.")
        actions = {"transfer_stat": transfer_probe_names, "transfer_manifest": transfer_manifest,
                   "transfer_read": transfer_read, "transfer_commit": transfer_commit,
                   "transfer_mkdir": transfer_mkdir}
        if name not in actions:
            raise Failure("invalid_request", "Unknown directory transfer operation.")
        try:
            return actions[name](data)
        except (FileExistsError, NotADirectoryError, PermissionError):
            raise Failure("conflict", "A transfer path changed or is not a regular workspace path.")
        except FileNotFoundError:
            raise Failure("not_found", "A transfer path or staged file no longer exists.")
    return base_operation(name, data)
