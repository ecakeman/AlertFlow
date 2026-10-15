package query

import "time"

// TargetPoints is roughly how many buckets a chart wants across any range —
// enough to show shape, few enough to stay legible and cheap to send.
const TargetPoints = 96

// bucketLadder holds the bucket widths a series may be downsampled to. Every
// range divides one of these exactly, so no bucket straddles the window edge.
var bucketLadder = []int64{30, 60, 300, 900, 1800, 3600, 7200, 28800}

// RangeSince maps a ?range= value to a lookback window (default 24h). "365d" is
// no longer offered as a tab but stays accepted — it is a documented API value.
func RangeSince(now time.Time, r string) time.Time {
	switch r {
	case "1h":
		return now.Add(-time.Hour)
	case "6h":
		return now.Add(-6 * time.Hour)
	case "7d":
		return now.Add(-7 * 24 * time.Hour)
	case "30d":
		return now.Add(-30 * 24 * time.Hour)
	case "365d":
		return now.Add(-365 * 24 * time.Hour)
	default:
		return now.Add(-24 * time.Hour)
	}
}

// ChooseBucket picks a bucket width for a span, in seconds. It must be at least
// span/TargetPoints (or the payload balloons) and at least the service's check
// interval — a bucket narrower than the interval leaves most buckets empty and
// renders as a comb. A 5-minute-interval service over 1h therefore gets 12 fat
// buckets rather than 98 mostly-empty ones.
//
// The caller passes the span because the bucket depends on the service's check
// interval, which this layer is told about rather than looks up.
func ChooseBucket(span int64, interval int) int64 {
	min := span / TargetPoints
	if int64(interval) > min {
		min = int64(interval)
	}
	for _, b := range bucketLadder {
		if b >= min {
			return b
		}
	}
	return bucketLadder[len(bucketLadder)-1]
}
