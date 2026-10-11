# service-unavailable

## 适用
incident_start with an offline segment or a firing external alert.

## 需要的证据
ana:segments, ana:errors, external alerts, tls snapshot.

## 候选原因与区分信号
Probe timeout, HTTP status, or an external alert that keeps the incident open after HTTP recovery.

## 调查顺序
Read the status segments, then the error class, then external alerts.

## 解释限制
A 503 does not prove a database failure.

## 停止条件与必须说明的未知项
Stop when the probe evidence is cited. Say when diagnostics are missing.
