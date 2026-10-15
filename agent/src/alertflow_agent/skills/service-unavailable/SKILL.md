# service-unavailable

## 适用
incident_start with an offline segment.

## 需要的证据
ana:segments, ana:errors, tls snapshot.

## 候选原因与区分信号
Probe timeout, HTTP status, or a latency cliff in the metric series.

## 调查顺序
Read the status segments, then the error class, then the TLS snapshot.

## 解释限制
A 503 does not prove a database failure.

## 停止条件与必须说明的未知项
Stop when the probe evidence is cited. Say when diagnostics are missing.
