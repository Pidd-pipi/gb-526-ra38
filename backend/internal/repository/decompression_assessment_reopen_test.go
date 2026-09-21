package repository

import (
	"context"
	"testing"
	"time"

	"commercial-diving-decompression-control/backend/internal/audit"
	"commercial-diving-decompression-control/backend/internal/auth"
	"commercial-diving-decompression-control/backend/internal/constants"
	"commercial-diving-decompression-control/backend/internal/decompression"
	"commercial-diving-decompression-control/backend/internal/model"
	"commercial-diving-decompression-control/backend/internal/util"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func reopenTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:reopen-test?mode=memory&cache=shared"), &gorm.Config{})
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
	return db
}

func seedReopenFixture(t *testing.T, db *gorm.DB, status constants.PlanStatus, version uint) (model.DivePlan, model.DecompressionAssessment) {
	t.Helper()
	air, err := decompression.EncodeGasMix(decompression.GasMix{O2: 0.21, N2: 0.79})
	if err != nil {
		t.Fatalf("encode gas: %v", err)
	}
	user := auth.User{Username: "planner-test", PasswordHash: "x", DisplayName: "Planner", Role: auth.RolePlanner, Active: true}
	if err := db.Create(&user).Error; err != nil {
		t.Fatalf("create user: %v", err)
	}
	profile := model.DiverProfile{ProfileCode: "TRN-REOPEN", DisplayName: "Reopen Profile", QualificationLevel: "commercial", DefaultO2Fraction: 0.21, DefaultHeFraction: 0, ProfileStatus: "active", LimitsNote: "test", Version: 1}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}
	plan := model.DivePlan{PlanCode: "REOPEN-1", DiverProfileID: profile.ID, WorksitePressureBar: 1, BreathingMixJSON: air, PlanStatus: status, CreatedBy: user.ID, Version: version, PlannedAt: time.Now().UTC()}
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
		t.Fatalf("marshal result: %v", err)
	}
	assessment := model.DecompressionAssessment{PlanID: plan.ID, AssessmentStatus: string(status), AlgorithmVersion: "training-compartment-v1", InputSnapshotJSON: snapshot, CompartmentLoadsJSON: curves, RiskFlagsJSON: flags, HighestRiskBand: decompression.HighestRiskBand(result.RiskFlags), ComparativeScore: result.ComparativeScore, AssumptionsJSON: assumptions}
	if err := db.Create(&assessment).Error; err != nil {
		t.Fatalf("create assessment: %v", err)
	}
	return plan, assessment
}

func reopenActor() audit.Entry {
	return audit.Entry{RequestID: "req-reopen-test", ActorID: 1, ActorUsername: "planner-test"}
}

func TestReopenSuccessMarksDraftAndSuperseded(t *testing.T) {
	db := reopenTestDB(t)
	repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
	plan, assessment := seedReopenFixture(t, db, constants.PlanModeled, 2)

	updatedPlan, updatedAssessment, err := repo.Reopen(context.Background(), plan, assessment, "bottom profile needs a longer segment", reopenActor())
	if err != nil {
		t.Fatalf("reopen returned error: %v", err)
	}
	if updatedPlan.PlanStatus != constants.PlanDraft {
		t.Fatalf("plan status = %s, want draft", updatedPlan.PlanStatus)
	}
	if updatedPlan.Version != plan.Version+1 {
		t.Fatalf("plan version = %d, want %d", updatedPlan.Version, plan.Version+1)
	}
	if updatedAssessment.AssessmentStatus != string(constants.AssessmentSuperseded) {
		t.Fatalf("assessment status = %s, want superseded", updatedAssessment.AssessmentStatus)
	}
	if updatedAssessment.SupersedeReason != "bottom profile needs a longer segment" {
		t.Fatalf("supersede reason = %q", updatedAssessment.SupersedeReason)
	}
	if updatedAssessment.InputSnapshotJSON == "" {
		t.Fatal("superseded assessment lost its input snapshot")
	}

	var events []audit.Event
	if err := db.Where("action IN ?", []string{"dive_plan.reopen", "decompression_assessment.supersede"}).Find(&events).Error; err != nil {
		t.Fatalf("load audit events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2", len(events))
	}
}

func TestReopenRejectsStaleVersionAndChangesNothing(t *testing.T) {
	db := reopenTestDB(t)
	repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
	plan, assessment := seedReopenFixture(t, db, constants.PlanModeled, 4)
	stale := plan
	stale.Version = 3

	_, _, err := repo.Reopen(context.Background(), stale, assessment, "stale client request", reopenActor())
	if err == nil {
		t.Fatal("expected conflict for stale version, got nil")
	}
	var appErr *util.AppError
	if !asAppError(err, &appErr) || appErr.Code != "PLAN_VERSION_CONFLICT" {
		t.Fatalf("error = %v, want PLAN_VERSION_CONFLICT", err)
	}
	var reloaded model.DivePlan
	if err := db.First(&reloaded, plan.ID).Error; err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	if reloaded.PlanStatus != constants.PlanModeled || reloaded.Version != 4 {
		t.Fatalf("plan changed after rejected reopen: status=%s version=%d", reloaded.PlanStatus, reloaded.Version)
	}
	var reloadedAssessment model.DecompressionAssessment
	if err := db.First(&reloadedAssessment, assessment.ID).Error; err != nil {
		t.Fatalf("reload assessment: %v", err)
	}
	if reloadedAssessment.AssessmentStatus != string(constants.PlanModeled) || reloadedAssessment.SupersedeReason != "" {
		t.Fatalf("assessment changed after rejected reopen: status=%s reason=%q", reloadedAssessment.AssessmentStatus, reloadedAssessment.SupersedeReason)
	}
}

func TestReopenRejectsApprovedPlanAndChangesNothing(t *testing.T) {
	db := reopenTestDB(t)
	repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
	plan, assessment := seedReopenFixture(t, db, constants.PlanApprovedTraining, 5)

	_, _, err := repo.Reopen(context.Background(), plan, assessment, "attempt to reopen approved plan", reopenActor())
	if err == nil {
		t.Fatal("expected conflict reopening approved plan, got nil")
	}
	var appErr *util.AppError
	if !asAppError(err, &appErr) || appErr.Code != "PLAN_VERSION_CONFLICT" {
		t.Fatalf("error = %v, want PLAN_VERSION_CONFLICT", err)
	}
	var reloaded model.DivePlan
	if err := db.First(&reloaded, plan.ID).Error; err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	if reloaded.PlanStatus != constants.PlanApprovedTraining {
		t.Fatalf("approved plan changed to %s", reloaded.PlanStatus)
	}
	var reloadedAssessment model.DecompressionAssessment
	if err := db.First(&reloadedAssessment, assessment.ID).Error; err != nil {
		t.Fatalf("reload assessment: %v", err)
	}
	if reloadedAssessment.AssessmentStatus != string(constants.PlanApprovedTraining) {
		t.Fatalf("approved assessment changed to %s", reloadedAssessment.AssessmentStatus)
	}
}

func TestConcurrentReopenOnlyOneWins(t *testing.T) {
	db := reopenTestDB(t)
	repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
	plan, assessment := seedReopenFixture(t, db, constants.PlanPendingReview, 3)

	type result struct {
		err error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	go func() {
		<-start
		_, _, err := repo.Reopen(context.Background(), plan, assessment, "first concurrent reason", reopenActor())
		results <- result{err}
	}()
	go func() {
		<-start
		_, _, err := repo.Reopen(context.Background(), plan, assessment, "second concurrent reason", reopenActor())
		results <- result{err}
	}()
	close(start)

	first := <-results
	second := <-results
	if (first.err == nil) == (second.err == nil) {
		t.Fatalf("expected exactly one successful reopen, got err1=%v err2=%v", first.err, second.err)
	}

	var reloadedPlan model.DivePlan
	if err := db.First(&reloadedPlan, plan.ID).Error; err != nil {
		t.Fatalf("reload plan: %v", err)
	}
	if reloadedPlan.PlanStatus != constants.PlanDraft {
		t.Fatalf("plan status = %s, want draft", reloadedPlan.PlanStatus)
	}
	var reloaded model.DecompressionAssessment
	if err := db.First(&reloaded, assessment.ID).Error; err != nil {
		t.Fatalf("reload assessment: %v", err)
	}
	if reloaded.AssessmentStatus != string(constants.AssessmentSuperseded) {
		t.Fatalf("assessment status = %s, want superseded", reloaded.AssessmentStatus)
	}
	if reloaded.SupersedeReason != "first concurrent reason" && reloaded.SupersedeReason != "second concurrent reason" {
		t.Fatalf("unexpected supersede reason %q", reloaded.SupersedeReason)
	}
}

func TestTransitionRejectsSupersededAssessment(t *testing.T) {
	db := reopenTestDB(t)
	repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
	plan, assessment := seedReopenFixture(t, db, constants.PlanModeled, 2)
	reopenedPlan, superseded, err := repo.Reopen(context.Background(), plan, assessment, "replace before resubmit", reopenActor())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	err = repo.Transition(context.Background(), reopenedPlan, superseded, constants.PlanPendingReview, 1, audit.Entry{RequestID: "req-transition-test", ActorID: 1, ActorUsername: "planner-test"})
	if err == nil {
		t.Fatal("expected transition to fail for superseded assessment")
	}
	var appErr *util.AppError
	if !asAppError(err, &appErr) {
		t.Fatalf("error = %v, want AppError", err)
	}
	if appErr.Code != "ASSESSMENT_STATE_CONFLICT" && appErr.Code != "PLAN_VERSION_CONFLICT" {
		t.Fatalf("error code = %s, want a state conflict", appErr.Code)
	}
}

func asAppError(err error, target **util.AppError) bool {
	for err != nil {
		if appErr, ok := err.(*util.AppError); ok {
			*target = appErr
			return true
		}
		type wrapper interface{ Unwrap() error }
		unwrap, ok := err.(wrapper)
		if !ok {
			return false
		}
		err = unwrap.Unwrap()
	}
	return false
}
