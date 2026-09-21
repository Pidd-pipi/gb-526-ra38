package service

import (
	"context"
	"testing"
	"time"

	"commercial-diving-decompression-control/backend/internal/audit"
	"commercial-diving-decompression-control/backend/internal/auth"
	"commercial-diving-decompression-control/backend/internal/constants"
	"commercial-diving-decompression-control/backend/internal/decompression"
	"commercial-diving-decompression-control/backend/internal/dto"
	"commercial-diving-decompression-control/backend/internal/model"
	"commercial-diving-decompression-control/backend/internal/repository"
	"commercial-diving-decompression-control/backend/internal/util"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type reopenFixture struct {
	db         *gorm.DB
	service    *DecompressionAssessmentService
	plan       model.DivePlan
	profile    model.DiverProfile
	assessment model.DecompressionAssessment
	actor      audit.Entry
}

func newReopenFixture(t *testing.T, status constants.PlanStatus, version uint) reopenFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:reopen-service-test?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&auth.User{}, &model.DiverProfile{}, &model.DivePlan{}, &model.ExposureSegment{}, &model.DecompressionAssessment{}, &audit.Event{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		sqlDB, _ := db.DB()
		_ = sqlDB.Close()
	})
	air, err := decompression.EncodeGasMix(decompression.GasMix{O2: 0.21, N2: 0.79})
	if err != nil {
		t.Fatalf("encode gas: %v", err)
	}
	user := auth.User{Username: "planner-svc", PasswordHash: "x", DisplayName: "Planner", Role: auth.RolePlanner, Active: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	profile := model.DiverProfile{ProfileCode: "TRN-SVC", DisplayName: "Service Profile", QualificationLevel: "commercial", DefaultO2Fraction: 0.21, DefaultHeFraction: 0, ProfileStatus: "active", LimitsNote: "test", Version: 1}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}
	plan := model.DivePlan{PlanCode: "SVC-REOPEN", DiverProfileID: profile.ID, WorksitePressureBar: 1, BreathingMixJSON: air, PlanStatus: status, CreatedBy: user.ID, Version: version, PlannedAt: time.Now().UTC()}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	segments := []model.ExposureSegment{
		{PlanID: plan.ID, SequenceNo: 1, DepthM: 30, DurationMin: 3, GasMixJSON: air, SegmentType: "descent", Notes: "test"},
		{PlanID: plan.ID, SequenceNo: 2, DepthM: 30, DurationMin: 20, GasMixJSON: air, SegmentType: "bottom", Notes: "test"},
		{PlanID: plan.ID, SequenceNo: 3, DepthM: 0, DurationMin: 4, AscentRateMMin: 8, GasMixJSON: air, SegmentType: "ascent", Notes: "test"},
	}
	if err := db.Create(&segments).Error; err != nil {
		t.Fatalf("create segments: %v", err)
	}
	result, err := decompression.Run(plan, profile, segments, "training-compartment-v1", 12)
	if err != nil {
		t.Fatalf("run model: %v", err)
	}
	snapshot, curves, flags, assumptions, err := decompression.MarshalResult(result)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assessment := model.DecompressionAssessment{PlanID: plan.ID, AssessmentStatus: string(status), AlgorithmVersion: "training-compartment-v1", InputSnapshotJSON: snapshot, CompartmentLoadsJSON: curves, RiskFlagsJSON: flags, HighestRiskBand: decompression.HighestRiskBand(result.RiskFlags), ComparativeScore: result.ComparativeScore, AssumptionsJSON: assumptions}
	if err := db.Create(&assessment).Error; err != nil {
		t.Fatalf("create assessment: %v", err)
	}
	auditRepo := audit.NewRepository(db)
	svc := NewDecompressionAssessmentService(
		repository.NewDecompressionAssessmentRepository(db, auditRepo),
		repository.NewDivePlanRepository(db, auditRepo),
		repository.NewDiverProfileRepository(db, auditRepo),
		repository.NewExposureSegmentRepository(db, auditRepo),
		"training-compartment-v1", 12,
	)
	return reopenFixture{
		db: db, service: svc, plan: plan, profile: profile, assessment: assessment,
		actor: audit.Entry{RequestID: "req-svc", ActorID: user.ID, ActorUsername: "planner-svc"},
	}
}

func appErrorCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var appErr *util.AppError
	for err != nil {
		if candidate, ok := err.(*util.AppError); ok {
			appErr = candidate
			break
		}
		unwrap, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = unwrap.Unwrap()
	}
	if appErr == nil {
		t.Fatalf("error %v is not an AppError", err)
	}
	return appErr.Code
}

func TestServiceReopenApprovedPlanRejected(t *testing.T) {
	fixture := newReopenFixture(t, constants.PlanApprovedTraining, 5)
	_, err := fixture.service.Reopen(context.Background(), fixture.plan.ID, dto.ReopenPlanRequest{Version: 5, Reason: "should be denied"}, fixture.actor)
	if code := appErrorCode(t, err); code != "INVALID_PLAN_TRANSITION" {
		t.Fatalf("code = %s, want INVALID_PLAN_TRANSITION", code)
	}
	var plan model.DivePlan
	if err := fixture.db.First(&plan, fixture.plan.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if plan.PlanStatus != constants.PlanApprovedTraining {
		t.Fatalf("approved plan status changed to %s", plan.PlanStatus)
	}
}

func TestServiceReopenStaleVersionRejected(t *testing.T) {
	fixture := newReopenFixture(t, constants.PlanModeled, 4)
	_, err := fixture.service.Reopen(context.Background(), fixture.plan.ID, dto.ReopenPlanRequest{Version: 3, Reason: "stale body"}, fixture.actor)
	if code := appErrorCode(t, err); code != "PLAN_VERSION_CONFLICT" {
		t.Fatalf("code = %s, want PLAN_VERSION_CONFLICT", code)
	}
	var assessment model.DecompressionAssessment
	if err := fixture.db.First(&assessment, fixture.assessment.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if assessment.AssessmentStatus != string(constants.PlanModeled) {
		t.Fatalf("assessment changed to %s after stale reopen", assessment.AssessmentStatus)
	}
}

func TestServiceReopenThenRemodelInvalidatesOldAssessmentForever(t *testing.T) {
	ctx := context.Background()
	fixture := newReopenFixture(t, constants.PlanPendingReview, 3)

	reopened, err := fixture.service.Reopen(ctx, fixture.plan.ID, dto.ReopenPlanRequest{Version: 3, Reason: "bottom time needs revision"}, fixture.actor)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Plan.PlanStatus != constants.PlanDraft {
		t.Fatalf("plan status = %s, want draft", reopened.Plan.PlanStatus)
	}
	if reopened.Assessment.AssessmentStatus != string(constants.AssessmentSuperseded) {
		t.Fatalf("assessment status = %s, want superseded", reopened.Assessment.AssessmentStatus)
	}
	if reopened.Assessment.SupersedeReason != "bottom time needs revision" {
		t.Fatalf("supersede reason = %q", reopened.Assessment.SupersedeReason)
	}
	oldAssessmentID := reopened.Assessment.ID

	// The superseded snapshot remains readable with its original evidence.
	oldView, err := fixture.service.Get(ctx, oldAssessmentID)
	if err != nil {
		t.Fatalf("read superseded snapshot: %v", err)
	}
	if oldView.InputSnapshot.Plan.PlanCode != "SVC-REOPEN" || len(oldView.CompartmentLoads) == 0 {
		t.Fatal("superseded assessment snapshot is no longer readable")
	}

	// Submitting or approving the old result must always be rejected.
	_, err = fixture.service.Submit(ctx, oldAssessmentID, dto.TransitionPlanRequest{TargetStatus: constants.PlanPendingReview, Version: reopened.Plan.Version, Reason: "old result submit attempt"}, fixture.actor)
	if code := appErrorCode(t, err); code != "ASSESSMENT_SUPERSEDED" {
		t.Fatalf("old submit code = %s, want ASSESSMENT_SUPERSEDED", code)
	}
	_, err = fixture.service.Approve(ctx, oldAssessmentID, dto.TransitionPlanRequest{TargetStatus: constants.PlanApprovedTraining, Version: reopened.Plan.Version, Reason: "old result approve attempt"}, fixture.actor)
	if code := appErrorCode(t, err); code != "ASSESSMENT_SUPERSEDED" {
		t.Fatalf("old approve code = %s, want ASSESSMENT_SUPERSEDED", code)
	}

	// Re-model the draft: a new current assessment is created while the old
	// row stays superseded.
	newRun, err := fixture.service.Run(ctx, fixture.plan.ID, dto.RunAssessmentRequest{PlanVersion: reopened.Plan.Version}, fixture.actor)
	if err != nil {
		t.Fatalf("re-run model: %v", err)
	}
	if newRun.ID == oldAssessmentID {
		t.Fatal("re-model reused the superseded assessment row")
	}
	if newRun.AssessmentStatus != string(constants.PlanModeled) {
		t.Fatalf("new assessment status = %s, want modeled", newRun.AssessmentStatus)
	}

	// Even after a fresh model run exists, the old assessment can never move.
	_, err = fixture.service.Submit(ctx, oldAssessmentID, dto.TransitionPlanRequest{TargetStatus: constants.PlanPendingReview, Version: newRun.InputSnapshot.Plan.Version, Reason: "old result after remodel"}, fixture.actor)
	if code := appErrorCode(t, err); code != "ASSESSMENT_SUPERSEDED" {
		t.Fatalf("post-remodel old submit code = %s, want ASSESSMENT_SUPERSEDED", code)
	}

	// The new assessment follows the normal review flow using the current
	// plan version advanced by the model run.
	var currentPlan model.DivePlan
	if err := fixture.db.First(&currentPlan, fixture.plan.ID).Error; err != nil {
		t.Fatalf("reload plan after re-model: %v", err)
	}
	submitted, err := fixture.service.Submit(ctx, newRun.ID, dto.TransitionPlanRequest{TargetStatus: constants.PlanPendingReview, Version: currentPlan.Version, Reason: "new result for review"}, fixture.actor)
	if err != nil {
		t.Fatalf("new assessment submit: %v", err)
	}
	if submitted.AssessmentStatus != string(constants.PlanPendingReview) {
		t.Fatalf("new assessment status = %s, want pending review", submitted.AssessmentStatus)
	}

	var oldFinal model.DecompressionAssessment
	if err := fixture.db.First(&oldFinal, oldAssessmentID).Error; err != nil {
		t.Fatalf("reload old assessment: %v", err)
	}
	if oldFinal.AssessmentStatus != string(constants.AssessmentSuperseded) {
		t.Fatalf("old assessment status = %s, want superseded forever", oldFinal.AssessmentStatus)
	}
}
