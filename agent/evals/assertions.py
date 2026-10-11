"""Checks a saved report has a passing verification block. It does not score a model."""


def passed(report: dict) -> bool:
    return bool((report.get("verification") or {}).get("passed"))
