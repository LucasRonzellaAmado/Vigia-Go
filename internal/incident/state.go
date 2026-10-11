package incident

import (
	"time"

	"github.com/luquinha/vigia/internal/store"
)

type Status string

const (
	StatusUnknown Status = "unknown"
	StatusUp      Status = "up"
	StatusDown    Status = "down"
)

type State struct {
	Status              Status
	ConsecutiveFailures int
	FailingSince        time.Time
}

type Transition int

const (
	NoChange Transition = iota
	WentDown
	Recovered
)

func (t Transition) String() string {
	switch t {
	case WentDown:
		return "went_down"
	case Recovered:
		return "recovered"
	}
	return "no_change"
}

func Evaluate(s State, ok bool, at time.Time, threshold int) (State, Transition) {
	threshold = max(threshold, 1)

	if ok {
		next := State{Status: StatusUp}
		if s.Status == StatusDown {
			return next, Recovered
		}
		return next, NoChange
	}

	next := s
	next.ConsecutiveFailures++
	if next.FailingSince.IsZero() {
		next.FailingSince = at
	}
	if s.Status == StatusDown {
		return next, NoChange
	}
	if next.ConsecutiveFailures >= threshold {
		next.Status = StatusDown
		return next, WentDown
	}
	return next, NoChange
}

func StateOf(m store.Monitor) State {
	s := State{
		Status:              Status(m.Status),
		ConsecutiveFailures: int(m.ConsecutiveFailures),
	}
	if m.FailingSince != nil {
		s.FailingSince = *m.FailingSince
	}
	return s
}
