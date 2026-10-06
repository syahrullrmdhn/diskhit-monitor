#!/usr/bin/env python3
import hashlib
import os
import pathlib
import subprocess
import tempfile
import unittest
import zipfile

ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "scripts" / "package-release.sh"
GO = "/usr/local/go/bin/go"


class ReleasePackageTests(unittest.TestCase):
    def test_package_contains_required_files_and_valid_checksum(self):
        with tempfile.TemporaryDirectory() as tmp:
            output = pathlib.Path(tmp)
            env = {**os.environ, "GO_BIN": GO, "VERSION": "test", "SOURCE_DATE_EPOCH": "0"}
            subprocess.run([str(SCRIPT), str(output)], cwd=ROOT, env=env, check=True)
            archive = output / "diskhist-windows-amd64-test.zip"
            self.assertTrue(archive.exists())
            expected = {
                "diskhist.exe",
                "config.yaml",
                "README.md",
                "update-diskhist.bat",
                "SHA256SUMS.txt",
            }
            with zipfile.ZipFile(archive) as package:
                self.assertEqual(set(package.namelist()), expected)
                binary = package.read("diskhist.exe")
                sums = package.read("SHA256SUMS.txt").decode("ascii").split()
                self.assertEqual(sums[0], hashlib.sha256(binary).hexdigest())
                self.assertEqual(sums[1], "diskhist.exe")

    def test_package_build_is_reproducible(self):
        with tempfile.TemporaryDirectory() as tmp:
            first = pathlib.Path(tmp) / "first"
            second = pathlib.Path(tmp) / "second"
            env = {**os.environ, "GO_BIN": GO, "VERSION": "test", "SOURCE_DATE_EPOCH": "0"}
            subprocess.run([str(SCRIPT), str(first)], cwd=ROOT, env=env, check=True)
            subprocess.run([str(SCRIPT), str(second)], cwd=ROOT, env=env, check=True)
            self.assertEqual(
                (first / "diskhist-windows-amd64-test.zip").read_bytes(),
                (second / "diskhist-windows-amd64-test.zip").read_bytes(),
            )


if __name__ == "__main__":
    unittest.main()
