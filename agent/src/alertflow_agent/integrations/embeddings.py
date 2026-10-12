"""嵌入与重排客户端。

两者都可能是"没配置"的——那时记忆功能**优雅降级**（不做向量召回），而不是报错。
按内容 hash 缓存，重复的事件卡不重复花钱。

嵌入：OpenAI 兼容 `POST {base}/embeddings`。
重排：`rerank_style=dashscope` 走阿里云原生 `text-rerank`；`openai` 走 `{base}/rerank`。
"""

from __future__ import annotations

import hashlib
import logging

import httpx

log = logging.getLogger("alertflow_agent")

_EMBED_CACHE: dict[str, list[float]] = {}
_CACHE_LIMIT = 512


def text_hash(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()[:32]


class EmbedClient:
    def __init__(self, base_url: str, api_key: str, model: str, dim: int = 1024) -> None:
        self.base = (base_url or "").rstrip("/")
        self.key = api_key or ""
        self.model = model or ""
        self.dim = dim

    @property
    def ready(self) -> bool:
        return bool(self.base and self.key and self.model)

    def embed(self, texts: list[str]) -> list[list[float] | None]:
        """返回与输入等长的向量列表；未配置或调用失败时对应项为 None。"""
        if not self.ready or not texts:
            return [None] * len(texts)
        out: list[list[float] | None] = [None] * len(texts)
        pending: list[tuple[int, str]] = []
        for index, text in enumerate(texts):
            cached = _EMBED_CACHE.get(text_hash(text))
            if cached is not None:
                out[index] = cached
            else:
                pending.append((index, text))
        if pending:
            vectors = self._call([text for _, text in pending])
            for (index, text), vector in zip(pending, vectors):
                out[index] = vector
                if vector is not None:
                    if len(_EMBED_CACHE) >= _CACHE_LIMIT:
                        _EMBED_CACHE.clear()
                    _EMBED_CACHE[text_hash(text)] = vector
        return out

    def _call(self, texts: list[str]) -> list[list[float] | None]:
        try:
            response = httpx.post(
                f"{self.base}/embeddings",
                headers={"Authorization": f"Bearer {self.key}", "Content-Type": "application/json"},
                json={"model": self.model, "input": texts, "dimensions": self.dim},
                timeout=20,
            )
            if response.status_code >= 300:
                log.warning("embed failed", extra={"status": response.status_code})
                return [None] * len(texts)
            data = response.json().get("data") or []
            ordered = sorted(data, key=lambda item: item.get("index", 0))
            return [item.get("embedding") for item in ordered] + [None] * (len(texts) - len(ordered))
        except Exception as exc:  # noqa: BLE001 - memory must never break a run
            log.warning("embed error: %s", type(exc).__name__)
            return [None] * len(texts)


class RerankClient:
    def __init__(self, base_url: str, api_key: str, model: str, style: str = "dashscope") -> None:
        self.base = (base_url or "").rstrip("/")
        self.key = api_key or ""
        self.model = model or ""
        self.style = style or "dashscope"

    @property
    def ready(self) -> bool:
        return bool(self.base and self.key and self.model)

    def rerank(self, query: str, documents: list[str], top_k: int = 5) -> list[tuple[int, float]] | None:
        """返回 [(原索引, 分数)]，按分数降序；未配置或失败返回 None（调用方退回向量序）。"""
        if not self.ready or not documents:
            return None
        try:
            if self.style == "openai":
                response = httpx.post(
                    f"{self.base}/rerank",
                    headers={"Authorization": f"Bearer {self.key}", "Content-Type": "application/json"},
                    json={"model": self.model, "query": query, "documents": documents, "top_n": top_k},
                    timeout=20,
                )
                if response.status_code >= 300:
                    return None
                results = response.json().get("results") or []
                pairs = [(int(r.get("index", 0)), float(r.get("relevance_score", 0.0))) for r in results]
            else:  # dashscope 原生
                response = httpx.post(
                    f"{self.base}/api/v1/services/rerank/text-rerank/text-rerank",
                    headers={"Authorization": f"Bearer {self.key}", "Content-Type": "application/json"},
                    json={
                        "model": self.model,
                        "input": {"query": query, "documents": documents},
                        "parameters": {"return_documents": False, "top_n": top_k},
                    },
                    timeout=20,
                )
                if response.status_code >= 300:
                    return None
                results = (response.json().get("output") or {}).get("results") or []
                pairs = [(int(r.get("index", 0)), float(r.get("relevance_score", 0.0))) for r in results]
            pairs.sort(key=lambda item: item[1], reverse=True)
            return pairs[:top_k]
        except Exception as exc:  # noqa: BLE001
            log.warning("rerank error: %s", type(exc).__name__)
            return None
