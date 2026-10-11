# intermittent-failure

## 适用
Warning rows, three or more transitions, or a warning event.

## 需要的证据
ana:status and warning check rows.

## 候选原因与区分信号
A retry recovered. The error is the first attempt, not the successful one.

## 调查顺序
Count warnings, then read one representative error.

## 解释限制
Warning counts as uptime.

## 停止条件与必须说明的未知项
Stop when the warning semantics are stated.
