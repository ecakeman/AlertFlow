import logging
import threading

from alertflow_agent.config import Settings
from alertflow_agent.db import MemoryStore, PostgresStore, ensure_database
from alertflow_agent.integrations.alertflow import AlertFlowClient
from alertflow_agent.integrations.embeddings import EmbedClient, RerankClient
from alertflow_agent.memory_store import InMemoryMemoryRepo, MemoryRepo
from alertflow_agent.runtime.graph import Runner
from alertflow_agent.runtime.llm import OpenAIModel, ScriptedModel

log = logging.getLogger("alertflow_agent")


def build(settings: Settings | None = None):
    settings = settings or Settings()
    if settings.llm_api_key and (not settings.llm_model or not settings.llm_base_url):
        raise RuntimeError("LLM_MODEL and LLM_BASE_URL are required when LLM_API_KEY is set")
    checkpoint = None
    if settings.agent_database_url:
        ensure_database(settings.agent_database_url)
        store = PostgresStore(settings.agent_database_url)
        checkpoint = _checkpointer(settings.agent_database_url)
    else:
        store = MemoryStore()
    model = OpenAIModel(settings) if settings.llm_api_key else ScriptedModel()
    client = AlertFlowClient(settings.alertflow_base_url, settings.alertflow_agent_token)
    memory = None
    if settings.memory_enabled:
        memory = MemoryRepo(settings.agent_database_url) if settings.agent_database_url else InMemoryMemoryRepo()
    embed = EmbedClient(settings.embed_base_url, settings.embed_api_key, settings.embed_model, settings.embed_dim)
    rerank = RerankClient(settings.rerank_base_url, settings.rerank_api_key, settings.rerank_model, settings.rerank_style)
    runner = Runner(settings, store, client, model, checkpointer=checkpoint[0] if checkpoint else None,
                    memory=memory, embed=embed, rerank=rerank)
    return settings, store, runner, checkpoint


def _checkpointer(url: str):
    from langgraph.checkpoint.postgres import PostgresSaver

    opened = PostgresSaver.from_conn_string(url)
    saver = opened.__enter__()
    saver.setup()
    return saver, opened


def serve(settings: Settings | None = None) -> None:
    import uvicorn

    from alertflow_agent.api.app import create_app
    from alertflow_agent.callbacks import CallbackSender

    settings, store, runner, checkpoint = build(settings)
    stop = threading.Event()
    worker = threading.Thread(target=_loop, args=(settings, store, runner, stop), name="agent-worker")
    worker.start()
    # 诊断结果通过这条线程回传给 AlertFlow；它发不发得出去不影响 run 本身。
    callbacks = CallbackSender(settings, store)
    sender = threading.Thread(target=callbacks.run, args=(stop,), name="agent-callback")
    sender.start()
    app = create_app(settings, store, runner)

    @app.on_event("shutdown")
    def _shutdown():
        stop.set()
        worker.join(timeout=settings.run_timeout_seconds)
        sender.join(timeout=5)
        if checkpoint is not None:
            checkpoint[1].__exit__(None, None, None)
        closer = getattr(store, "conn", None)
        if closer is not None:
            closer.close()

    uvicorn.run(app, host="0.0.0.0", port=8000)


def _loop(settings, store, runner, stop: threading.Event) -> None:
    for run_id in store.running_ids():
        store.transition(run_id, {"running"}, status="queued", stage="resume")
    active = 0
    lock = threading.Lock()

    def run(row):
        nonlocal active
        try:
            runner.execute(row["id"], resume=row.get("stage") == "resume")
        except Exception:
            log.exception("run failed")
            store.finish(row["id"], status="failed", failure_reason="internal_error", stage="failed")
        finally:
            with lock:
                active -= 1

    threads = []
    while not stop.is_set():
        with lock:
            room = settings.max_concurrent_runs - active
        if room > 0:
            row = store.claim_queued()
            if row and row.get("status") != "cancelled":
                with lock:
                    active += 1
                thread = threading.Thread(target=run, args=(row,), name=f"run-{row['id']}")
                thread.start()
                threads.append(thread)
        stop.wait(0.5)
    for thread in threads:
        thread.join(timeout=settings.run_timeout_seconds)
