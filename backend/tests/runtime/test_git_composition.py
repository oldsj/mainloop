"""The internal schema name must not change serialized Git evidence."""

import json
import unittest

from models.push_gate import GitComposition, GitObservation


class GitCompositionTests(unittest.TestCase):
    def test_wire_alias_survives_nested_dump_and_round_trip(self):
        wire = dict(payload_image="image", provider="claude", schema=2, cli_version="1")
        value = GitComposition.model_validate(wire)
        self.assertEqual(value.schema_version, 2)
        self.assertNotIn("schema", GitComposition.model_fields)
        self.assertEqual(value.model_dump(), wire)
        self.assertEqual(json.loads(value.model_dump_json()), wire)
        observation = GitObservation.model_validate(
            {
                "runtime": dict(
                    session_id="s",
                    generation_id="g",
                    atespace="a",
                    actor_name="n",
                    actor_uid="u",
                    revision="r",
                ),
                "context_id": "context",
                "agent": dict(namespace="ns", name="agent"),
                "workspace": dict(repo="owner/repo", branch="feature"),
                "runtime_composition": wire,
            }
        )
        self.assertEqual(observation.model_dump()["runtime_composition"], wire)
        self.assertEqual(
            GitObservation.model_validate_json(observation.model_dump_json()),
            observation,
        )
