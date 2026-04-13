#!/usr/bin/env python3
"""Claude Code agent runner with artifact-based session management."""

import json
import os
import subprocess
import sys
import uuid
from pathlib import Path


def get_sessions(payload):
    if not isinstance(payload, dict):
        return []
    artifact = payload.get("artifact")
    if not isinstance(artifact, dict):
        return []
    claude_code = artifact.get("claude_code")
    if not isinstance(claude_code, dict):
        return []
    sessions = claude_code.get("sessions")
    if not isinstance(sessions, list):
        return []
    return sessions


def resolve_session(sessions, invoked_type, invoked_name):
    for session in sessions:
        if session.get("type") == invoked_type and session.get("name") == invoked_name:
            return session["id"], True
    return str(uuid.uuid4()), False


def update_sessions(sessions, invoked_type, invoked_name, session_id):
    new_entry = {"type": invoked_type, "name": invoked_name, "id": session_id}
    result = []
    found = False
    for session in sessions:
        if session.get("type") == invoked_type and session.get("name") == invoked_name:
            result.append(new_entry)
            found = True
        else:
            result.append(session)
    if not found:
        result.append(new_entry)
    return result


def build_payload_patch(sessions):
    return {
        "payload_patch": {
            "artifact": {
                "claude_code": {
                    "sessions": sessions,
                }
            }
        }
    }


def write_payload_patch(sessions, output_dir=None):
    if output_dir is None:
        output_dir = str(Path.home() / ".boid" / "output")
    os.makedirs(output_dir, exist_ok=True)
    patch = build_payload_patch(sessions)
    output_path = os.path.join(output_dir, "payload_patch.yaml")
    with open(output_path, "w") as f:
        json.dump(patch, f, indent=2)


def read_payload_from_file(path):
    try:
        with open(path) as f:
            return json.load(f)
    except (FileNotFoundError, json.JSONDecodeError, IOError):
        return {}


def read_payload_from_string(data):
    if not data or not data.strip():
        return {}
    try:
        return json.loads(data)
    except (json.JSONDecodeError, ValueError):
        return {}


def ensure_skills_symlink():
    skills_src = Path.home() / ".local" / "share" / "boid" / "skills" / "boid-sandbox"
    skills_link = Path.home() / ".claude" / "skills" / "boid-sandbox"
    if not skills_link.exists() and not skills_link.is_symlink():
        skills_link.parent.mkdir(parents=True, exist_ok=True)
        skills_link.symlink_to(skills_src)


def main():
    ensure_skills_symlink()

    interactive = os.environ.get("BOID_INTERACTIVE") == "1"
    model = os.environ.get("BOID_MODEL", "")
    invoked_type = os.environ.get("BOID_INVOKED_TYPE", "executor")
    invoked_name = os.environ.get("BOID_INVOKED_NAME", "")

    if interactive:
        payload_path = str(Path.home() / ".boid" / "context" / "payload.json")
        payload = read_payload_from_file(payload_path)
    else:
        payload = read_payload_from_string(sys.stdin.read())

    sessions = get_sessions(payload)
    session_id, is_resume = resolve_session(sessions, invoked_type, invoked_name)

    args = ["claude", "--dangerously-skip-permissions"]
    if is_resume:
        args.extend(["--resume", session_id])
    else:
        args.extend(["--session-id", session_id])
    if model:
        args.extend(["--model", model])

    if interactive:
        args.append("/boid-sandbox")
        result = subprocess.run(args)
        exit_code = result.returncode
    else:
        args.extend(["--verbose", "--output-format=stream-json", "-p", "/boid-sandbox"])
        script_path = Path(__file__).resolve()
        prefix = script_path.name.split("--", 1)[0] + "--" if "--" in script_path.name else ""
        format_stream = str(script_path.parent / f"{prefix}format-stream.py")

        claude_proc = subprocess.Popen(
            ["setsid", "-w"] + args,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
        )
        format_proc = subprocess.Popen(
            ["python3", format_stream],
            stdin=claude_proc.stdout,
        )
        claude_proc.stdout.close()
        format_proc.wait()
        exit_code = claude_proc.wait()

    updated_sessions = update_sessions(sessions, invoked_type, invoked_name, session_id)
    write_payload_patch(updated_sessions)

    sys.exit(exit_code)


if __name__ == "__main__":
    main()
