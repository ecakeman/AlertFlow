# latency-degradation

## 适用
median_ratio at least 1.5 with at least 20 online samples.

## 需要的证据
ana:latency recent and baseline.

## 候选原因与区分信号
Slower successful checks. Offline latency is excluded.

## 调查顺序
Compare recent median with the earlier buckets.

## 解释限制
Fewer than eight baseline buckets is NO_BASELINE.

## 停止条件与必须说明的未知项
Stop when the ratio is cited or the gap is named.
