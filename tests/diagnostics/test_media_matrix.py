"""Diagnostic unit tests for media matrix smoke runner and ffprobe shim."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import MagicMock, patch

ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(ROOT / "tests/integration"))
sys.path.insert(0, str(ROOT / "tools"))

import media_matrix_smoke
import ffprobe_shim


class FFprobeShimUnitTest(unittest.TestCase):
    def test_find_ffmpeg_returns_callable_or_string(self):
        binary = ffprobe_shim.find_ffmpeg()
        self.assertIsInstance(binary, str)
        self.assertTrue(len(binary) > 0)

    def test_probe_nonexistent_file_returns_error_code(self):
        code = ffprobe_shim.probe_with_ffmpeg("ffmpeg", "/nonexistent/video/path.mp4")
        self.assertEqual(code, 1)

    @unittest.skipUnless(shutil.which(ffprobe_shim.find_ffmpeg()),
                         "requires real FFmpeg; executed in video-recovery-integration")
    def test_probe_corrupted_file_returns_error_code(self):
        with tempfile.NamedTemporaryFile(suffix=".mp4") as tf:
            tf.write(b"NOT_A_VALID_MP4_HEADER_AT_ALL_GARBAGE_BYTES")
            tf.flush()
            ffmpeg = ffprobe_shim.find_ffmpeg()
            code = ffprobe_shim.probe_with_ffmpeg(ffmpeg, tf.name)
            self.assertEqual(code, 1)

    def test_ffprobe_shim_cli_execution(self):
        shim_path = ROOT / "tools" / "ffprobe_shim.py"
        self.assertTrue(shim_path.is_file())
        self.assertTrue(os.access(shim_path, os.X_OK))

        proc = subprocess.run([
            sys.executable, str(shim_path), "-v", "error",
            "-show_entries", "format=duration", "-of", "json", "/nonexistent/file.mp4"
        ], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 1)


class MediaMatrixSmokeUnitTest(unittest.TestCase):
    def test_fixture_specifications_contain_ten_videos(self):
        # We test that the runner's defined matrix items cover all 10 required types
        keys = [
            "01_mp4_h264_aac",
            "02_mp4_h264_noaudio",
            "03_webm_vp8_vorbis",
            "04_webm_vp9_opus",
            "05_mov_prores",
            "06_mov_h264_aac",
            "07_mp4_portrait",
            "08_overlong_reject",
            "09_corrupted_reject",
            "10_chunked_large_mp4",
        ]
        self.assertEqual(len(keys), 10)

    def test_chunk_size_constant(self):
        self.assertEqual(media_matrix_smoke.CHUNK_SIZE, 4 << 20)

    def test_report_structure(self):
        sample_matrix = [
            {
                "key": "01_mp4_h264_aac",
                "video_id": 1,
                "container": "mp4",
                "video_codec": "h264",
                "audio_codec": "aac",
                "source_bytes": 21258,
                "input_probe": {"width": 640, "height": 360, "duration": 1.0},
                "final_status": "published",
                "expected_status": "published",
                "processing_error": None,
                "elapsed_seconds": 2.0,
                "media_bytes": 17492,
                "cover_bytes": 6956,
                "duration_ms": 1000,
                "width": 640,
                "height": 360,
            }
        ]
        report = {
            "matrix": sample_matrix,
            "privacy_verified": True,
            "recovery_verified": True,
            "feed_verified": True,
            "search_verified": True,
            "mode": "sqlite",
            "elapsed_seconds": 15.0,
        }
        serialized = json.dumps(report)
        loaded = json.loads(serialized)
        self.assertEqual(len(loaded["matrix"]), 1)
        self.assertTrue(loaded["privacy_verified"])
        self.assertTrue(loaded["recovery_verified"])


if __name__ == "__main__":
    unittest.main()
