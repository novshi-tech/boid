#!/usr/bin/env python3
"""Tests for run-agent.py session management logic."""

import importlib.util
import json
import os
import sys
import tempfile
import unittest

_spec = importlib.util.spec_from_file_location(
    "run_agent",
    os.path.join(os.path.dirname(os.path.abspath(__file__)), "run-agent.py"),
)
run_agent = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(run_agent)


class TestGetSessions(unittest.TestCase):
    def test_normal_payload(self):
        payload = {
            "artifact": {
                "claude_code": {
                    "sessions": [
                        {"type": "executor", "name": "", "id": "abc-123"},
                    ]
                }
            }
        }
        result = run_agent.get_sessions(payload)
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["id"], "abc-123")

    def test_empty_payload(self):
        self.assertEqual(run_agent.get_sessions({}), [])

    def test_missing_artifact(self):
        self.assertEqual(run_agent.get_sessions({"instructions": {}}), [])

    def test_missing_claude_code(self):
        self.assertEqual(run_agent.get_sessions({"artifact": {}}), [])

    def test_missing_sessions(self):
        self.assertEqual(
            run_agent.get_sessions({"artifact": {"claude_code": {}}}), []
        )

    def test_none_payload(self):
        self.assertEqual(run_agent.get_sessions(None), [])


class TestResolveSession(unittest.TestCase):
    def test_hit_executor(self):
        sessions = [
            {"type": "executor", "name": "", "id": "abc-123"},
            {"type": "verifier", "name": "security", "id": "def-456"},
        ]
        session_id, is_resume = run_agent.resolve_session(sessions, "executor", "")
        self.assertEqual(session_id, "abc-123")
        self.assertTrue(is_resume)

    def test_hit_verifier_by_name(self):
        sessions = [
            {"type": "verifier", "name": "security", "id": "sec-id"},
            {"type": "verifier", "name": "performance", "id": "perf-id"},
        ]
        session_id, is_resume = run_agent.resolve_session(
            sessions, "verifier", "performance"
        )
        self.assertEqual(session_id, "perf-id")
        self.assertTrue(is_resume)

    def test_miss(self):
        sessions = [
            {"type": "executor", "name": "", "id": "abc-123"},
        ]
        session_id, is_resume = run_agent.resolve_session(
            sessions, "verifier", "security"
        )
        self.assertFalse(is_resume)
        import uuid

        uuid.UUID(session_id)

    def test_empty_sessions(self):
        session_id, is_resume = run_agent.resolve_session([], "executor", "")
        self.assertFalse(is_resume)
        import uuid

        uuid.UUID(session_id)


class TestUpdateSessions(unittest.TestCase):
    def test_add_new_to_empty(self):
        result = run_agent.update_sessions([], "executor", "", "new-id")
        self.assertEqual(result, [{"type": "executor", "name": "", "id": "new-id"}])

    def test_update_existing(self):
        sessions = [{"type": "executor", "name": "", "id": "old-id"}]
        result = run_agent.update_sessions(sessions, "executor", "", "new-id")
        self.assertEqual(len(result), 1)
        self.assertEqual(result[0]["id"], "new-id")

    def test_preserve_others(self):
        sessions = [
            {"type": "executor", "name": "", "id": "exec-id"},
            {"type": "verifier", "name": "security", "id": "sec-id"},
            {"type": "verifier", "name": "performance", "id": "perf-id"},
        ]
        result = run_agent.update_sessions(
            sessions, "verifier", "security", "new-sec-id"
        )
        self.assertEqual(len(result), 3)
        self.assertEqual(result[0], {"type": "executor", "name": "", "id": "exec-id"})
        self.assertEqual(
            result[1], {"type": "verifier", "name": "security", "id": "new-sec-id"}
        )
        self.assertEqual(
            result[2], {"type": "verifier", "name": "performance", "id": "perf-id"}
        )

    def test_add_new_preserves_existing(self):
        sessions = [{"type": "executor", "name": "", "id": "exec-id"}]
        result = run_agent.update_sessions(
            sessions, "verifier", "security", "new-sec-id"
        )
        self.assertEqual(len(result), 2)
        self.assertEqual(result[0], {"type": "executor", "name": "", "id": "exec-id"})
        self.assertEqual(
            result[1], {"type": "verifier", "name": "security", "id": "new-sec-id"}
        )


class TestBuildPayloadPatch(unittest.TestCase):
    def test_structure(self):
        sessions = [{"type": "executor", "name": "", "id": "abc-123"}]
        patch = run_agent.build_payload_patch(sessions)
        self.assertEqual(
            patch,
            {
                "payload_patch": {
                    "artifact": {
                        "claude_code": {
                            "sessions": sessions,
                        }
                    }
                }
            },
        )

    def test_multiple_sessions(self):
        sessions = [
            {"type": "executor", "name": "", "id": "abc"},
            {"type": "verifier", "name": "security", "id": "def"},
        ]
        patch = run_agent.build_payload_patch(sessions)
        self.assertEqual(
            patch["payload_patch"]["artifact"]["claude_code"]["sessions"], sessions
        )


class TestWritePayloadPatch(unittest.TestCase):
    def test_writes_valid_json(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            sessions = [{"type": "executor", "name": "", "id": "test-id"}]
            run_agent.write_payload_patch(sessions, output_dir=tmpdir)

            output_path = os.path.join(tmpdir, "payload_patch.yaml")
            self.assertTrue(os.path.exists(output_path))

            with open(output_path) as f:
                data = json.load(f)

            self.assertIn("payload_patch", data)
            self.assertEqual(
                data["payload_patch"]["artifact"]["claude_code"]["sessions"],
                sessions,
            )

    def test_creates_output_dir(self):
        with tempfile.TemporaryDirectory() as tmpdir:
            nested = os.path.join(tmpdir, "nested", "dir")
            sessions = [{"type": "executor", "name": "", "id": "test-id"}]
            run_agent.write_payload_patch(sessions, output_dir=nested)
            self.assertTrue(os.path.exists(os.path.join(nested, "payload_patch.yaml")))


class TestReadPayloadFromFile(unittest.TestCase):
    def test_reads_json_file(self):
        with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
            json.dump(
                {
                    "artifact": {
                        "claude_code": {
                            "sessions": [
                                {"type": "executor", "name": "", "id": "file-id"}
                            ]
                        }
                    }
                },
                f,
            )
            f.flush()
            path = f.name
        try:
            payload = run_agent.read_payload_from_file(path)
            self.assertEqual(
                payload["artifact"]["claude_code"]["sessions"][0]["id"], "file-id"
            )
        finally:
            os.unlink(path)

    def test_missing_file_returns_empty(self):
        payload = run_agent.read_payload_from_file("/nonexistent/path.json")
        self.assertEqual(payload, {})

    def test_invalid_json_returns_empty(self):
        with tempfile.NamedTemporaryFile(
            mode="w", suffix=".json", delete=False
        ) as f:
            f.write("not json{{{")
            f.flush()
            path = f.name
        try:
            payload = run_agent.read_payload_from_file(path)
            self.assertEqual(payload, {})
        finally:
            os.unlink(path)


class TestReadPayloadFromString(unittest.TestCase):
    def test_valid_json(self):
        data = json.dumps({"artifact": {"claude_code": {"sessions": []}}})
        payload = run_agent.read_payload_from_string(data)
        self.assertEqual(payload["artifact"]["claude_code"]["sessions"], [])

    def test_empty_string(self):
        self.assertEqual(run_agent.read_payload_from_string(""), {})

    def test_invalid_json(self):
        self.assertEqual(run_agent.read_payload_from_string("{bad json"), {})


class TestEnvVarDefaults(unittest.TestCase):
    def test_default_type_is_executor(self):
        env = {}
        self.assertEqual(env.get("BOID_INVOKED_TYPE", "executor"), "executor")

    def test_default_name_is_empty(self):
        env = {}
        self.assertEqual(env.get("BOID_INVOKED_NAME", ""), "")

    def test_explicit_values_used(self):
        env = {"BOID_INVOKED_TYPE": "verifier", "BOID_INVOKED_NAME": "security"}
        self.assertEqual(env.get("BOID_INVOKED_TYPE", "executor"), "verifier")
        self.assertEqual(env.get("BOID_INVOKED_NAME", ""), "security")


if __name__ == "__main__":
    unittest.main()
