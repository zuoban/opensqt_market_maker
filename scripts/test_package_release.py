"""Exercise the release script without building or starting the trading app."""

import hashlib
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest
from zipfile import ZipFile


REPOSITORY = Path(__file__).resolve().parents[1]


class ReleasePackagingTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="opensqt-package-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        (self.root / "scripts").mkdir()
        for name in ("package_release.sh", "package_zip.py"):
            shutil.copy2(REPOSITORY / "scripts" / name, self.root / "scripts" / name)
        (self.root / "main.go").write_text('package main\nvar Version = "v1.2.3"\n')
        for name in ("README.md", "ARCHITECTURE.md", "config.example.yaml", ".env.example"):
            (self.root / name).write_text(f"template: {name}\n")
        (self.root / "config.yaml").write_text("private-local-config-fixture\n")
        (self.root / ".env").write_text("private-local-env-fixture\n")
        self.demo = Path("live_server/这里面留在自己电脑/演示页面.html")
        (self.root / self.demo).parent.mkdir(parents=True)
        (self.root / self.demo).write_text("演示页面", encoding="utf-8")
        self.env = {
            key: value for key, value in os.environ.items()
            if not key.startswith(("GIT_", "GO", "OPENSQT_"))
            and key not in ("VERSION", "TARGET_OS", "TARGET_ARCH", "PACKAGE_OS_NAME")
        }
        subprocess.run(["git", "init", "-q", str(self.root)], env=self.env, check=True)
        subprocess.run(["git", "add", "--", str(self.demo)], cwd=self.root, env=self.env, check=True)
        (self.root / "live_server/local-only.txt").write_text("untracked-demo-fixture\n")
        (self.root / "tools").mkdir()
        fake_go = self.root / "tools/go"
        fake_go.write_text(
            '#!/bin/sh\nset -eu\ntouch build-called\n'
            'while [ "$#" -gt 0 ]; do\n'
            '  if [ "$1" = "-o" ]; then\n'
            '    shift\nprintf "fixture executable\\n" > "$1"\nexit 0\n'
            '  fi\nshift\ndone\nexit 1\n'
        )
        fake_go.chmod(0o755)
        self.env.update(
            PATH=str(self.root / "tools") + os.pathsep + self.env["PATH"],
            TARGET_OS="windows", TARGET_ARCH="amd64",
        )

    def package(self, **env):
        return subprocess.run(
            ["bash", "scripts/package_release.sh"], cwd=self.root,
            env={**self.env, **env}, text=True, capture_output=True,
        )

    def test_mismatched_version_rejected_before_build_or_output_changes(self):
        archive = self.root / "dist/opensqt_market_maker_v9.9.9_windows_amd64.zip"
        archive.parent.mkdir()
        archive.write_bytes(b"existing archive")
        result = self.package(VERSION="v9.9.9")
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("不一致", result.stderr)
        self.assertFalse((self.root / "build-called").exists())
        self.assertEqual(archive.read_bytes(), b"existing archive")

    def test_invalid_or_missing_source_version_rejected_even_with_override(self):
        for declaration in (
            "", 'var Version = "v1.02.3"', 'var Version = "1.2.3"',
            'var Version = "v1.2"', 'var Version = "v1.2.3/../../escape"',
            'var Version = "v1.2.3"\nvar Version = "v1.2.4"',
        ):
            with self.subTest(declaration=declaration):
                (self.root / "main.go").write_text("package main\n" + declaration + "\n")
                result = self.package(VERSION="v1.2.3")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn("Version", result.stderr)
                self.assertFalse((self.root / "build-called").exists())
                self.assertFalse((self.root / "dist").exists())

    def test_windows_archive_preserves_unicode_and_only_ships_templates_and_tracked_demo(self):
        for version in (None, "v1.2.3"):
            with self.subTest(version=version):
                result = self.package(**({} if version is None else {"VERSION": version}))
                self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                basename = "opensqt_market_maker_v1.2.3_windows_amd64"
                archive = self.root / "dist" / f"{basename}.zip"
                with ZipFile(archive) as opened:
                    contents = {
                        name.removeprefix(basename + "/"): opened.read(name)
                        for name in opened.namelist() if not name.endswith("/")
                    }
                    self.assertEqual(set(contents), {
                        "opensqt_market_maker.exe", "README.md", "ARCHITECTURE.md",
                        "config.yaml", "config.example.yaml", ".env.example", self.demo.as_posix(),
                    })
                    self.assertEqual(contents["config.yaml"], (self.root / "config.example.yaml").read_bytes())
                    self.assertEqual(contents[self.demo.as_posix()], "演示页面".encode())
                    member = opened.getinfo(f"{basename}/{self.demo.as_posix()}")
                    self.assertTrue(member.flag_bits & 0x800)
                    extracted = self.root / "extracted"
                    opened.extractall(extracted)
                    self.assertEqual((extracted / basename / self.demo).read_text(encoding="utf-8"), "演示页面")
                digest, filename = Path(str(archive) + ".sha256").read_text().split()
                self.assertEqual(digest, hashlib.sha256(archive.read_bytes()).hexdigest())
                self.assertEqual(filename, archive.name)


class ReleaseToolchainTests(unittest.TestCase):
    def test_docker_matches_ci_go_version(self):
        go_version = re.search(r"^go (\S+)$", (REPOSITORY / "go.mod").read_text(), re.MULTILINE)
        docker_version = re.search(r"^ARG GO_VERSION=(\S+)$", (REPOSITORY / "Dockerfile").read_text(), re.MULTILINE)
        self.assertIsNotNone(go_version)
        self.assertIsNotNone(docker_version)
        self.assertEqual(docker_version[1], go_version[1])


if __name__ == "__main__":
    unittest.main()
