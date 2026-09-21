package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"commercial-diving-decompression-control/backend/internal/audit"
	"commercial-diving-decompression-control/backend/internal/constants"
	"commercial-diving-decompression-control/backend/internal/decompression"
	"commercial-diving-decompression-control/backend/internal/dto"
	"commercial-diving-decompression-control/backend/internal/model"
	"commercial-diving-decompression-control/backend/internal/repository"
	"commercial-diving-decompression-control/backend/internal/util"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type reopenHarness struct {
	db      *gorm.DB
	plans   *repository.DivePlanRepository
	segs    *repository.ExposureSegmentRepository
	assess  *repository.DecompressionAssessmentRepository
	planSvc *DivePlanService
	assSvc  *DecompressionAssessmentService
	profile model.DiverProfile
	plan    model.DivePlan
}

func newReopenHarness(t *testing.T) *reopenHarness {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.DiverProfile{}, &model.DivePlan{}, &model.ExposureSegment{}, &model.DecompressionAssessment{}, &audit.Event{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Migrator().DropTable("audit_events", "decompression_assessments", "exposure_segments", "dive_plans", "diver_profiles"); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})
	auditRepo := audit.NewRepository(db)
	planRepo := repository.NewDivePlanRepository(db, auditRepo)
	profileRepo := repository.NewDiverProfileRepository(db, auditRepo)
	segmentRepo := repository.NewExposureSegmentRepository(db, auditRepo)
	assessmentRepo := repository.NewDecompressionAssessmentRepository(db, auditRepo)
	h := &reopenHarness{
		db:      db,
		plans:   planRepo,
		segs:    segmentRepo,
		assess:  assessmentRepo,
		planSvc: NewDivePlanService(planRepo, profileRepo, assessmentRepo),
		assSvc:  NewDecompressionAssessmentService(assessmentRepo, planRepo, profileRepo, segmentRepo, "training-compartment-v1", 24),
	}
	h.profile = model.DiverProfile{ProfileCode: "TRN-SVC", DisplayName: "Service Reopen", QualificationLevel: "commercial", DefaultO2Fraction: 0.21, DefaultHeFraction: 0, ProfileStatus: "active", Version: 1}
	if err := db.Create(&h.profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}
	air, err := decompression.EncodeGasMix(decompression.GasMix{O2: 0.21, N2: 0.79})
	if err != nil {
		t.Fatalf("encode gas: %v", err)
	}
	h.plan = model.DivePlan{PlanCode: "SVC-REOPEN-1", DiverProfileID: h.profile.ID, WorksitePressureBar: 1, BreathingMixJSON: air, PlanStatus: constants.PlanDraft, CreatedBy: 7, Version: 1, PlannedAt: time.Now().UTC().Add(24 * time.Hour)}
	createEntry := audit.Entry{RequestID: "req-seed", ActorID: 7, ActorUsername: "planner", Action: "dive_plan.create", EntityType: "dive_plan", AfterSummary: "draft"}
	if err := planRepo.Create(context.Background(), &h.plan, createEntry); err != nil {
		t.Fatalf("create plan: %v", err)
	}
	segments := []model.ExposureSegment{
		{PlanID: h.plan.ID, SequenceNo: 1, DepthM: 30, DurationMin: 3, GasMixJSON: air, SegmentType: "descent", Notes: "descent"},
		{PlanID: h.plan.ID, SequenceNo: 2, DepthM: 30, DurationMin: 20, GasMixJSON: air, SegmentType: "bottom", Notes: "bottom"},
		{PlanID: h.plan.ID, SequenceNo: 3, DepthM: 0, DurationMin: 4, AscentRateMMin: 9, GasMixJSON: air, SegmentType: "ascent", Notes: "ascent"},
	}
	for index := range segments {
		entry := audit.Entry{RequestID: "req-seed", ActorID: 7, ActorUsername: "planner", Action: "exposure_segment.create", EntityType: "exposure_segment"}
		if err := segmentRepo.Create(context.Background(), &segments[index], h.plan.Version, entry); err != nil {
			t.Fatalf("create segment %d: %v", index, err)
		}
		h.plan.Version++
	}
	return h
}

func reopenActorEntry() audit.Entry {
	return audit.Entry{RequestID: "req-reopen", ActorID: 7, ActorUsername: "planner"}
}

func TestReopenRemodelSupersedeLoop(t *testing.T) {
	h := newReopenHarness(t)
	ctx := context.Background()

	first, err := h.assSvc.Run(ctx, h.plan.ID, dto.RunAssessmentRequest{PlanVersion: h.plan.Version}, reopenActorEntry())
	if err != nil {
		t.Fatalf("first model run: %v", err)
	}
	if first.AssessmentStatus != string(constants.PlanModeled) {
		t.Fatalf("first assessment status = %s", first.AssessmentStatus)
	}
	modeledPlan, err := h.plans.Get(ctx, h.plan.ID)
	if err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	if modeledPlan.PlanStatus != constants.PlanModeled {
		t.Fatalf("plan status after run = %s", modeledPlan.PlanStatus)
	}

	// Reopen the modeled plan with the current version and a reason.
	reopened, err := h.planSvc.Reopen(ctx, h.plan.ID, dto.ReopenPlanRequest{Version: modeledPlan.Version, Reason: "bottom time must change for training comparison"}, reopenActorEntry())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.PlanStatus != constants.PlanDraft {
		t.Fatalf("plan status after reopen = %s", reopened.PlanStatus)
	}
	if reopened.LastReopenReason != "bottom time must change for training comparison" {
		t.Fatalf("reopen reason = %q", reopened.LastReopenReason)
	}

	// The original snapshot remains readable with its superseded marker.
	oldRead, err := h.assSvc.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("read superseded assessment: %v", err)
	}
	if oldRead.AssessmentStatus != constants.AssessmentSuperseded || oldRead.SupersededReason == "" || oldRead.SupersededAt == nil {
		t.Fatalf("old assessment not marked superseded: %+v", oldRead)
	}
	if len(oldRead.CompartmentLoads) == 0 || oldRead.InputSnapshot.Plan.ID != h.plan.ID {
		t.Fatalf("superseded snapshot must remain replayable")
	}

	// The old assessment can no longer be submitted.
	_, err = h.assSvc.Submit(ctx, first.ID, dto.TransitionPlanRequest{TargetStatus: constants.PlanPendingReview, Version: reopened.Version, Reason: "stale submit must fail"}, reopenActorEntry())
	var appErr *util.AppError
	if !errors.As(err, &appErr) || appErr.Code != "ASSESSMENT_SUPERSEDED" {
		t.Fatalf("submit superseded assessment: expected ASSESSMENT_SUPERSEDED, got %v", err)
	}
	// It cannot be approved either.
	_, err = h.assSvc.Approve(ctx, first.ID, dto.TransitionPlanRequest{TargetStatus: constants.PlanApprovedTraining, Version: reopened.Version, Reason: "stale approve must fail"}, reopenActorEntry())
	if !errors.As(err, &appErr) || appErr.Code != "ASSESSMENT_SUPERSEDED" {
		t.Fatalf("approve superseded assessment: expected ASSESSMENT_SUPERSEDED, got %v", err)
	}

	// Re-modeling from draft succeeds and produces a fresh current assessment.
	second, err := h.assSvc.Run(ctx, h.plan.ID, dto.RunAssessmentRequest{PlanVersion: reopened.Version}, reopenActorEntry())
	if err != nil {
		t.Fatalf("second model run: %v", err)
	}
	if second.ID == first.ID || second.AssessmentStatus != string(constants.PlanModeled) {
		t.Fatalf("new assessment not created correctly: %+v", second)
	}
	// The old assessment stays superseded after a successful re-model.
	oldAgain, err := h.assSvc.Get(ctx, first.ID)
	if err != nil {
		t.Fatalf("reread old assessment: %v", err)
	}
	if oldAgain.AssessmentStatus != constants.AssessmentSuperseded {
		t.Fatalf("old assessment status = %s, stays superseded", oldAgain.AssessmentStatus)
	}
	// The new assessment follows the normal review path.
	newPlan, err := h.plans.Get(ctx, h.plan.ID)
	if err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	submitted, err := h.assSvc.Submit(ctx, second.ID, dto.TransitionPlanRequest{TargetStatus: constants.PlanPendingReview, Version: newPlan.Version, Reason: "ready for supervisor review"}, reopenActorEntry())
	if err != nil {
		t.Fatalf("submit new assessment: %v", err)
	}
	if submitted.AssessmentStatus != string(constants.PlanPendingReview) {
		t.Fatalf("new assessment status = %s", submitted.AssessmentStatus)
	}
}

func TestReopenGuards(t *testing.T) {
	tests := []struct {
		name     string
		build    func(h *reopenHarness) (uint, dto.ReopenPlanRequest)
		wantCode string
	}{
		{
			name: "draft plan cannot reopen",
			build: func(h *reopenHarness) (uint, dto.ReopenPlanRequest) {
				return h.plan.ID, dto.ReopenPlanRequest{Version: h.plan.Version, Reason: "draft reopen must be rejected"}
			},
			wantCode: "PLAN_REOPEN_NOT_ALLOWED",
		},
		{
			name: "stale version rejected",
			build: func(h *reopenHarness) (uint, dto.ReopenPlanRequest) {
				first, err := h.assSvc.Run(context.Background(), h.plan.ID, dto.RunAssessmentRequest{PlanVersion: h.plan.Version}, reopenActorEntry())
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				return first.PlanID, dto.ReopenPlanRequest{Version: 1, Reason: "client held an old input version"}
			},
			wantCode: "PLAN_VERSION_CONFLICT",
		},
		{
			name: "approved plan rejected",
			build: func(h *reopenHarness) (uint, dto.ReopenPlanRequest) {
				first, err := h.assSvc.Run(context.Background(), h.plan.ID, dto.RunAssessmentRequest{PlanVersion: h.plan.Version}, reopenActorEntry())
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				plan, err := h.plans.Get(context.Background(), h.plan.ID)
				if err != nil {
					t.Fatalf("reload: %v", err)
				}
				supervisor := audit.Entry{RequestID: "req-approve", ActorID: 9, ActorUsername: "supervisor"}
				if _, err := h.assSvc.Submit(context.Background(), first.ID, dto.TransitionPlanRequest{TargetStatus: constants.PlanPendingReview, Version: plan.Version, Reason: "submit"}, reopenActorEntry()); err != nil {
					t.Fatalf("submit: %v", err)
				}
				plan, _ = h.plans.Get(context.Background(), h.plan.ID)
				if _, err := h.assSvc.Approve(context.Background(), first.ID, dto.TransitionPlanRequest{TargetStatus: constants.PlanApprovedTraining, Version: plan.Version, Reason: "human approval for training"}, supervisor); err != nil {
					t.Fatalf("approve: %v", err)
				}
				plan, _ = h.plans.Get(context.Background(), h.plan.ID)
				return plan.ID, dto.ReopenPlanRequest{Version: plan.Version, Reason: "approval is terminal for the reopen loop"}
			},
			wantCode: "PLAN_REOPEN_NOT_ALLOWED",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newReopenHarness(t)
			id, req := test.build(h)
			beforePlan, _ := h.plans.Get(context.Background(), id)
			_, err := h.planSvc.Reopen(context.Background(), id, req, reopenActorEntry())
			var appErr *util.AppError
			if !errors.As(err, &appErr) || appErr.Code != test.wantCode {
				t.Fatalf("expected %s, got %v", test.wantCode, err)
			}
			afterPlan, _ := h.plans.Get(context.Background(), id)
			if afterPlan.PlanStatus != beforePlan.PlanStatus || afterPlan.Version != beforePlan.Version {
				t.Fatalf("plan changed on rejected reopen: %s v%d -> %s v%d", beforePlan.PlanStatus, beforePlan.Version, afterPlan.PlanStatus, afterPlan.Version)
			}
		})
	}
}
