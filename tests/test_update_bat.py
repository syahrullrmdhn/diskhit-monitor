import pathlib
import unittest


ROOT = pathlib.Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "update-diskhist.bat"


class UpdateBatchTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.text = SCRIPT.read_text(encoding="ascii")
        cls.lower = cls.text.lower()

    def test_self_elevates_and_targets_expected_service(self):
        self.assertIn("net session", self.lower)
        self.assertIn("start \"\" /wait powershell", self.lower)
        self.assertIn("diskhistmonitor", self.lower)

    def test_downloads_latest_binary_and_checksum(self):
        self.assertIn("releases/latest/download", self.lower)
        self.assertIn("%base_url%/diskhist-windows-amd64.exe", self.lower)
        self.assertIn("%base_url%/sha256sums.txt", self.lower)
        self.assertIn("get-filehash", self.lower)

    def test_preserves_data_and_has_rollback(self):
        self.assertIn("config.yaml", self.lower)
        self.assertIn("diskhist.db", self.lower)
        self.assertIn("backup", self.lower)
        self.assertIn("rollback", self.lower)

    def test_stops_replaces_starts_and_checks_health(self):
        self.assertIn("sc.exe stop", self.lower)
        self.assertIn("copy /y", self.lower)
        self.assertIn("sc.exe start", self.lower)
        self.assertIn("http://localhost:8787/healthz", self.lower)


if __name__ == "__main__":
    unittest.main()
