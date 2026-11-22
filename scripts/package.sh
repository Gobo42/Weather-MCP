#!/bin/sh
set -eu
exec python3 - "$0" "$@" <<'PY'
import argparse
import hashlib
import os
from pathlib import Path
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile

project = Path(sys.argv.pop(1)).resolve().parents[1]
parser = argparse.ArgumentParser(description="Build and package Weather Agent without installing it")
parser.add_argument("--output-dir", default=str(project / "dist"))
arguments = parser.parse_args()

def safe_path(path):
    for component in (path, *path.parents):
        if component.is_symlink():
            raise ValueError("symlink path refused")

archive_temporary = None
checksum_temporary = None
try:
    if not os.path.ismount("/build") or not os.access("/build", os.W_OK):
        raise ValueError("/build must be mounted and writable")
    if shutil.disk_usage("/build").free < 1024 * 1024 * 1024:
        raise ValueError("at least 1 GiB free on /build is required")
    build_root = Path("/build/weather-agent")
    safe_path(build_root)
    if not build_root.exists():
        build_root.mkdir(mode=0o750)
        build_root.chmod(0o750)
    safe_path(Path(arguments.output_dir).absolute())
    output = Path(os.path.abspath(os.path.normpath(arguments.output_dir)))
    safe_path(output)
    if output == Path(output.anchor):
        raise ValueError("filesystem root refused")
    if not output.exists():
        if not output.parent.is_dir():
            raise ValueError("output parent must exist")
        output.mkdir(mode=0o750)
        output.chmod(0o750)
    mode = output.stat()
    if not stat.S_ISDIR(mode.st_mode) or mode.st_uid != os.getuid() or mode.st_mode & 0o022:
        raise ValueError("output must be an owned directory without shared write access")
    archive = output / "weather-agent-linux-amd64.tar.gz"
    checksum = Path(str(archive) + ".sha256")
    if archive.exists() or checksum.exists() or archive.is_symlink() or checksum.is_symlink():
        raise ValueError("existing distribution refused")
    if build_root.stat().st_uid != os.getuid() or build_root.stat().st_mode & 0o027:
        raise ValueError("owned build directory without shared write or other-user access required")
    environment = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="amd64", GOCACHE="/build/cache", GOMODCACHE="/build/mod", GOTMPDIR="/build/tmp", TMPDIR="/build/tmp")
    with tempfile.TemporaryDirectory(dir=build_root, prefix="package-") as temporary:
        Path(temporary).chmod(0o750)
        root = Path(temporary) / "weather-agent"
        (root / "bin").mkdir(parents=True)
        subprocess.run(["/usr/local/go/bin/go", "build", "-trimpath", "-ldflags=-s -w", "-o", str(root / "bin/weather-agent"), "."], cwd=project, env=environment, check=True)
        (root / "bin/weather-agent").chmod(0o755)
        files = {
            "README.md": "docs/README.md",
            "THIRD_PARTY_NOTICES.md": "docs/THIRD_PARTY_NOTICES.md",
            "licenses/open-location-code.txt": "licenses/open-location-code.txt",
            "licenses/mysql.txt": "licenses/mysql.txt",
            "licenses/edwards25519.txt": "licenses/edwards25519.txt",
            "docs/configuration.md": "docs/configuration.md",
            "sql/README.md": "sql/README.md",
            "sql/provision-weatheragent.sql": "sql/provision-weatheragent.sql",
            "sql/migrations/001_initial.sql": "sql/migrations/001_initial.sql",
            "sql/migrations/002_multiple_locations.sql": "sql/migrations/002_multiple_locations.sql",
            "deploy/weather-agent.service": "deploy/weather-agent.service",
            "examples/collector.example.json": "config/collector.example.json",
            "examples/mariadb.example.json": "config/mariadb.example.json",
            "examples/credentials/database.example.json": "config/credentials/database.example.json",
            "examples/mcp/app.example.json": "config/mcp/apps.d/weather.example.json",
            "examples/mcp/connection.example.json": "config/mcp/connections/weather.example.json",
            "examples/mcp/credentials.example.json": "config/mcp/credentials/weather.example.json",
        }
        for source, destination in files.items():
            source_path = project / source
            safe_path(source_path)
            if not source_path.is_file():
                raise ValueError("required distribution file is missing")
            target = root / destination
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(source_path, target)
            target.chmod(0o600 if "credentials" in target.relative_to(root).parts else 0o640 if target.suffix == ".json" else 0o644)
        for directory in [root, *(entry for entry in root.rglob("*") if entry.is_dir())]:
            directory.chmod(0o700 if directory.name == "credentials" else 0o750)
        with tempfile.NamedTemporaryFile(dir=output, prefix=".weather-", delete=False) as temporary_archive:
            archive_temporary = Path(temporary_archive.name)
        with tarfile.open(archive_temporary, "w:gz") as bundle:
            bundle.add(root, arcname="weather-agent")
        with tarfile.open(archive_temporary) as bundle:
            for member in bundle:
                parts = Path(member.name).parts
                if not parts or parts[0] != "weather-agent" or ".." in parts or member.issym() or member.islnk():
                    raise ValueError("unsafe archive layout")
        with archive_temporary.open("rb") as archive_file:
            digest = hashlib.file_digest(archive_file, "sha256").hexdigest()
        with tempfile.NamedTemporaryFile(dir=output, prefix=".weather-", delete=False) as temporary_checksum:
            checksum_temporary = Path(temporary_checksum.name)
            temporary_checksum.write((digest + "  " + archive.name + "\n").encode())
            temporary_checksum.flush()
            os.fsync(temporary_checksum.fileno())
        with archive_temporary.open("rb") as archive_file:
            os.fsync(archive_file.fileno())
        os.link(archive_temporary, archive)
        try:
            os.link(checksum_temporary, checksum)
        except BaseException:
            if archive.samefile(archive_temporary):
                archive.unlink()
            raise
        directory = os.open(output, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
        print(archive)
        print(checksum)
except (OSError, ValueError, subprocess.CalledProcessError) as error:
    print("Packaging failed: " + str(error), file=sys.stderr)
    sys.exit(1)
finally:
    for temporary in (archive_temporary, checksum_temporary):
        if temporary is not None:
            temporary.unlink(missing_ok=True)
PY
