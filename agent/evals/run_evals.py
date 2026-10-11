#!/usr/bin/env python3
"""Print recorded eval results. Never invents a table."""

import os
import sys
from pathlib import Path


def main() -> int:
    if not os.environ.get("LLM_API_KEY"):
        print("NOT RUN")
        return 1
    folder = Path(__file__).resolve().parent / "results"
    files = sorted(folder.glob("*.json")) if folder.exists() else []
    if not files:
        print("NOT RUN")
        return 1
    for path in files:
        print(path.read_text())
    return 0


if __name__ == "__main__":
    sys.exit(main())
