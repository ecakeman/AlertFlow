"""记忆的检索路径：预筛 → 向量召回 → rerank → 组装「既往上下文」。

三段分工是刻意的：
- 预筛用关系型（服务 + 时间窗）——记忆的第一维永远是「哪个服务」，这一步便宜且准；
- 向量召回解决「措辞不同但同源」；
- rerank 把「同一个根因、不同表述」从「表面像、其实无关」里挑出来。
未配置嵌入时**优雅退化**到「最近事件」，而不是没有记忆。
"""

from __future__ import annotations

import logging

from ..analysis.errors import classify_error
from ..analysis.stats import error_stats
from ..analysis.timeline import status_counts
from .memory import _observed_layer, signature_key  # noqa: PLC2701 - 复用同一套抽取口径

log = logging.getLogger("alertflow_agent")


def query_card(state: dict) -> str:
    """用**当前**症状与内部证据签名叫出查询串——与事件卡同一口径，保证可比。"""
    event = state.get("event") or {}
    rows = state.get("raw_checks") or []
    counts = status_counts(rows)
    total = max(1, int(counts.get("total") or 0))
    offline = int((counts.get("offline") or {}).get("n") or 0)
    errors = error_stats(rows, classify_error) or {}
    codes = [int(k) for k, v in (errors.get("status_codes") or {}).items() if v]
    classes = [k for k, v in (errors.get("classes") or {}).items() if v]
    names = []
    for ident, item in (state.get("evidence") or {}).items():
        data = item.get("data") if isinstance(item.get("data"), dict) else {}
        if item.get("source_type") == "project_diagnostic" and data.get("status") in ("degraded", "unhealthy"):
            names.append(f"{data.get('kind')}:{data.get('name')}")
    return (
        f"服务 {event.get('service_id')}｜{event.get('severity')}｜"
        f"症状: {','.join('HTTP '+str(c) for c in sorted(set(codes))[:4])}；错误类: {','.join(sorted(set(classes))[:4])}；"
        f"offline 占比 {offline/total:.0%}｜内部: {', '.join(sorted(set(names))[:6])}"
    )


def current_signature_key(state: dict) -> str:
    from ..schemas.memory import EvidenceSignature, SignatureItem

    items = []
    for item in (state.get("evidence") or {}).values():
        data = item.get("data") if isinstance(item.get("data"), dict) else {}
        if (item.get("source_type") == "project_diagnostic" and data.get("kind") != "service_info"
                and data.get("status") in ("degraded", "unhealthy")):
            items.append(SignatureItem(kind=str(data.get("kind")), name=str(data.get("name")), status=data.get("status")))
    return signature_key(EvidenceSignature(abnormal=items))


def recall_prior(state: dict, repo, embed, rerank, settings) -> dict | None:
    """返回 {profile, hits, query, same_key_hits}；没有任何历史时返回 None（而非空壳）。"""
    service_id = (state.get("event") or {}).get("service_id")
    if not service_id or repo is None:
        return None
    profile = repo.get_profile(service_id)
    recent = repo.list_recent(service_id, settings.memory_lookback_days, 50)
    if not recent and not profile:
        return None

    query = query_card(state)
    hits: list[dict] = []
    if embed is not None and getattr(embed, "ready", False):
        vector = embed.embed([query])[0]
        if vector:
            candidates = repo.search(service_id, vector, settings.memory_recall_k)
            if candidates and rerank is not None and getattr(rerank, "ready", False):
                docs = [item["card"] for item, _ in candidates]
                pairs = rerank.rerank(query, docs, settings.memory_rerank_k)
                if pairs:
                    hits = [{**candidates[i][0], "vector_score": candidates[i][1], "rerank_score": score}
                            for i, score in pairs if 0 <= i < len(candidates)]
            if not hits:
                hits = [{**item, "vector_score": score} for item, score in candidates[: settings.memory_rerank_k]]
    if not hits:
        # 退化：没有嵌入/召回失败时，用最近事件顶上（记忆仍在，只是不排序）
        hits = [{**item, "vector_score": None} for item in recent[: settings.memory_rerank_k]]

    key = current_signature_key(state)
    same_key = [h for h in recent if key != "sig:none" and h.get("conclusion") and _key_of(h) == key]
    # 时间轴附件（C3）与事件簿一并召回；读失败只丢附件，不影响事件簿结论。
    attachments: list[dict] = []
    try:
        attachments = repo.list_attachments(service_id, limit=20)
    except Exception:
        attachments = []
    return {"profile": profile, "hits": hits, "query": query, "signature_key": key,
            "same_key_hits": same_key, "attachments": attachments}


def _key_of(episode: dict) -> str | None:
    # 事件里存了 profile_key 吗？存在 start 时未必回填，这里用结论层兜底。
    return episode.get("profile_key")


def render_block(prior: dict | None) -> str | None:
    """把检索结果压成一块**紧凑**的既往上下文：档案摘要 + k 条一句话结论（带 ep:id）。"""
    if not prior:
        return None
    lines: list[str] = []
    profile = prior.get("profile") or {}
    baseline = profile.get("baseline") or {}
    if baseline:
        lines.append(
            f"已知基线: 采样 {baseline.get('samples')} 次，p50={baseline.get('p50_ms')}ms、p95={baseline.get('p95_ms')}ms，"
            f"在线率 {baseline.get('online_ratio')}，检查间隔 {baseline.get('check_interval_s')}s"
        )
    causes = profile.get("known_causes") or []
    if causes:
        top = "；".join(f"{c.get('text')}（{c.get('count')} 次）" for c in causes[:4])
        lines.append(f"已知根因: {top}")
    patterns = profile.get("known_patterns") or []
    if patterns:
        seen = "；".join(f"{p.get('text')}（样本 {p.get('samples')}{'，疑似' if p.get('tentative') else ''}）" for p in patterns[:3])
        lines.append(f"疑似模式: {seen}")
    if prior.get("signature_key") and prior["signature_key"] != "sig:none":
        lines.append(f"本次内部签名键: {prior['signature_key']}")
    if not lines:
        lines.append("（无既往档案）")
    lines.append("既往相似事件（结论是**当时的判断**，需用本次证据复核）:")
    for hit in prior.get("hits") or []:
        conclusion = (hit.get("conclusion") or {}).get("text") or "—"
        when = (hit.get("started_at") or "?")[:16].replace("T", " ")
        score = hit.get("rerank_score")
        score_text = f"rerank={score:.3f}" if isinstance(score, (int, float)) else (
            f"cos={hit['vector_score']:.3f}" if isinstance(hit.get("vector_score"), (int, float)) else "recency")
        lines.append(f"  ep:{hit['id']}｜{when}｜{score_text}｜{conclusion}")
    attachments = prior.get("attachments") or []
    if attachments:
        lines.append("近期时间轴附件（历史结论/专家建议，需用本次证据复核）:")
        for item in attachments[:5]:
            when = str(item.get("occurred_at") or item.get("recorded_at") or "?")[:16].replace("T", " ")
            text = str(item.get("summary") or "").replace("\n", " ")[:160]
            lines.append(f"  {item.get('attachment_type')}｜{item.get('trust_level')}｜{when}｜{text}")
    return "既往上下文（来自长期记忆）:\n" + "\n".join(lines)
