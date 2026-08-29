#!/usr/bin/env python3
"""sweep task が**最初の一手**として実行する: この巡の対象を組んで自分の description に書く。

    python3 ~/.claude/skills/boid-metaproject/scripts/sweep_targets.py \
        --judge-skill /<判断スキル> [--max-targets N]

中身は `boidmeta.sweep`。このファイルは import パスを通すだけの入口で、メタプロジェクト
側にコピーするものではない —— 詳しくは `boidmeta/__init__.py`。
"""
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from boidmeta.sweep import main  # noqa: E402

if __name__ == "__main__":
    sys.exit(main())
