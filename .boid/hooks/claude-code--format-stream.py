#!/usr/bin/env python3
"""Format Claude stream-json output into human-readable text."""

import json
import sys


def truncate(s, max_len=200):
    """Truncate string to max_len, appending '... (truncated)' if needed."""
    if len(s) > max_len:
        return s[:max_len] + "... (truncated)"
    return s


def format_system_init(event):
    """Format a system init event."""
    lines = ["--- system init ---"]
    session_id = event.get("session_id", "")
    if session_id:
        lines.append(f"session_id: {session_id}")
    model = event.get("model", "")
    if model:
        lines.append(f"model: {model}")
    cwd = event.get("cwd", "")
    if cwd:
        lines.append(f"cwd: {cwd}")
    tools = event.get("tools", [])
    lines.append(f"tools: {len(tools)} available")
    return "\n".join(lines)


def format_tool_use(block):
    """Format a tool_use content block."""
    name = block.get("name", "unknown")
    lines = [f"--- tool_use: {name} ---"]
    inp = block.get("input", {})
    prominent_keys = [
        "file_path", "path", "command", "pattern", "url",
        "query", "glob", "content", "old_string", "new_string",
        "description", "prompt", "skill",
    ]
    remaining = {}
    for key, value in inp.items():
        if key in prominent_keys:
            lines.append(f"{key}: {truncate(str(value))}")
        else:
            remaining[key] = value
    if remaining:
        lines.append(f"input: {truncate(json.dumps(remaining, ensure_ascii=False))}")
    return "\n".join(lines)


def format_assistant(event):
    """Format an assistant message event."""
    message = event.get("message", {})
    content = message.get("content", [])
    sections = []
    for block in content:
        btype = block.get("type", "")
        if btype == "text":
            text = block.get("text", "")
            sections.append(f"--- assistant ---\n{text}")
        elif btype == "tool_use":
            sections.append(format_tool_use(block))
    return "\n\n".join(sections)


def format_tool_result_content(content):
    """Format tool result content (string or list)."""
    if isinstance(content, str):
        text = content
    elif isinstance(content, list):
        text_parts = []
        for item in content:
            if isinstance(item, dict) and item.get("type") == "text":
                text_parts.append(item.get("text", ""))
            elif isinstance(item, str):
                text_parts.append(item)
        text = "\n".join(text_parts)
    else:
        text = str(content)

    lines = text.split("\n")
    if len(lines) > 20:
        remaining = len(lines) - 20
        return "\n".join(lines[:20]) + f"\n... ({remaining} more lines)"
    return text


def format_user(event):
    """Format a user message event (typically tool_result)."""
    message = event.get("message", {})
    content = message.get("content", [])
    sections = []
    for block in content:
        if isinstance(block, dict) and block.get("type") == "tool_result":
            sections.append("--- tool_result ---")
            result_content = block.get("content", "")
            if result_content:
                sections.append(format_tool_result_content(result_content))
    if sections:
        return "\n".join(sections)
    return None


def format_result(event):
    """Format a result event."""
    lines = ["--- result ---"]
    for key in ["duration_ms", "num_turns", "total_cost_usd", "is_error", "subtype"]:
        if key in event:
            lines.append(f"{key}: {event[key]}")
    return "\n".join(lines)


def process_line(line):
    """Process a single line of stream-json, returning formatted text or None."""
    line = line.strip()
    if not line:
        return None

    try:
        event = json.loads(line)
    except json.JSONDecodeError:
        return f"[raw] {truncate(line)}"

    etype = event.get("type", "")

    if etype == "system" and event.get("subtype") == "init":
        return format_system_init(event)
    elif etype == "assistant":
        return format_assistant(event)
    elif etype == "user":
        return format_user(event)
    elif etype == "result":
        return format_result(event)
    else:
        # Ignore other types (stream_event, etc.)
        return None


def main():
    first = True
    for line in sys.stdin:
        try:
            output = process_line(line)
            if output is not None:
                if not first:
                    print()
                print(output)
                first = False
        except Exception as e:
            print(f"[error] {e}", file=sys.stderr)
            print(f"[error] {truncate(line.strip())}")


if __name__ == "__main__":
    main()
