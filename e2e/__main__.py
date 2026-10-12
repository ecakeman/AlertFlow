"""命令行入口。

    python -m e2e --mode memory_off                      # 跑一遍，打印逐场景结果
    python -m e2e --modes memory_off memory_on --out data/compare.json   # 双模式对照

对照不是人工比文案：compare() 按**断言名**逐条对齐，直接指出"哪条断言只在一种模式下通过"。
"""

from __future__ import annotations

import argparse
import json
import pathlib
import sys

from .models import SuiteReport, compare
from .runner import Runner, Stack


def main() -> int:
    parser = argparse.ArgumentParser(prog="e2e", description="AlertFlow 端到端场景测试")
    parser.add_argument("--mode", choices=["memory_on", "memory_off"], default="memory_off",
                        help="单模式运行（默认 memory_off）")
    parser.add_argument("--modes", nargs="*", help="依次运行多种模式并产出对照报告")
    parser.add_argument("--only", nargs="*", help="只跑这些场景 id")
    parser.add_argument("--limit", type=int, help="最多跑前 N 个场景")
    parser.add_argument("--out", default="", help="结果 JSON 的输出路径")
    args = parser.parse_args()

    stack = Stack()
    runner = Runner(stack)
    modes = args.modes or [args.mode]
    reports: dict[str, SuiteReport] = {}

    for mode in modes:
        report = runner.run(mode, only=args.only, limit=args.limit)
        reports[mode] = report
        if args.out and len(modes) == 1:
            pathlib.Path(args.out).write_text(report.model_dump_json(indent=2), encoding="utf-8")

    for mode, report in reports.items():
        metrics = report.metrics
        print(f"\n== {mode} ==")
        print(f"   场景 {metrics['passed']}/{metrics['scenarios']} 通过 · "
              f"断言 {metrics['checks_passed']}/{metrics['checks_total']} · "
              f"检测延迟 {metrics.get('detection_latency_s') or '—'} · "
              f"平均 tokens {metrics.get('agent_tokens_avg') or '—'}")

    if len(reports) > 1:
        comparison = compare(reports)
        target = pathlib.Path(args.out or "suite_compare.json")
        target.write_text(
            json.dumps({"comparison": comparison, "reports": {m: r.model_dump() for m, r in reports.items()}},
                       ensure_ascii=False, indent=2), encoding="utf-8")
        print("\n== 指标对照 ==")
        for mode, metrics in comparison["metrics"].items():
            print(f"   {mode}: 场景 {metrics['scenarios_passed']}/{metrics['scenarios']} · "
                  f"断言 {metrics['checks_passed']}/{metrics['checks_total']} · "
                  f"平均 tokens {metrics['agent_tokens_avg']} · 检测 p50 {metrics['detection_p50_s']}s")
        print("\n== 只在记忆模式下评估的断言（关闭记忆时判 N/A）==")
        for key in comparison["memory_only"][:20]:
            modes = comparison["per_check"].get(key) or comparison["per_scenario"].get(key) or {}
            print(f"   {key}: {modes}")
        print("\n== 两模式结果不同的断言 ==")
        for key in comparison["differing"][:20]:
            print(f"   {key}: {comparison['per_check'].get(key) or comparison['per_scenario'].get(key)}")
        if not comparison["differing"]:
            print("   无（除记忆类断言外，两模式一致）")
        print(f"\n对照报告写到 {target}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
