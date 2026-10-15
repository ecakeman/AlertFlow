from pydantic_settings import BaseSettings, SettingsConfigDict


class Settings(BaseSettings):
    model_config = SettingsConfigDict(env_file=".env", extra="ignore")

    agent_webhook_token: str = ""
    agent_api_token: str = ""
    alertflow_base_url: str = "http://127.0.0.1:8080"
    alertflow_agent_token: str = ""
    agent_database_url: str = ""
    diagnostics_file: str = ""
    llm_api_key: str = ""
    llm_base_url: str = ""
    llm_model: str = "gpt-4o-mini"
    agent_enable_warning_runs: bool = False
    agent_revise_on_resolve: bool = False
    check_lookback_minutes: int = 60
    check_max_rows: int = 500
    # --- 入站与按需查询的边界 ---
    # webhook_max_bytes 与 Go 侧 AgentMaxPayloadBytes 对齐：下推的默认上下文比早期
    # 事件大得多，64 KiB 装不下。
    webhook_max_bytes: int = 512 * 1024
    context_max_checks: int = 200
    context_max_metric_points: int = 200
    context_max_text: int = 2000
    # 回头找 AlertFlow 要数据的工具（检查历史/指标/Incident 详情）每次 run 的次数上限。
    # 默认上下文已经把基础数据下推了，所以这些是补充手段，不是主路径。
    max_query_calls: int = 8
    max_lead_rounds: int = 8
    max_llm_calls: int = 24
    max_tool_calls: int = 14
    max_replans: int = 2
    max_revisions: int = 2
    max_subagent_runs: int = 2
    subagent_max_rounds: int = 4
    max_total_tokens: int = 150_000
    # 单次 prompt 的上下文预算（与 run 级累计预算 max_total_tokens 是两件事）
    context_budget_tokens: int = 24_000
    run_timeout_seconds: int = 300
    report_grace_seconds: int = 90
    max_concurrent_runs: int = 2

    # --- 长期记忆（事件簿 / 服务档案）---
    memory_enabled: bool = True
    memory_lookback_days: int = 30          # 预筛窗口
    memory_recall_k: int = 20               # 向量召回条数
    memory_rerank_k: int = 5                # rerank 后保留条数
    # 嵌入（OpenAI 兼容）：POST {base}/embeddings
    embed_base_url: str = ""
    embed_api_key: str = ""
    embed_model: str = ""
    embed_dim: int = 1024                   # 必须与 schema.sql 的 vector(N) 一致
    # 重排：style=dashscope 走阿里云原生 text-rerank；style=openai 走 {base}/rerank
    rerank_base_url: str = ""
    rerank_api_key: str = ""
    rerank_model: str = ""
    rerank_style: str = "dashscope"
