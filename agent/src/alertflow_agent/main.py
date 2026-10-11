import threading
import time

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore, PostgresStore, ensure_database
from alertflow_agent.integrations.alertflow import AlertFlowClient
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import OpenAIModel, ScriptedModel


def build(settings: Settings | None = None):
    settings = settings or Settings()
    if settings.agent_database_url:
        ensure_database(settings.agent_database_url)
        store = PostgresStore(settings.agent_database_url)
    else:
        store = MemoryStore()
    model = OpenAIModel(settings) if settings.llm_api_key else ScriptedModel()
    client = AlertFlowClient(settings.alertflow_base_url, settings.alertflow_agent_token)
    runner = Runner(settings, store, client, model, checkpointer=_checkpointer(settings))
    return settings, store, runner


def _checkpointer(settings: Settings):
    if not settings.agent_database_url:
        return None
    try:
        from langgraph.checkpoint.postgres import PostgresSaver

        opened = PostgresSaver.from_conn_string(settings.agent_database_url)
        saver = opened.__enter__() if hasattr(opened, "__enter__") else opened
        saver.setup()
        return saver
    except Exception:
        return None


def serve(settings: Settings | None = None) -> None:
    import uvicorn

    from alertflow_agent.api.app import create_app

    settings, store, runner = build(settings)
    threading.Thread(target=_loop, args=(settings, store, runner), daemon=True).start()
    uvicorn.run(create_app(settings, store, runner), host="0.0.0.0", port=8000)


def _loop(settings, store, runner) -> None:
    for run_id in store.running_ids():
        store.update(run_id, status="queued", stage="resume")
    active = 0
    lock = threading.Lock()

    def run(row):
        nonlocal active
        try:
            runner.execute(row["id"], resume=row.get("stage") == "resume")
        except Exception:
            store.update(row["id"], status="failed", failure_reason="internal_error", stage="failed")
        finally:
            with lock:
                active -= 1

    while True:
        with lock:
            room = settings.max_concurrent_runs - active
        if room > 0:
            row = store.claim_queued()
            if row and row.get("status") != "cancelled":
                with lock:
                    active += 1
                threading.Thread(target=run, args=(row,), daemon=True).start()
        time.sleep(0.5)


if __name__ == "__main__":
    serve()
