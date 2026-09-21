package constants

import "testing"

func TestPlanTransitions(t *testing.T) {
	tests := []struct {
		name string
		from PlanStatus
		to   PlanStatus
		want bool
	}{
		{"model draft", PlanDraft, PlanModeled, true},
		{"submit modeled", PlanModeled, PlanPendingReview, true},
		{"approve review", PlanPendingReview, PlanApprovedTraining, true},
		{"archive approval", PlanApprovedTraining, PlanArchived, true},
		{"cannot skip review", PlanModeled, PlanApprovedTraining, false},
		{"archive terminal", PlanArchived, PlanDraft, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CanTransitionPlan(test.from, test.to); got != test.want {
				t.Fatalf("transition %s -> %s = %t, want %t", test.from, test.to, got, test.want)
			}
		})
	}
}

func TestCanReopenPlan(t *testing.T) {
	tests := []struct {
		name   string
		status PlanStatus
		want   bool
	}{
		{"draft has no model to replace", PlanDraft, false},
		{"modeled carries reopen entry", PlanModeled, true},
		{"pending review carries reopen entry", PlanPendingReview, true},
		{"approved plan is locked", PlanApprovedTraining, false},
		{"archived plan is terminal", PlanArchived, false},
		{"superseded assessment is not a plan state", AssessmentSuperseded, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CanReopenPlan(test.status); got != test.want {
				t.Fatalf("CanReopenPlan(%s) = %t, want %t", test.status, got, test.want)
			}
		})
	}
}
