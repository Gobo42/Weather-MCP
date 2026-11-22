import hashlib
import os
import shutil
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest


PROJECT = Path(__file__).resolve().parents[1]
SCRIPT = PROJECT / "scripts/package.sh"


class PackagingTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(dir="/build/weather-agent/test-tmp", prefix="package-test-")
        self.addCleanup(self.temporary.cleanup)
        self.root = Path(self.temporary.name)

    def package(self, destination, success=True):
        result = subprocess.run(["bash", str(SCRIPT), "--output-dir", str(destination)], cwd=PROJECT, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, success, result.stderr)

    def test_directory_variants_and_checksums(self):
        for relative in (False, True):
            for suffix in ("", "/", "///"):
                destination = self.root / ("out-" + str(relative) + str(len(suffix)))
                if suffix == "/":
                    destination.mkdir(mode=0o700)
                argument = os.path.relpath(destination, PROJECT) if relative else str(destination)
                self.package(argument + suffix)
                archive = destination / "weather-agent-linux-amd64.tar.gz"
                digest = hashlib.sha256(archive.read_bytes()).hexdigest()
                self.assertEqual(Path(str(archive) + ".sha256").read_text(), digest + "  " + archive.name + "\n")
                with tarfile.open(archive) as bundle:
                    self.assertTrue(all(member.name.split("/")[0] == "weather-agent" and not member.issym() and not member.islnk() for member in bundle))
                    self.assertIn("weather-agent/bin/weather-agent", bundle.getnames())
                    self.assertIn("weather-agent/licenses/open-location-code.txt", bundle.getnames())
                    self.assertIn("weather-agent/sql/migrations/002_multiple_locations.sql",bundle.getnames())
                    self.assertIn("weather-agent/docs/THIRD_PARTY_NOTICES.md", bundle.getnames())
                    for member in bundle:
                        if member.isdir():
                            self.assertEqual(member.mode, 0o700 if member.name.endswith("/credentials") else 0o750)
                    self.assertIn("set-location", bundle.extractfile("weather-agent/docs/configuration.md").read().decode())
                    self.assertFalse(any("superpowers" in name or name.endswith(".go") for name in bundle.getnames()))
                self.package(argument + suffix, False)
                self.assertEqual(hashlib.sha256(archive.read_bytes()).hexdigest(), digest)

    def test_symlinks_roots_files_and_partial_existing_outputs(self):
        target = self.root / "target"
        target.mkdir(mode=0o700)
        link = self.root / "link"
        link.symlink_to(target)
        for destination in (str(link) + "///", str(link / ".." / "escaped"), "/", self.root / "missing" / "child"):
            self.package(destination, False)
        blocked = self.root / "file"
        blocked.write_text("keep")
        self.package(str(blocked) + "///", False)
        self.assertEqual(blocked.read_text(), "keep")
        existing = target / "weather-agent-linux-amd64.tar.gz.sha256"
        existing.write_text("keep")
        self.package(target, False)
        self.assertEqual(existing.read_text(), "keep")
        self.assertFalse((target / "weather-agent-linux-amd64.tar.gz").exists())

    def test_new_output_with_private_umask(self):
        destination = self.root / "private-umask-output"
        result = subprocess.run(["bash", str(SCRIPT), "--output-dir", str(destination)], cwd=PROJECT, capture_output=True, text=True, umask=0o077)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(destination.stat().st_mode & 0o777, 0o750)
        with tarfile.open(destination / "weather-agent-linux-amd64.tar.gz") as bundle:
            self.assertEqual(bundle.getmember("weather-agent/bin/weather-agent").mode, 0o755)
            for name in ("config/collector.example.json","config/mariadb.example.json","config/mcp/apps.d/weather.example.json","config/mcp/connections/weather.example.json"):
                self.assertEqual(bundle.getmember("weather-agent/"+name).mode,0o640)
            for name in ("config/credentials/database.example.json","config/mcp/credentials/weather.example.json"):
                self.assertEqual(bundle.getmember("weather-agent/"+name).mode,0o600)

    def test_failed_build_leaves_no_distribution_or_temporary_archive(self):
        checkout = self.root / "broken-checkout"
        (checkout / "scripts").mkdir(parents=True)
        shutil.copyfile(SCRIPT, checkout / "scripts/package.sh")
        shutil.copyfile(PROJECT / "go.mod", checkout / "go.mod")
        destination = self.root / "output"
        result = subprocess.run(["bash", str(checkout / "scripts/package.sh"), "--output-dir", str(destination)], capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("no Go files", result.stderr)
        self.assertEqual(list(destination.iterdir()), [])


if __name__ == "__main__":
    unittest.main()
