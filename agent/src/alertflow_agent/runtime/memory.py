"""记忆的写入与档案派生。

**写在 persist 之后、图之外的一步**：它失败绝不能影响报告——所以整段都被
try/except 包住，出错只记日志。事件卡是确定性地从「事件 + 证据 + 报告」里抽出来的，
不额外调用模型（省钱、可复现）；只有报告里的 conclusion 是模型产出。
"""

from __future__ import annotations

import hashlib
import json
import logging
from datetime import datetime, timezone

from ..analysis.errors import classify_error
from ..analysis.stats import error_stats
from ..analysis.timeline import status_counts
from .verify import cited
from ..schemas.memory import (
    Conclusion,
    EpisodeCard,
    EpisodeOutcome,
    EvidenceSignature,
    KnownCause,
    KnownPattern,
    ServiceBaseline,
    ServiceProfile,
    SignatureItem,
    SourceRef,
    Symptom,
    TimelineAttachment,
)

log = logging.getLogger("alertflow_agent")

_ABNORMAL = {"degraded", "unhealthy"}


def _parse(value) -> datetime | None:
    if isinstance(value, datetime):
        return value
    if not isinstance(value, str) or not value:
        return None
    try:
        parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=timezone.utc)


def signature_key(signature: EvidenceSignature) -> str:
    """由内部证据签名派生的失败类型键——**不向目标索取 profile 名**（那是答案泄漏）。

    同一种内部故障 → 同一把键，用于事件簿去重与「同类复发」判定。
    """
    names = sorted(f"{item.kind}:{item.name}" for item in signature.abnormal)
    if not names:
        return "sig:none"
    import hashlib

    digest = hashlib.sha1("|".join(names).encode("utf-8")).hexdigest()[:12]
    return f"sig:{digest}"


def build_card(state: dict, report: dict) -> EpisodeCard:
    event = state.get("event") or {}
    rows = state.get("raw_checks") or []
    evidence = state.get("evidence") or {}

    counts = status_counts(rows)
    total = max(1, int(counts.get("total") or 0))
    offline = int((counts.get("offline") or {}).get("n") or 0)
    errors = error_stats(rows, classify_error)

    status_codes: list[int] = []
    error_classes: list[str] = []
    if isinstance(errors, dict):
        for code, n in (errors.get("status_codes") or {}).items():
            if not n:
                continue
            try:
                status_codes.append(int(float(code)))
            except (TypeError, ValueError):
                continue
        error_classes = [k for k, v in (errors.get("classes") or {}).items() if v]

    abnormal: list[SignatureItem] = []
    kinds: list[str] = []
    earliest: datetime | None = None
    for ident, item in evidence.items():
        if item.get("source_type") != "project_diagnostic":
            continue
        # 证据条目的字段在 ``data`` 里（_add 把整个 item 放进去），不在顶层。
        data = item.get("data")
        if not isinstance(data, dict):
            continue
        kind = str(data.get("kind") or item.get("kind") or "")
        if kind == "service_info":
            continue
        kinds.append(kind)
        status = data.get("status")
        if status in _ABNORMAL:
            abnormal.append(SignatureItem(kind=kind, name=str(data.get("name")), status=status))
            observed = _parse(data.get("observed_at")) or _parse(item.get("observed_at"))
            if observed and (earliest is None or observed < earliest):
                earliest = observed

    # 结论优先取报告里的结构化 conclusion；缺了就退到最强的一条 supported 假设。
    conclusion = _conclusion_from(report)

    started = _parse(event.get("started_at")) or _parse(event.get("occurred_at"))
    resolved = _parse(event.get("resolved_at"))
    duration = int((resolved - started).total_seconds()) if (started and resolved) else None

    signature = EvidenceSignature(abnormal=abnormal, kinds=sorted(set(kinds)), earliest_deviation_at=earliest)
    return EpisodeCard(
        service_id=str(event.get("service_id") or ""),
        target_endpoint=str(event.get("service_url") or ""),
        profile_key=signature_key(signature),
        incident_id=event.get("incident_id"),
        run_id=state.get("run_id"),
        started_at=started,
        resolved_at=resolved,
        duration_s=duration,
        severity=str(event.get("severity") or "outage"),
        outcome=EpisodeOutcome(resolved_by="self_healed" if resolved else "open", recurred=False),
        symptom=Symptom(
            status_codes=sorted(set(status_codes)),
            error_classes=sorted(set(error_classes))[:8],
            failing_layer=_observed_layer(error_classes),
            severity=str(event.get("severity") or "outage"),
            offline_ratio=round(offline / total, 4),
            transitions=len((state.get("analysis") or {}).get("segments") or []) if isinstance(state.get("analysis"), dict) else 0,
        ),
        signature=signature,
        conclusion=conclusion,
        evidence_ids=sorted(cited(report))[:40],
    )


def _observed_layer(error_classes: list[str]) -> str:
    """症状层 = **外部观测**到故障发生在哪一层（http/timeout/dns/tls…）。"""
    joined = " ".join(error_classes).lower()
    if "timeout" in joined:
        return "timeout"
    if "dns" in joined:
        return "network_dns"
    if "tls" in joined:
        return "tls"
    if "reset" in joined:
        return "connection_reset"
    if "http_status" in joined or "status" in joined:
        return "http"
    return "unknown"


def _conclusion_from(report: dict) -> Conclusion:
    raw = report.get("conclusion")
    if isinstance(raw, dict) and raw.get("text"):
        try:
            return Conclusion.model_validate(raw)
        except Exception:  # noqa: BLE001
            pass
    best = None
    for hypothesis in report.get("hypotheses") or []:
        if hypothesis.get("status") in ("supported", "possible"):
            if best is None or (hypothesis.get("status") == "supported"):
                best = hypothesis
                if hypothesis.get("status") == "supported":
                    break
    if best:
        return Conclusion(
            text=str(best.get("text") or "")[:400],
            layer=str(best.get("layer") or "unknown"),
            confidence="medium" if best.get("status") == "supported" else "low",
            evidence_ids=[str(v) for v in (best.get("supporting_evidence_ids") or [])][:12],
        )
    return Conclusion(text="证据不足以形成结论", layer="unknown", confidence="low")


# ------------------------------------------------------------------ 档案 ----

def baseline_from_checks(rows: list[dict], series: list[dict], interval: int | None, window_s: int = 86_400) -> ServiceBaseline:
    """基线**由原始检查统计**（不靠 LLM）：成功请求的延迟分位 + 在线占比。"""
    latencies = sorted(float(r["latency_ms"]) for r in rows if r.get("status") == "online" and r.get("latency_ms") is not None)
    total = len(rows)
    online = sum(1 for r in rows if r.get("status") == "online")
    if not latencies:
        p50 = p95 = None
    else:
        p50 = latencies[len(latencies) // 2]
        p95 = latencies[min(len(latencies) - 1, int(len(latencies) * 0.95))]
    return ServiceBaseline(
        window_s=window_s, samples=total, p50_ms=p50, p95_ms=p95,
        online_ratio=round(online / total, 4) if total else None, check_interval_s=interval,
    )


def merge_profile(previous: dict | None, service_id: str, card: EpisodeCard, baseline: ServiceBaseline) -> ServiceProfile:
    """把新事件并进服务档案：结论计数 + 模式（样本 <3 标疑似）。"""
    profile = ServiceProfile(service_id=service_id, baseline=baseline)
    if previous:
        try:
            profile = ServiceProfile.model_validate(previous)
            profile.baseline = baseline  # 基线每次重算
        except Exception:  # noqa: BLE001
            pass
    profile.episode_count = int(profile.episode_count or 0) + 1

    text = card.conclusion.text.strip()
    if text:
        found = False
        for cause in profile.known_causes:
            if cause.text == text:
                cause.count += 1
                cause.last_seen = card.started_at
                found = True
                break
        if not found:
            profile.known_causes.append(KnownCause(layer=card.conclusion.layer, text=text,
                                                   count=1, last_seen=card.started_at))
        profile.known_causes.sort(key=lambda c: c.count, reverse=True)
        profile.known_causes = profile.known_causes[:20]

    key = card.profile_key
    if key and key != "sig:none":
        found = False
        for pattern in profile.known_patterns:
            if pattern.key == key:
                pattern.samples += 1
                pattern.tentative = pattern.samples < 3
                found = True
                break
        if not found:
            profile.known_patterns.append(KnownPattern(key=key, text=card.conclusion.text[:200],
                                                       samples=1, tentative=True))
    return profile


def _diagnosis_attachment(state: dict, report: dict, card: EpisodeCard) -> TimelineAttachment:
    """把本次 run 的结论落成一条时间轴附件（只存结论与来源定位，不复制原文）。"""
    run_id = str(state.get("run_id") or card.run_id or "")
    if card.incident_id is not None:
        anchor_type, anchor_id = "incident", str(card.incident_id)
    else:
        anchor_type, anchor_id = "time_window", run_id or card.service_id

    refs: list[SourceRef] = []
    for ident, item in list((state.get("evidence") or {}).items())[:20]:
        if not isinstance(item, dict):
            continue
        refs.append(SourceRef(
            source_type=str(item.get("source_type") or "unknown"),
            source_record_id=str(ident),
            evidence_id=str(ident),
            digest=hashlib.sha256(json.dumps(item.get("data"), sort_keys=True, default=str).encode()).hexdigest()[:16],
        ))

    summary = str(report.get("summary") or "")[:1000]
    passed = bool((report.get("verification") or {}).get("passed"))
    digest = hashlib.sha256(
        json.dumps({"summary": summary, "refs": [r.model_dump() for r in refs]}, sort_keys=True).encode()
    ).hexdigest()
    return TimelineAttachment(
        attachment_id=f"att:{run_id}:diagnosis_report",
        service_id=card.service_id,
        anchor_type=anchor_type,
        anchor_id=anchor_id,
        attachment_type="diagnosis_report",
        source_system="agent_runtime",
        source_record_id=run_id,
        run_id=run_id or None,
        incident_id=card.incident_id,
        occurred_at=card.started_at,
        recorded_at=datetime.now(timezone.utc),
        summary=summary,
        trust_level="validated_agent_report" if passed else "derived_summary",
        source_refs=refs,
        content_digest=digest,
        metadata={"report_version": report.get("version"), "status": report.get("status")},
    )


def remember(state: dict, report: dict, repo, embed, settings) -> dict:
    """写入事件簿 + 更新档案。返回结构化摘要（给日志与 API 展示）。"""
    card = build_card(state, report)
    if not card.service_id:
        return {"saved": False, "reason": "no service"}
    vector = None
    if embed is not None and embed.ready:
        vector = embed.embed([card.render()])[0]
    episode_id = repo.save_episode(card, vector, getattr(embed, "model", "") if embed else "")
    baseline = baseline_from_checks(state.get("raw_checks") or [], state.get("series") or [], state.get("interval"))
    previous = repo.get_profile(card.service_id)
    profile = merge_profile(previous, card.service_id, card, baseline)
    repo.upsert_profile(profile)
    # 时间轴附件与旧事件簿并存：重复 remember（resume/重试）由唯一约束保证幂等。
    attachment_saved = False
    try:
        attachment_saved = repo.save_attachment(_diagnosis_attachment(state, report, card))
    except Exception as exc:  # 记忆写入失败绝不能打断调查
        log.warning("timeline attachment not saved: %s", exc)
    return {"saved": True, "episode_id": episode_id, "profile_key": card.profile_key,
            "episodes": profile.episode_count, "embedded": bool(vector),
            "attachment_saved": attachment_saved}
