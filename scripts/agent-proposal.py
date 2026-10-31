#!/usr/bin/env python3
"""Validate, scan, and transfer coding-agent proposals between Actions jobs."""

import io
import os
import pathlib
import re
import stat
import subprocess
import sys
import tarfile
import tempfile


ROOT = pathlib.Path.cwd().resolve()
MAX_FILE = 20 * 1024 * 1024
MAX_TOTAL = 50 * 1024 * 1024
MAX_PATHS = 500
ACTION_LINE = re.compile(
    r"(?:Command|File read|File write|File edit|File change|Repository search|"
    r"File search|Web fetch|Web search|Tool call) (?:started|finished|failed|requested)"
)


def reject(reason):
    raise ValueError(f"agent proposal rejected: {reason}")


def git(*args, data=None):
    result = subprocess.run(
        ["git", *args], input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False
    )
    if result.returncode:
        reject("Git could not inspect the proposal")
    return result.stdout


def names(data):
    return [os.fsdecode(name) for name in data.split(b"\0") if name]


def excluded_name(name):
    lower = [part.lower() for part in name.split("/")]
    return any(
        part == ".git" or part.startswith(".env") or part in (".ssh", ".aws", ".gnupg")
        for part in lower
    ) or lower[-1] in (
        ".npmrc", ".pypirc", ".netrc", "id_rsa", "id_ed25519", "credentials.json"
    ) or lower[-1].endswith((".pem", ".key", ".p12", ".pfx", ".jks", ".keystore"))


def path_for(name):
    if not name or name.startswith("/") or "\\" in name or re.search(r"[\x00-\x1f\x7f]", name):
        reject("invalid proposal path")
    parts = name.split("/")
    if any(part in ("", ".", "..") for part in parts):
        reject("noncanonical proposal path")
    if excluded_name(name):
        reject("credential or repository-control path")
    target = ROOT.joinpath(*parts)
    for parent in target.parents:
        if parent == ROOT:
            break
        if parent.is_symlink():
            reject("symlink directory in proposal path")
    return target


def text_file(data):
    if len(data) > MAX_FILE or b"\0" in data:
        reject("oversized or binary proposal file")
    try:
        data.decode("utf-8")
    except UnicodeDecodeError:
        reject("non-UTF-8 proposal file")
    return data


def read_file(path):
    descriptor = os.open(path, os.O_RDONLY | os.O_NONBLOCK | getattr(os, "O_NOFOLLOW", 0))
    with os.fdopen(descriptor, "rb") as handle:
        info = os.fstat(handle.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > MAX_FILE:
            reject("nonregular, linked, or oversized proposal file")
        contents = handle.read(MAX_FILE + 1)
        if len(contents) != info.st_size:
            reject("proposal file changed while reading")
    return text_file(contents)


def patch_paths(patch):
    if len(patch) > MAX_TOTAL or b"\0" in patch or b"GIT binary patch" in patch:
        reject("oversized or binary patch")
    if re.search(rb"(?m)^(?:new file mode|deleted file mode|old mode|new mode) (?:120000|160000)\r?$", patch):
        reject("symlink or submodule mode in patch")
    if re.search(rb"(?m)^index [0-9a-f]+\.\.[0-9a-f]+ (?:120000|160000)\r?$", patch):
        reject("symlink or submodule index in patch")
    result = git("apply", "--numstat", "-z", "-", data=patch)
    found = []
    for entry in result.split(b"\0"):
        if not entry:
            continue
        fields = entry.split(b"\t", 2)
        if len(fields) != 3 or not fields[2]:
            reject("unsupported patch path encoding")
        name = os.fsdecode(fields[2])
        path_for(name)
        found.append(name)
    if len(found) > MAX_PATHS or len(found) != len(set(found)):
        reject("too many or duplicate patch paths")
    return found


def archive_files(filename, patched):
    files = []
    total = 0
    try:
        with tarfile.open(filename, "r:") as archive:
            for member in archive:
                if member.type != tarfile.REGTYPE or member.name in patched:
                    reject("unsupported archive member type or overlapping path")
                path_for(member.name)
                if member.size > MAX_FILE or member.mode & 0o7000:
                    reject("unsafe archive file size or mode")
                total += member.size
                if total > MAX_TOTAL:
                    reject("archive exceeds proposal size limit")
                stream = archive.extractfile(member)
                if stream is None:
                    reject("unreadable archive member")
                data = stream.read(MAX_FILE + 1)
                if len(data) != member.size:
                    reject("truncated archive member")
                files.append((member.name, text_file(data), member.mode & 0o777))
                if len(files) > MAX_PATHS:
                    reject("too many archive members")
    except (tarfile.TarError, OSError):
        reject("invalid proposal archive")
    if len(files) != len({name for name, _, _ in files}):
        reject("duplicate archive paths")
    return files


def action_bytes(path):
    if not path.exists():
        return b""
    contents = read_file(path)
    if len(contents) > 16 * 1024 or any(
        not ACTION_LINE.fullmatch(line) for line in contents.decode().splitlines()
    ):
        reject("invalid action summary")
    return contents


def scan(directory, scanner, config):
    patch = (directory / "proposal.patch").read_bytes()
    patched = patch_paths(patch)
    archive = archive_files(directory / "new-files.tar", patched)
    with tempfile.TemporaryDirectory(prefix="openpost-agent-scan-") as temp:
        scan_root = pathlib.Path(temp)
        (scan_root / "proposal.patch").write_bytes(patch)
        total = len(patch)
        for name in patched:
            target = path_for(name)
            if not os.path.lexists(target):
                continue
            content = read_file(target)
            total += len(content)
            if total > MAX_TOTAL:
                reject("proposal scan exceeds size limit")
            staged = scan_root / name
            staged.parent.mkdir(parents=True, exist_ok=True)
            staged.write_bytes(content)
        for name, content, _ in archive:
            total += len(content)
            if total > MAX_TOTAL:
                reject("proposal scan exceeds size limit")
            staged = scan_root / name
            staged.parent.mkdir(parents=True, exist_ok=True)
            staged.write_bytes(content)
        actions = action_bytes(directory / "agent-actions.txt")
        (scan_root / "agent-actions.txt").write_bytes(actions)
        result = subprocess.run(
            [str(scanner), "dir", "--config", str(config), "--redact=100", "--no-banner", "--no-color", "--timeout=300", str(scan_root)],
            check=False,
        )
        if result.returncode:
            reject("secret scan failed")


def pack(directory, scanner, config):
    if git("rev-parse", "HEAD").strip() != os.environ["GITHUB_SHA"].encode():
        reject("checkout revision changed")
    patched = names(git("diff", "HEAD", "--name-only", "--no-renames", "-z"))
    untracked = names(git("ls-files", "--others", "--exclude-standard", "-z"))
    ignored = names(git("ls-files", "--others", "--ignored", "--exclude-standard", "-z"))
    if any(path_is_secret(name) for name in ignored):
        reject("ignored credential file created")
    for name in patched + untracked:
        path_for(name)
    if set(patched) & set(untracked) or len(patched) + len(untracked) > MAX_PATHS:
        reject("ambiguous or oversized file set")
    patch = git("diff", "HEAD", "--binary", "--no-renames", "--no-ext-diff", "--no-textconv")
    if set(patch_paths(patch)) != set(patched):
        reject("patch and changed files disagree")
    directory.mkdir(parents=True, exist_ok=True)
    (directory / "proposal.patch").write_bytes(patch)
    with tarfile.open(directory / "new-files.tar", "w:") as archive:
        for name in untracked:
            source = path_for(name)
            data = read_file(source)
            info = tarfile.TarInfo(name)
            info.mode = stat.S_IMODE(source.stat().st_mode) & 0o777
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))
    (directory / "agent-actions.txt").write_bytes(action_bytes(pathlib.Path(os.environ["RUNNER_TEMP"]) / "agent-actions.txt"))
    scan(directory, scanner, config)


def path_is_secret(name):
    return excluded_name(name)


def apply(directory, manifest):
    patch = (directory / "proposal.patch").read_bytes()
    patched = patch_paths(patch)
    archived = archive_files(directory / "new-files.tar", patched)
    if any(os.path.lexists(path_for(name)) for name, _, _ in archived):
        reject("archive file already exists in checkout")
    git("apply", "--check", str(directory / "proposal.patch"))
    git("apply", str(directory / "proposal.patch"))
    for name in patched:
        path = path_for(name)
        if os.path.lexists(path):
            read_file(path)
    for name, data, mode in archived:
        path = path_for(name)
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("xb") as handle:
            handle.write(data)
        path.chmod(mode & 0o755 | 0o600)
    paths = sorted(set(patched) | {name for name, _, _ in archived})
    manifest.write_bytes(b"".join(b":(literal)" + os.fsencode(path) + b"\0" for path in paths))


if __name__ == "__main__":
    try:
        command, *arguments = sys.argv[1:]
        if command == "pack" and len(arguments) == 3:
            pack(pathlib.Path(arguments[0]), pathlib.Path(arguments[1]), pathlib.Path(arguments[2]))
        elif command == "scan" and len(arguments) == 3:
            scan(pathlib.Path(arguments[0]), pathlib.Path(arguments[1]), pathlib.Path(arguments[2]))
        elif command == "apply" and len(arguments) == 2:
            apply(pathlib.Path(arguments[0]), pathlib.Path(arguments[1]))
        else:
            reject("expected pack, scan, or apply command")
    except (ValueError, OSError, KeyError) as error:
        print(f"agent proposal validation failed: {error}", file=sys.stderr)
        sys.exit(1)
