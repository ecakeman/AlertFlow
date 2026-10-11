# tls-evidence

## 适用
HTTPS with a tls error, daysLeft at most 14, or tls error class.

## 需要的证据
alertflow_tls or ana:errors classes.tls.

## 候选原因与区分信号
Certificate problem versus an unrelated HTTP failure.

## 调查顺序
Read the single TLS snapshot, then the error class.

## 解释限制
The snapshot has no history.

## 停止条件与必须说明的未知项
Stop when the snapshot limitation is stated.
