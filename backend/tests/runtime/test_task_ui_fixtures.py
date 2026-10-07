"""Validate frontend fixture exports against frozen S0 contracts, offline."""

import json
import shutil
import subprocess  # nosec B404 - executes only the tracked offline fixture exporter
import unittest
from pathlib import Path

from models.task import TaskView


class TaskUIFixtures(unittest.TestCase):
    def test_representative_frontend_views_match_s0(self):
        root = Path(__file__).resolve().parents[3]
        node = shutil.which("node")
        self.assertIsNotNone(node, "Node is required for the frontend fixture export")
        exported = subprocess.run(  # nosec B603 - resolved Node and fixed tracked script, no shell
            [node, "src/lib/taskFixtures.export.ts"],
            cwd=root / "frontend",
            check=True,
            capture_output=True,
            text=True,
        )
        fixtures = json.loads(exported.stdout)
        self.assertGreaterEqual(len(fixtures), 6)
        for index, payload in enumerate(fixtures):
            with self.subTest(index=index):
                TaskView.model_validate(payload)
