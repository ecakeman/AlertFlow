"""唯一的时间窗/调查范围构造函数（C1 / 规格 §7.7.3）。

历史遗留：graph.py、tools.py、integrations/mcp.py 曾各自推导时间窗（4 套算法）。
现在全部改成调用这里：父级算一次 ``InvestigationWindow``，adapter 只消费
``state["window"]``，不得再自己加减 lookback。

时间规则：
1. incident_start 以事件 occurred_at 为上界；
2. incident_resolve 只有在 resolved_at 真正存在时才用它，否则回退到事件时间并记录
   ``RESOLVE_TIME_MISSING``；
3. 下界 = max(已验证 started_at, window_end - lookback_limit)，lookback 同时受
   ``CHECK_LOOKBACK_MINUTES`` 与 ``MCP_MAX_WINDOW_SECONDS`` 约束；
4. 所有来源（Go gRPC / Prometheus / Loki / Memory）消费同一个 start/end。
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Optional

from alertflow_agent.schemas.context import (
    InvestigationScope,
    InvestigationWindow,
    TriggerType,
    ensure_utc,
)

_RESOLVE_MISSING = "RESOLVE_TIME_MISSING"
_STARTED_AFTER_END = "STARTED_AFTER_WINDOW_END"
_OCCURRED_MISSING = "OCCURRED_AT_MISSING"
_END_IN_FUTURE = "WINDOW_END_IN_FUTURE"


def parse_time(value) -> Optional[datetime]:
    """接受 ISO 字符串 / Unix 秒 / datetime；解析失败返回 None（不猜）。"""
    if value is None:
        return None
    if isinstance(value, datetime):
        return ensure_utc(value)
    if isinstance(value, (int, float)):
        return datetime.fromtimestamp(float(value), tz=timezone.utc)
    if isinstance(value, str):
        text = value.strip()
        if not text:
            return None
        try:
            parsed = datetime.fromisoformat(text.replace("Z", "+00:00"))
        except ValueError:
            return None
        if parsed.tzinfo is None:
            parsed = parsed.replace(tzinfo=timezone.utc)
        return parsed.astimezone(timezone.utc)
    return None


def build_investigation_window(
    event: dict,
    *,
    lookback_minutes: int = 60,
    max_window_seconds: int = 3600,
    now: Optional[datetime] = None,
    window_kind: str = "root_cause",
) -> InvestigationWindow:
    """父级唯一入口：从事件 envelope 推导 InvestigationWindow。"""
    now = ensure_utc(now) if now is not None else datetime.now(timezone.utc)
    event = event or {}
    trigger = str(event.get("event") or event.get("trigger_type") or "incident_start")
    gaps: list[str] = []
    accuracy = "exact"

    occurred = parse_time(event.get("occurred_at"))
    if occurred is None:
        occurred = now
        accuracy = "unknown"
        gaps.append(_OCCURRED_MISSING)

    resolved = parse_time(event.get("resolved_at"))
    anchor = "occurred_at"
    if trigger == "incident_resolve":
        if resolved is not None:
            end = resolved
            anchor = "resolved_at"
        else:
            end = occurred
            accuracy = "derived"
            gaps.append(_RESOLVE_MISSING)
    else:
        end = occurred

    # lookback 双重上限：业务回看分钟数 + MCP 单次查询窗口上限
    lookback_seconds = max(0, min(int(lookback_minutes) * 60, int(max_window_seconds)))
    lower = end - timedelta(seconds=lookback_seconds)

    started = parse_time(event.get("started_at"))
    if started is not None:
        if started > end:
            # 开始时间晚于窗口上界：不收缩窗口，只记录时间不一致
            accuracy = "derived"
            gaps.append(_STARTED_AFTER_END)
        else:
            lower = max(lower, started)

    if end > now:
        # 事件时间在未来：把窗口收敛到当前时刻，避免查询未来数据
        end = now
        lower = min(lower, end)
        accuracy = "derived"
        gaps.append(_END_IN_FUTURE)
    if lower > end:
        lower = end
        gaps.append("WINDOW_GAP")

    return InvestigationWindow(
        window_start=lower,
        window_end=end,
        window_kind=window_kind,  # type: ignore[arg-type]
        anchor_field=anchor,
        lookback_seconds=int((end - lower).total_seconds()),
        time_accuracy=accuracy,  # type: ignore[arg-type]
        gap_codes=gaps,
    )


def window_between(base, start: int, end: int, kind: str = "root_cause") -> InvestigationWindow:
    """从一个工具请求的 [start, end] 派生**子窗口**。

    工具只能缩小/平移查询窗口，不能另立算法：窗口的 kind/anchor/accuracy 仍沿用
    父级 InvestigationWindow，lookback 记为子窗口跨度。
    """
    start_dt = datetime.fromtimestamp(int(start), timezone.utc)
    end_dt = datetime.fromtimestamp(int(end), timezone.utc)
    return InvestigationWindow(
        window_start=start_dt,
        window_end=end_dt,
        window_kind=kind or (base.window_kind if base else "root_cause"),
        anchor_field=base.anchor_field if base else "",
        lookback_seconds=max(0, int(end) - int(start)),
        time_accuracy=base.time_accuracy if base else "derived",
        gap_codes=list(base.gap_codes) if base else [],
    )


def scope_with_window(scope: InvestigationScope, window: InvestigationWindow) -> InvestigationScope:
    """复制 scope 的 run/service/incident，只替换查询窗口。"""
    return scope.model_copy(update={"window": window})


def window_from_state(
    state: dict,
    settings=None,
    *,
    now: Optional[datetime] = None,
) -> InvestigationWindow:
    """工具/adapter 统一取窗口：优先父级已算好的 state["window"]。"""
    raw = state.get("window")
    if raw:
        if isinstance(raw, InvestigationWindow):
            return raw
        return InvestigationWindow.model_validate(raw)
    event = state.get("event") or {}
    lookback = int(getattr(settings, "check_lookback_minutes", 60) or 0)
    cap = int(getattr(settings, "mcp_max_window_seconds", 3600) or lookback * 60)
    return build_investigation_window(
        event, lookback_minutes=lookback, max_window_seconds=cap, now=now
    )


def scope_from_event(
    event: dict,
    run_id: str,
    *,
    window: Optional[InvestigationWindow] = None,
    settings=None,
    now: Optional[datetime] = None,
) -> InvestigationScope:
    """构造一次 run 的 InvestigationScope（窗口由父级传入或现场推导）。"""
    event = event or {}
    if window is None:
        window = build_investigation_window(
            event,
            lookback_minutes=int(getattr(settings, "check_lookback_minutes", 60) or 0),
            max_window_seconds=int(getattr(settings, "mcp_max_window_seconds", 3600) or 3600),
            now=now,
        )
    occurred = parse_time(event.get("occurred_at")) or window.window_end
    trigger = str(event.get("event") or event.get("trigger_type") or "incident_start")
    if trigger not in ("incident_start", "incident_resolve", "manual", "chat"):
        trigger = "manual"
    incident_id = event.get("incident_id")
    if incident_id is not None:
        try:
            incident_id = int(incident_id)
        except (TypeError, ValueError):
            incident_id = None
    return InvestigationScope(
        run_id=run_id,
        service_id=str(event.get("service_id") or ""),
        incident_id=incident_id,
        trigger_type=trigger,  # type: ignore[arg-type]
        occurred_at=occurred,
        started_at=parse_time(event.get("started_at")),
        resolved_at=parse_time(event.get("resolved_at")),
        observed_at=parse_time(event.get("observed_at")),
        recorded_at=parse_time(event.get("recorded_at")),
        time_accuracy=window.time_accuracy,
        window=window,
    )


def window_from_context(context: dict, settings=None) -> Optional[InvestigationWindow]:
    """diagnostic_context 里 Go 已给定的 timeWindow：转成 InvestigationWindow 并消费。"""
    time_window = (context or {}).get("timeWindow") or {}
    start = parse_time(time_window.get("from"))
    end = parse_time(time_window.get("to"))
    if start is None or end is None or end < start:
        return None
    gaps: list[str] = []
    if (context or {}).get("coverage", {}).get("truncated"):
        gaps.append("WINDOW_GAP")
    return InvestigationWindow(
        window_start=start,
        window_end=end,
        window_kind="root_cause",
        anchor_field="timeWindow",
        lookback_seconds=int((end - start).total_seconds()),
        time_accuracy="derived",
        gap_codes=gaps,
    )
