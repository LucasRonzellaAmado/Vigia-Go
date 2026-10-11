package auth

type Plan string

const (
	PlanFree Plan = "free"
	PlanPro  Plan = "pro"
)

type Limits struct {
	Plan               Plan
	MaxMonitors        int
	MaxProjects        int
	MinIntervalSeconds int
	MaxChannels        int
	CustomDomain       bool
}

func LimitsFor(plan string) Limits {
	switch Plan(plan) {
	case PlanPro:
		return Limits{
			Plan:               PlanPro,
			MaxMonitors:        200,
			MaxProjects:        20,
			MinIntervalSeconds: 30,
			MaxChannels:        50,
			CustomDomain:       true,
		}
	default:
		return Limits{
			Plan:               PlanFree,
			MaxMonitors:        10,
			MaxProjects:        2,
			MinIntervalSeconds: 60,
			MaxChannels:        5,
			CustomDomain:       false,
		}
	}
}

func (p Plan) Label() string {
	if p == PlanPro {
		return "Pro"
	}
	return "Free"
}
