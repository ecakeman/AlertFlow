package event

// Transition is what a check event should do to incidents. It does not touch
// the database or send a notification.
type Transition string

const (
	TransitionNone    Transition = ""
	TransitionOpen    Transition = "open"
	TransitionResolve Transition = "resolve"
	TransitionWarning Transition = "warning"
)

// Decide maps a previous status and the event status onto one incident action.
// warning after offline is a recovery, not a second warning notification.
// An unchanged status does nothing, including the first online check when the
// previous status is already online.
func Decide(prev, current string) Transition {
	if prev == current {
		return TransitionNone
	}
	switch current {
	case "offline":
		return TransitionOpen
	case "online", "warning":
		if prev == "offline" {
			return TransitionResolve
		}
		if current == "warning" {
			return TransitionWarning
		}
		return TransitionNone
	default:
		return TransitionNone
	}
}
