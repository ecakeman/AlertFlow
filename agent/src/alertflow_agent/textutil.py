import hashlib
import math
import os
import re

_CJK = re.compile(r"([\u4e00-\u9fff])")


def embedding_dim() -> int:
    return int(os.environ.get("ALERTFLOW_EMBEDDING_DIM", "384"))


def segment(text: str) -> str:
    """给 simple 分词器插入空格，使中文单字可被全文检索。"""
    spaced = _CJK.sub(r" \1 ", text or "")
    return re.sub(r"\s+", " ", spaced).strip()


def embed(text: str, dim: int | None = None) -> list[float]:
    size = dim or embedding_dim()
    vec = [0.0] * size
    tokens = segment(text).lower().split()
    if not tokens:
        tokens = ["empty"]
    for token in tokens:
        digest = hashlib.sha256(token.encode()).digest()
        idx = int.from_bytes(digest[:4], "little") % size
        sign = 1.0 if digest[4] % 2 == 0 else -1.0
        vec[idx] += sign
    norm = math.sqrt(sum(value * value for value in vec)) or 1.0
    return [value / norm for value in vec]


def content_hash(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def search_text(summary: str, symptoms: list[str], observations: list[str], diagnosis: list[str]) -> str:
    def block(items: list[str]) -> str:
        return "\n".join(items)

    return (
        f"故障概括：{summary}\n\n"
        f"异常表现：\n{block(symptoms)}\n\n"
        f"调查发现：\n{block(observations)}\n\n"
        f"诊断结果：\n{block(diagnosis)}\n"
    )


def rrf(rankings: list[list[str]], k: int = 60) -> dict[str, float]:
    scores: dict[str, float] = {}
    for ranking in rankings:
        for rank, fd_id in enumerate(ranking, start=1):
            scores[fd_id] = scores.get(fd_id, 0.0) + 1.0 / (k + rank)
    return scores


def token_overlap(query: str, document: str) -> float:
    q = set(segment(query).lower().split())
    d = set(segment(document).lower().split())
    if not q:
        return 0.0
    return len(q & d) / len(q)
