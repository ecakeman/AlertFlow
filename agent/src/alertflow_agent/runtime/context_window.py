"""上下文窗口管理器：按**优先级**把有限的上下文预算分配出去。

核心不是"拼字符串"，而是：**块 → 层级 → 预算 → 降级阶梯**。

- 每个块提供若干「档位」（从详细到精简），最后可为空（表示可整体丢弃）；
- 装配时先按 ORDER 拼好，若超预算就**从最低层级、最靠后的块开始逐档降级**，
  而不是把尾巴一刀切断；
- PIN 层的块永不被丢弃；
- 整个过程产出结构化 report，供观测与断言。
"""

from __future__ import annotations

from dataclasses import dataclass, field

PIN, HIGH, MEDIUM, LOW = 0, 1, 2, 3
TIER_NAMES = {PIN: "pin", HIGH: "high", MEDIUM: "medium", LOW: "low"}


def estimate_tokens(text: str) -> int:
    """对中文诚实的分词估算：CJK 约 1 字/token，其余约 4 字符/token。

    原来的 ``len(text)//4`` 对中文低估约 4 倍——上下文预算的账会算错。
    """
    if not text:
        return 0
    cjk = 0
    for ch in text:
        code = ord(ch)
        if 0x3000 <= code <= 0x303F or 0x4E00 <= code <= 0x9FFF or 0x3400 <= code <= 0x4DBF or 0xFF00 <= code <= 0xFFEF:
            cjk += 1
    return max(1, cjk + (len(text) - cjk) // 4)


@dataclass
class Block:
    name: str
    tier: int
    # variants[0] 最详细，越往后越精简；最后一项为空串＝可以整体丢弃。
    variants: list[str] = field(default_factory=list)
    # protect：可以降级，但**永不被丢弃**（PIN 层、以及"丢了就没法引用"的关键证据）。
    protect: bool = False

    @property
    def droppable(self) -> bool:
        if self.protect:
            return False
        return not self.variants or self.variants[-1] == ""

    @property
    def min_index(self) -> int:
        """能降到的最低档位。受保护的块不许降到空档（那等于悄悄丢了）。"""
        if not self.variants:
            return 0
        if not self.protect:
            return len(self.variants) - 1
        for i in range(len(self.variants) - 1, -1, -1):
            if self.variants[i]:
                return i
        return 0

    def floor_tokens(self) -> int:
        """降到最低档仍占用的 token；不可丢弃的块贡献地板。"""
        if not self.protect or not self.variants:
            return 0
        return estimate_tokens(self.variants[self.min_index])


@dataclass
class Fit:
    """装配结果：只读、可序列化，直接落事件。"""

    text: str
    tokens: int
    budget: int
    floor: int = 0
    blocks: list[dict] = field(default_factory=list)
    # 每个块最终选中的文本（与 blocks 同序，丢弃的为空串）。
    # 供上层把叶子块重组为固定六区块，同时保留细粒度裁剪。
    texts: list[str] = field(default_factory=list)

    @property
    def trimmed(self) -> bool:
        return any(b["action"] != "kept" for b in self.blocks)

    @property
    def over_floor(self) -> bool:
        """预算低于不可再压缩的地板——这不是装配失败，是配置不合理。"""
        return self.floor > self.budget

    def summary(self) -> dict:
        return {
            "budget": self.budget,
            "tokens": self.tokens,
            "floor": self.floor,
            "over_floor": self.over_floor,
            "trimmed": self.trimmed,
            "dropped": [b["name"] for b in self.blocks if b["action"] == "dropped"],
            "degraded": [b["name"] for b in self.blocks if b["action"] == "degraded"],
            "by_tier": _by_tier(self.blocks),
        }


def _by_tier(blocks: list[dict]) -> dict:
    out: dict = {}
    for block in blocks:
        row = out.setdefault(block["tier"], {"n": 0, "tokens": 0})
        row["n"] += 1
        row["tokens"] += block["tokens"]
    return out


def assemble(blocks: list[Block], budget: int) -> Fit:
    """把块装进预算：先全量，超了就从低层级逐档降级，最后才丢。"""
    index = [0] * len(blocks)
    dropped = [False] * len(blocks)

    def tokens_at(i: int) -> int:
        if dropped[i]:
            return 0
        return estimate_tokens(blocks[i].variants[index[i]])

    def total() -> int:
        return sum(tokens_at(i) for i in range(len(blocks)))

    # 降级顺序：层级低者先，同层靠后者先（越靠后越"可牺牲"）。
    order = sorted(range(len(blocks)), key=lambda i: (-blocks[i].tier, -i))

    while total() > budget:
        if not _step_down(blocks, index, dropped, order):
            break

    rendered = []
    report = []
    texts: list[str] = []
    for i, block in enumerate(blocks):
        if dropped[i]:
            report.append({"name": block.name, "tier": TIER_NAMES[block.tier], "variant": -1,
                           "tokens": 0, "action": "dropped"})
            texts.append("")
            continue
        text = block.variants[index[i]]
        if not text:
            report.append({"name": block.name, "tier": TIER_NAMES[block.tier], "variant": index[i],
                           "tokens": 0, "action": "dropped"})
            texts.append("")
            continue
        rendered.append(text)
        texts.append(text)
        report.append({
            "name": block.name, "tier": TIER_NAMES[block.tier], "variant": index[i],
            "tokens": estimate_tokens(text),
            "action": "kept" if index[i] == 0 else "degraded",
        })
    text = "\n\n".join(rendered)
    return Fit(text=text, tokens=estimate_tokens(text), budget=budget,
               floor=sum(b.floor_tokens() for b in blocks), blocks=report, texts=texts)


def _step_down(blocks: list[Block], index: list[int], dropped: list[bool], order: list[int]) -> bool:
    """降一档：先降档位；降到最低仍超预算，才丢**未受保护**的块。"""
    # 第一步：所有块都可以降档（受保护的降到 min_index 为止）。
    for i in order:
        if dropped[i]:
            continue
        if index[i] < blocks[i].min_index:
            index[i] += 1
            return True
    # 第二步：只有**未受保护**的块才允许整体丢弃。
    for i in order:
        if dropped[i] or blocks[i].protect:
            continue
        if blocks[i].droppable:
            dropped[i] = True
            return True
    return False
