#!/usr/bin/env python3
"""判断を boid へ書く唯一の口。

    python3 ~/.claude/skills/boid-metaproject/scripts/write.py <verb> [--report] < payload.json

中身は `boidmeta.write`。このファイルは import パスを通すだけの入口。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from boidmeta.write import main  # noqa: E402

if __name__ == "__main__":
    sys.exit(main())
