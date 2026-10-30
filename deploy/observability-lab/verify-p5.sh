#!/usr/bin/env bash
# P5 观测链路三层一致性验证：原生 Prometheus API / 原生 LogQL / MCP。
#
# 用法（在集群 Ready、核心栈与 observability overlay 都 apply 之后）：
#   deploy/observability-lab/verify-p5.sh
#   PROM_URL=http://127.0.0.1:19090 LOKI_URL=http://127.0.0.1:13100 \
#   PROM_MCP_URL=http://127.0.0.1:19091 LOKI_MCP_URL=http://127.0.0.1:19092 \
#     deploy/observability-lab/verify-p5.sh
#
# 默认通过 kubectl port-forward 把四个 Service 映射到本机高位端口，脚本退出时
# 自动清理。不修改集群，不写任何东西。依赖：bash、curl、python3、kubectl。
#
# 退出码：0 全部 PASS；1 有 FAIL；2 前置条件不满足（集群/端口/工具缺失）。
set -uo pipefail

# 证据留存：所有输出同时写进 evidence/p5-verify-<UTC>.log（可用 EVIDENCE_DIR 覆盖）。
SCRIPT_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
EVIDENCE_DIR=${EVIDENCE_DIR:-"$SCRIPT_DIR/evidence"}
mkdir -p "$EVIDENCE_DIR" 2>/dev/null || true
EVIDENCE_LOG="$EVIDENCE_DIR/p5-verify-$(date -u +%Y%m%dT%H%M%SZ).log"
exec > >(tee -a "$EVIDENCE_LOG") 2>&1
log() { printf '[%s] %s\n' "$(date -u +%H:%M:%SZ)" "$*"; }
log "evidence log -> $EVIDENCE_LOG"

NS=observability
PF_PIDS=""
FAILS=0
WARNS=0

cleanup() {
  for pid in $PF_PIDS; do kill "$pid" >/dev/null 2>&1 || true; done
}
trap cleanup EXIT

pass() { printf 'PASS  %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; FAILS=$((FAILS + 1)); }
# 故障注入期间目标本来就该不健康；这不是链路故障，单独记为 WARN 并在总结里列出。
warn() { printf 'WARN  %s\n' "$*"; WARNS=$((WARNS + 1)); }
info() { printf -- '---   %s\n' "$*"; }

need() {
  command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1" >&2; exit 2; }
}
need curl
need python3
need kubectl

# json FIELD 从 stdin 读 JSON，取点分路径；用于从 API 响应里摘值。
json() {
  python3 -c '
import json,sys
path=sys.argv[1].split(".") if sys.argv[1] else []
d=json.load(sys.stdin)
for p in path:
    if p=="": continue
    if isinstance(d,list): d=d[int(p)]
    else: d=d[p]
# 标量直接原样输出（Prometheus API 的 value 是字符串 "1"，json.dumps 会多出引号，
# 导致 [[ "$val" == "1" ]] 恒假）；容器/对象才走 JSON。
print(d if not isinstance(d,(dict,list)) else json.dumps(d,ensure_ascii=False))
' "$1"
}

port_forward() { # port_forward <svc> <local> <remote>
  local svc=$1 localp=$2 remote=$3
  kubectl -n "$NS" port-forward "svc/$svc" "${localp}:${remote}" >/tmp/pf-"$svc".log 2>&1 &
  PF_PIDS="$PF_PIDS $!"
  for _ in $(seq 1 40); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${localp}") 2>/dev/null; then exec 3>&-; return 0; fi
    sleep 0.25
  done
  fail "port-forward svc/${svc} ${localp}:${remote} 未就绪"
  return 1
}

prom_get() { # prom_get <query>
  curl -sS --max-time 10 -G "$PROM_URL/api/v1/query" --data-urlencode "query=$1"
}

loki_get() { # loki_get <logql> <start_ns> <end_ns>
  curl -sS --max-time 10 -G "$LOKI_URL/loki/api/v1/query_range" \
    --data-urlencode "query=$1" \
    --data-urlencode "start=$2" --data-urlencode "end=$3" \
    --data-urlencode "limit=100"
}

# ---- Streamable HTTP MCP 客户端（initialize → 记 session → tools/call） ----------
mcp_call() { # mcp_call <url> <tool> <arguments-json>
  python3 - "$1" "$2" "$3" <<'PY'
import json, sys, urllib.request

url, tool, args = sys.argv[1], sys.argv[2], json.loads(sys.argv[3])
headers = {"Content-Type": "application/json",
           "Accept": "application/json, text/event-stream"}

def post(payload, sid=None):
    h = dict(headers)
    if sid:
        h["Mcp-Session-Id"] = sid
    req = urllib.request.Request(url, data=json.dumps(payload).encode(), headers=h)
    resp = urllib.request.urlopen(req, timeout=15)
    sid_out = resp.headers.get("Mcp-Session-Id")
    raw = resp.read().decode()
    # notifications/initialized 回 202 且 body 为空；初始化/调用才需要解析。
    if not raw.strip():
        return None, sid_out
    # 服务端可能回 SSE 帧（event: message\ndata: {...}）或纯 JSON
    for line in raw.splitlines():
        if line.startswith("data: "):
            return json.loads(line[6:]), sid_out
    return json.loads(raw), sid_out

init, sid = post({"jsonrpc": "2.0", "id": 1, "method": "initialize",
                  "params": {"protocolVersion": "2024-11-05", "capabilities": {},
                             "clientInfo": {"name": "verify-p5", "version": "1"}}})
post({"jsonrpc": "2.0", "method": "notifications/initialized"}, sid)
listed, _ = post({"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}}, sid)
names = [t["name"] for t in listed.get("result", {}).get("tools", [])]
called, _ = post({"jsonrpc": "2.0", "id": 3, "method": "tools/call",
                  "params": {"name": tool, "arguments": args}}, sid)
print(json.dumps({"tools": names, "result": called.get("result"),
                  "error": called.get("error")}, ensure_ascii=False))
PY
}

# =================================================================================
info "1/5 前置检查"
if ! kubectl -n "$NS" get deploy,sts,ds >/dev/null 2>&1; then
  echo "observability namespace 不可用；先 apply deploy/k8s/overlays/observability" >&2
  exit 2
fi
kubectl -n alertflow get deploy alertflow-api >/dev/null 2>&1 \
  || { echo "alertflow-api 未部署：观测栈 probe 无目标" >&2; exit 2; }

if [[ -z "${PROM_URL:-}" ]]; then
  port_forward prometheus 19090 9090 || exit 2
  PROM_URL=http://127.0.0.1:19090
fi
if [[ -z "${LOKI_URL:-}" ]]; then
  port_forward loki 13100 3100 || exit 2
  LOKI_URL=http://127.0.0.1:13100
fi
if [[ -z "${PROM_MCP_URL:-}" ]]; then
  port_forward prometheus-mcp 19091 8080 || exit 2
  PROM_MCP_URL=http://127.0.0.1:19091/mcp
fi
if [[ -z "${LOKI_MCP_URL:-}" ]]; then
  port_forward loki-mcp 19092 8080 || exit 2
  LOKI_MCP_URL=http://127.0.0.1:19092/stream
fi
info "PROM_URL=$PROM_URL"
info "LOKI_URL=$LOKI_URL"
info "PROM_MCP_URL=$PROM_MCP_URL"
info "LOKI_MCP_URL=$LOKI_MCP_URL"

# =================================================================================
info "2/5 原生 Prometheus API：blackbox probe_*（service_id 由 relabel 生成）"
NOW=$(date +%s)
for sid in fault-lab alertflow; do
  # up 只说明 Prometheus 抓到了 blackbox，不说明被探测目标健康；probe_success 才说明。
  up=$(prom_get "up{service_id=\"$sid\"}")
  val=$(printf '%s' "$up" | json "data.result.0.value.1" 2>/dev/null || true)
  if [[ "$val" == "1" ]]; then pass "up{service_id=\"$sid\"} == 1（scrape 正常）"
  else fail "up{service_id=\"$sid\"} != 1（blackbox scrape 异常）: $up"; fi

  succ=$(prom_get "probe_success{service_id=\"$sid\"}")
  info "原生 API 原始响应 probe_success{service_id=\"$sid\"}: $succ"
  val=$(printf '%s' "$succ" | json "data.result.0.value.1" 2>/dev/null || true)
  if [[ "$val" == "1" ]]; then pass "probe_success{service_id=\"$sid\"} == 1（目标健康）"
  elif [[ "$val" == "0" ]]; then
    warn "probe_success{service_id=\"$sid\"} == 0：目标当前不健康（若为 fault-lab 故障注入则符合预期，不是链路故障）"
  else fail "probe_success{service_id=\"$sid\"} 缺失或异常: $succ"; fi

  dur=$(prom_get "probe_duration_seconds{service_id=\"$sid\"}")
  val=$(printf '%s' "$dur" | json "data.result.0.value.1" 2>/dev/null || true)
  if [[ -n "$val" && "$val" != "null" ]]; then pass "probe_duration_seconds{service_id=\"$sid\"} = $val"
  else fail "probe_duration_seconds{service_id=\"$sid\"} 缺失: $dur"; fi

  code=$(prom_get "probe_http_status_code{service_id=\"$sid\"}")
  val=$(printf '%s' "$code" | json "data.result.0.value.1" 2>/dev/null || true)
  if [[ "$val" == "200" ]]; then pass "probe_http_status_code{service_id=\"$sid\"} = 200"
  elif [[ -n "$val" && "$val" != "null" ]]; then
    warn "probe_http_status_code{service_id=\"$sid\"} = $val（故障注入期间非 200 符合预期）"
  else fail "probe_http_status_code{service_id=\"$sid\"} 缺失: $code"; fi
done

# service_id 必须是精确匹配，不能靠子串：fault-lab 的 series 不应同时出现 alertflow。
fl=$(prom_get 'count(probe_success{service_id="fault-lab"})')
al=$(prom_get 'count(probe_success{service_id="alertflow"})')
info "fault-lab series=$(printf '%s' "$fl" | json data.result.0.value.1) alertflow series=$(printf '%s' "$al" | json data.result.0.value.1)"

# =================================================================================
info "3/5 原生 LogQL：Alloy 是否把 service_id 带进 Loki"
START=$(( (NOW - 3600) * 1000000000 ))
END=$(( NOW * 1000000000 ))
for sid in fault-lab alertflow; do
  resp=$(loki_get "{service_id=\"$sid\"}" "$START" "$END")
  n=$(printf '%s' "$resp" | python3 -c '
import json,sys
d=json.load(sys.stdin)
res=d.get("data",{}).get("result",[])
print(sum(len(v.get("values",[])) for v in res))
' 2>/dev/null || echo ERR)
  if [[ "$n" =~ ^[0-9]+$ ]] && [[ "$n" -gt 0 ]]; then
    pass "LogQL {service_id=\"$sid\"} 近 1h 有 $n 行"
    if [[ "$sid" == "fault-lab" ]]; then
      NATIVE_LOKI_FLA=$resp
      NATIVE_FLA_LINE=$(printf '%s' "$resp" | python3 -c '
import json,sys
d=json.load(sys.stdin)
for v in d.get("data",{}).get("result",[]):
    for ts,line in v.get("values",[]):
        print(ts + " " + line); raise SystemExit
' 2>/dev/null || true)
      info "原生 LogQL 首行（fault-lab）: ${NATIVE_FLA_LINE:-<none>}"
    fi
  else
    fail "LogQL {service_id=\"$sid\"} 近 1h 无日志（Alloy 未采集或 service_id 未注入）: $resp"
  fi
done

# 标签值精确性：Loki 里 service_id 的取值集合应包含 fault-lab 与 alertflow。
labels=$(curl -sS --max-time 10 -G "$LOKI_URL/loki/api/v1/label/service_id/values")
info "Loki label service_id values: $labels"
for sid in fault-lab alertflow; do
  if printf '%s' "$labels" | grep -q "\"$sid\""; then pass "Loki 有 service_id=$sid"
  else fail "Loki 缺少 service_id=$sid"; fi
done

# =================================================================================
info "4/5 MCP 层：Prometheus MCP 查同一 PromQL"
PMCP=$(mcp_call "$PROM_MCP_URL" query "{\"query\":\"probe_success{service_id=\\\"fault-lab\\\"}\"}")
TNAMES=$(printf '%s' "$PMCP" | json tools 2>/dev/null || true)
for t in query range_query metric_metadata label_names label_values; do
  if printf '%s' "$TNAMES" | grep -q "\"$t\""; then pass "Prometheus MCP tools/list 含 $t"
  else fail "Prometheus MCP tools/list 缺 $t（--mcp.tools=core 配置有问题）"; fi
done
if printf '%s' "$TNAMES" | grep -q '"delete_series"'; then
  fail "Prometheus MCP 暴露了 delete_series（管理工具未收口）"
else pass "Prometheus MCP 未暴露 delete_series/clean_tombstones 等管理工具"; fi

native_succ=$(prom_get 'probe_success{service_id="fault-lab"}')
native_val=$(printf '%s' "$native_succ" | json "data.result.0.value.1")
# 故障注入会让 probe_success 在 0/1 间跳变，瞬时取样必然错位；取最近 10 分钟
# 的原生采样集合，MCP 的值只需落在集合里即证明"同一指标、同一源"。
native_recent=$(curl -sS --max-time 10 -G "$PROM_URL/api/v1/query_range" \
  --data-urlencode 'query=probe_success{service_id="fault-lab"}' \
  --data-urlencode "start=$(( NOW - 600 ))" --data-urlencode "end=$NOW" \
  --data-urlencode 'step=15s' | python3 -c '
import json,sys
try: d=json.load(sys.stdin)
except Exception: print(""); raise SystemExit
vals=set()
for r in d.get("data",{}).get("result",[]):
    for ts,v in r.get("values",[]): vals.add(str(v))
print(" ".join(sorted(vals)))
' 2>/dev/null || true)
info "原生 probe_success 最近 10 分钟采样集合: [$native_recent]"
mcp_val=$(printf '%s' "$PMCP" | python3 -c '
import json,sys,re
d=json.load(sys.stdin)
res=d.get("result") or {}
txt=""
for c in (res.get("content") or []):
    txt += c.get("text","")
# text 本身是 v0.18.0 的 JSON 字符串：{"result":"... => 1 @[<ts>]","warnings":null}，
# 且 query 输出里的 => 被编码成 \u003e；先解一层 JSON 再匹配。
try:
    inner = json.loads(txt)
    if isinstance(inner, dict) and isinstance(inner.get("result"), str):
        txt = inner["result"]
except Exception:
    pass
m = re.search(r"=>\s*([0-9.eE+-]+)\s*@\[", txt) or re.search(
    r"\"value\"\s*:\s*\[\s*[0-9.]+\s*,\s*\"([0-9.eE+-]+)\"", txt)
print(m.group(1) if m else "NONE")
' 2>/dev/null || echo ERR)
if [[ "$mcp_val" != "NONE" && (" $native_recent " == *" $mcp_val "* || "$mcp_val" == "$native_val") ]]; then
  pass "MCP probe_success=$mcp_val 与原生 API 一致（原生最近采样 [$native_recent]）"
else fail "MCP probe_success=$mcp_val 与原生 API=$native_val/[$native_recent] 不一致"; fi

# range_query 同窗口
PRANGE=$(mcp_call "$PROM_MCP_URL" range_query "{\"query\":\"probe_success{service_id=\\\"fault-lab\\\"}\",\"start_time\":\"$(( NOW - 1800 ))\",\"end_time\":\"$NOW\",\"step\":\"60s\"}")
if printf '%s' "$PRANGE" | grep -q '"result"'; then pass "MCP range_query 返回结果"
else fail "MCP range_query 失败: $PRANGE"; fi

# =================================================================================
info "5/5 MCP 层：Loki MCP 查同一 LogQL 窗口"
LMCP=$(mcp_call "$LOKI_MCP_URL" loki_query "{\"query\":\"{service_id=\\\"fault-lab\\\"}\",\"start\":\"$(date -u -d "@$((NOW-1800))" +%Y-%m-%dT%H:%M:%SZ)\",\"end\":\"$(date -u -d "@$NOW" +%Y-%m-%dT%H:%M:%SZ)\",\"limit\":100}")
LNAMES=$(printf '%s' "$LMCP" | json tools 2>/dev/null || true)
if printf '%s' "$LNAMES" | grep -q '"loki_query"'; then pass "Loki MCP tools/list 含 loki_query"
else fail "Loki MCP tools/list 缺 loki_query: $LMCP"; fi
if printf '%s' "$LMCP" | grep -q '"result"'; then pass "Loki MCP loki_query 返回结果"
else fail "Loki MCP loki_query 失败: $LMCP"; fi

# 三层一致的日志侧证据：MCP 返回的文本里必须出现原生 LogQL 查到的同一行内容。
LMCP_TEXT=$(printf '%s' "$LMCP" | python3 -c '
import json,sys
d=json.load(sys.stdin)
res=d.get("result") or {}
out=""
for c in (res.get("content") or []):
    out += c.get("text","")
print(out)
' 2>/dev/null || true)
if [[ -n "${NATIVE_FLA_LINE:-}" ]]; then
  LINE_BODY=${NATIVE_FLA_LINE#* }
  # MCP 返回的是 JSON 文本，日志里的引号会被转义；两种形态都比一次。
  LINE_BODY_JSON=$(python3 -c 'import json,sys; print(json.dumps(sys.argv[1])[1:-1])' "$LINE_BODY" 2>/dev/null || true)
  if printf '%s' "$LMCP_TEXT" | grep -qF -- "$LINE_BODY" \
     || { [[ -n "$LINE_BODY_JSON" ]] && printf '%s' "$LMCP_TEXT" | grep -qF -- "$LINE_BODY_JSON"; }; then
    pass "MCP 返回日志含原生 LogQL 同一行（内容一致）"
  else
    fail "MCP 返回日志不含原生 LogQL 首行内容：$LINE_BODY"
  fi
  if printf '%s' "$LMCP_TEXT" | grep -qF -- "service_id"; then
    pass "MCP 返回日志带 service_id 流标签"
  else
    info "MCP 返回文本未显式出现 service_id 字样（取决于 format），仅内容匹配已校验"
  fi
else
  info "原生 LogQL 无行，跳过 MCP 内容一致性比对"
fi

# =================================================================================
echo
if [[ "$FAILS" -eq 0 ]]; then
  if [[ "$WARNS" -eq 0 ]]; then
    echo "RESULT: PASS（原生 API、LogQL、MCP 三层一致）"
  else
    echo "RESULT: PASS（三层一致；${WARNS} 项 WARN 为故障注入期间的预期不健康状态，详见上方 WARN 行）"
  fi
  exit 0
fi
echo "RESULT: FAIL（${FAILS} 项，另 ${WARNS} 项 WARN）"
exit 1
