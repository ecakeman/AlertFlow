"""e2e：AlertFlow 端到端场景测试。

    python -m e2e --mode memory_off --out data/suite_off.json
    python -m e2e --modes memory_off memory_on --out data/compare.json

场景目录来自 Fault Lab（GET /scenarios），断言见 checks.REGISTRY，
引擎见 runner.Runner——三者互相独立，引擎里没有场景专属代码。
"""

from .models import CheckResult, Observed, ScenarioResult, SuiteReport, compare
from .runner import Runner, Stack

__all__ = ["CheckResult", "Observed", "ScenarioResult", "SuiteReport", "compare", "Runner", "Stack"]
