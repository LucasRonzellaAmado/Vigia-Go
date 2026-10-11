package alert

import "time"

const maxReminderGap = 24 * time.Hour

func ShouldRemind(lastAlert *time.Time, sent int, now time.Time, base time.Duration) bool {
	if base <= 0 {
		return false
	}
	if lastAlert == nil || sent <= 0 {
		return true
	}
	return now.Sub(*lastAlert) >= ReminderGap(sent, base)
}

func ReminderGap(sent int, base time.Duration) time.Duration {
	gap := base
	for i := 1; i < sent; i++ {
		gap *= 2
		if gap >= maxReminderGap {
			return maxReminderGap
		}
	}
	return min(gap, maxReminderGap)
}
