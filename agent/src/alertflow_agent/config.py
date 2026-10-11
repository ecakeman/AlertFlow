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
    max_lead_rounds: int = 8
    max_llm_calls: int = 24
    max_tool_calls: int = 14
    max_replans: int = 2
    max_revisions: int = 2
    max_subagent_runs: int = 2
    subagent_max_rounds: int = 4
    max_total_tokens: int = 150_000
    run_timeout_seconds: int = 300
    report_grace_seconds: int = 90
    max_concurrent_runs: int = 2
