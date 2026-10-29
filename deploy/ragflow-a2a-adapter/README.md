# RAGFlow A2A adapter

The `multi-agent` profile runs `python -m alertflow_agent.a2a_adapter`. It uses `a2a-sdk==1.3.0` HTTP+JSON. The Agent Card is `GET /.well-known/agent-card.json` and declares one skill, `runbook_research`.

`POST /message:send` turns the user text into RAGFlow v0.27.3 `POST /api/v1/agents/chat/completions` (`agent_id`, `query`, `stream: false`) and returns a `runbook-research.v1` artifact. `GET /tasks/{id}` reads the same row after a restart. The row is `a2a_adapter_tasks` in the AlertFlow Postgres database. A repeated message id does not call RAGFlow again.

`RAGFLOW_BASE_URL` empty means the task fails without opening a connection. `RAGFLOW_API_KEY` stays in the adapter environment. It is not written to the artifact, the agent report, or evidence.

The main agent calls `consult_runbook_agent` only when `A2A_ADAPTER_URL` is set, and at most once per run. The request contains the service, the incident time, verified observations, the hypothesis, and the question. A failure is a coverage gap. The report still goes through the existing verifier. A citation is recorded only when both a document id and a chunk id are present.

Start it with `scripts/up-agent-ragflow.sh`. Stop it with `docker compose --profile ragflow --profile multi-agent --profile agent down`. That command does not delete volumes.
