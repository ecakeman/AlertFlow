# P1 验收

相对产品基线 `169ccab`。主图没有改成三 Specialist。

| 编号 | 要求 | 实现 | 测试 |
| --- | --- | --- | --- |
| P1-A2A-WORKING | 接受任务后可查询 `working`，`SendMessage` 不等待 RAGFlow | `RunbookExecutor.execute`、`_run` | `test_working_duplicate_stays_working_until_the_lookup_finishes` |
| P1-A2A-DUPLICATE | 终态返回原任务；执行中返回 `working`；过期失败且不重跑 | `RunbookExecutor.read_task` | `test_expired_working_task_fails_without_another_lookup`、`test_repeat_message_does_not_call_ragflow_again` |
| P1-A2A-GET | `GetTask` 也能收敛超时或重启后的 `working` | `LedgerTaskStore.get` | `test_restart_fails_a_working_task_without_rerunning`、`test_postgres_restart_fails_the_saved_working_task` |
| P1-A2A-FAILED | `FAILED` 不写成成功 evidence | `integrations/runbook.py` 的 `consult` | `test_failed_lookup_is_a_coverage_gap` |
| P1-A2A-MIME | Card 输入与发送的 Part 都是 `application/json` | `build_card`、`consult` | `test_card_declares_only_runbook_research` |
| P1-RUNBOOK-CATALOG | `source_type=runbook` 进入现有 fact catalog | `verify._catalog_payload`、`_consult_runbook` | `test_runbook_catalog_keeps_content_and_only_real_citations` |
| P1-RAGFLOW-CONFIG | URL、API key、agent id 缺任一则不发请求 | `ragflow.research` | `test_missing_ragflow_api_key_does_not_post` |
| P1-PROM-UP | `up` 只表示 scrape 成功 | `runtime/tools.py` 的 `_prom_metric` | `test_configured_query_writes_scoped_evidence_once` |
| P1-CONFIG-PRIORITY | shell、`.env`、Compose 空默认 | `docker-compose.yml` 的 `${VAR:-}`；脚本不再 export | `test_shell_overrides_dotenv`、`test_dotenv_overrides_the_empty_default`、`test_unset_shell_and_dotenv_keep_the_empty_default` |
| P1-PROFILE-RUNBOOK | profile 名是 `runbook`；agent 不加入 `ragflow` 网络 | `docker-compose.yml` 的 `a2a-adapter` | `test_default_compose_keeps_optional_urls_empty` |
| P1-LLM-ATTEMPTS | 调用前计数，失败也计数，成功次数分开 | `runtime/model.py` 的 `_reserve` | `test_failed_attempt_then_retry_counts_both`、`test_two_failures_do_not_count_as_success`、`test_retry_stops_when_the_attempt_budget_is_spent`、`test_second_call_stops_when_tokens_are_exhausted`、`test_two_workers_share_one_attempt_budget` |
| P1-PYTHON-LOCK | 本地、CI、镜像使用同一 `agent/uv.lock` | `agent/Dockerfile`、`.github/workflows/test.yml` | `uv export --frozen` 能读出 lock。干净镜像构建这次没有跑，CI 会跑 `images` job |

真实 RAGFlow completion、真实 Prometheus/Loki 查询、Fault Lab 进程：`SKIPPED`。没有用 mock 代替这些运行。

# P1.1 验收

| 编号 | 要求 | 实现 | 测试 |
| --- | --- | --- | --- |
| P1.1-A2A-CANCEL | 取消同时写 status、cancelled、artifact 和 task_json；alias 可取消；终态不覆盖；迟到结果仍是 canceled；context_id 保留 | `RunbookExecutor.persist_cancel`、`_store` | `test_cancel_matches_task_json_and_keeps_context`、`test_cancel_is_idempotent_and_a_late_result_cannot_complete_it`、`test_cancel_does_not_replace_a_completed_task`、`test_postgres_cancel_matches_task_json_for_an_alias` |
| P1.1-REPORT-BUDGET | 报告次数和 token 从现有上限预留，调查不能提前花掉 | `runtime/model.py` 的 `_report_attempt_reserve`、`_report_token_reserve` | `test_investigation_cannot_spend_the_report_reserve`、`test_report_attempts_stop_at_the_reserve`、`test_report_tokens_stop_at_the_reserve_and_the_global_cap`、`test_report_retries_count_and_two_callers_share_the_reserve` |
| P1.1-PROM-UP | `sum(up)` 也标成 scrape 成功；`probe_success` 和 label 名 `up` 不标 | `_prom_metric` 复用 `_mask_promql` | `test_configured_query_writes_scoped_evidence_once` |

# P2 验收

fake client 不是真实集群。真实集群：`NOT RUN`。

| 编号 | 要求 | 实现 | 测试 |
| --- | --- | --- | --- |
| P2-TARGETS | 一个 service 可挂多个 Deployment/StatefulSet；同一对象不能映射两次；旧字段冲突被拒绝 | `settings.kubernetes.targets`、`normalizeKubernetesTargets`、`Clone` | `TestKubernetesTargetsKeepKindInTheIdentity`、`TestKubernetesMappingValidation` |
| P2-STATEFULSET | Pod 只沿 owner UID 归属；同名 Deployment 与 StatefulSet 不串服务；Event 按 InvolvedObject | `workloadOfPod`、StatefulSet informer、`listNamespace` | `TestStatefulSetStaysOnItsOwnService` |
| P2-RECOVER | 初次连接失败后，下一次 Sync 重新 Connect 并 List；写入失败保持 `gap` | `Observer.Connect`、`SyncTargets`、`failSource` | `TestSyncReconnectsAfterTheInitialFailure`、`TestObjectWriteFailureMarksAGap` |
| P2-RBAC | 命名空间内只读 Role，含 statefulsets，无 Secret 和写权限 | `deploy/k8s/observer-rbac.yaml` | 清单审查。没有应用到集群 |
