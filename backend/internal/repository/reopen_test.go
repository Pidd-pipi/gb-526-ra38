package repository

import (
	"context"
	"errors"
	"testing"
	"time"

	"commercial-diving-decompression-control/backend/internal/audit"
	"commercial-diving-decompression-control/backend/internal/constants"
	"commercial-diving-decompression-control/backend/internal/decompression"
	"commercial-diving-decompression-control/backend/internal/model"
	"commercial-diving-decompression-control/backend/internal/util"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func reopenTestDB(t *testing.T) *gorm.DB {
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
	return db
}

func reopenActor() audit.Entry {
	return audit.Entry{RequestID: "test-request", ActorID: 7, ActorUsername: "planner"}
}

func reopenFixture(t *testing.T, db *gorm.DB, status constants.PlanStatus, version uint) (model.DivePlan, model.DecompressionAssessment) {
	t.Helper()
	air, err := decompression.EncodeGasMix(decompression.GasMix{O2: 0.21, N2: 0.79})
	if err != nil {
		t.Fatalf("encode gas: %v", err)
	}
	profile := model.DiverProfile{ProfileCode: "TRN-REOPEN", DisplayName: "Reopen Profile", QualificationLevel: "commercial", DefaultO2Fraction: 0.21, ProfileStatus: "active", Version: 1}
	if err := db.Create(&profile).Error; err != nil {
		t.Fatalf("create profile: %v", err)
	}
	plan := model.DivePlan{PlanCode: "REOPEN-1", DiverProfileID: profile.ID, WorksitePressureBar: 1, BreathingMixJSON: air, PlanStatus: status, CreatedBy: 7, Version: version, PlannedAt: time.Now().UTC()}
	if err := db.Create(&plan).Error; err != nil {
		t.Fatalf("create plan: %v", err)
	}
	segment := model.ExposureSegment{PlanID: plan.ID, SequenceNo: 1, DepthM: 30, DurationMin: 20, GasMixJSON: air, SegmentType: "bottom"}
	if err := db.Create(&segment).Error; err != nil {
		t.Fatalf("create segment: %v", err)
	}
	assessment := model.DecompressionAssessment{PlanID: plan.ID, AssessmentStatus: string(status), AlgorithmVersion: "training-compartment-v1", InputSnapshotJSON: `{"plan_id":1}`, CompartmentLoadsJSON: `[]`, RiskFlagsJSON: `[]`, HighestRiskBand: "informational", ComparativeScore: 12.5, AssumptionsJSON: `{}`}
	if err := db.Create(&assessment).Error; err != nil {
		t.Fatalf("create assessment: %v", err)
	}
	return plan, assessment
}

func reopenEntries() (audit.Entry, audit.Entry) {
	planEntry := reopenActor()
	planEntry.Action = "dive_plan.reopen"
	planEntry.EntityType = "dive_plan"
	assessmentEntry := reopenActor()
	assessmentEntry.Action = "decompression_assessment.superseded"
	assessmentEntry.EntityType = "decompression_assessment"
	return planEntry, assessmentEntry
}

func TestReopenAndSupersede(t *testing.T) {
	tests := []struct {
		name    string
		status  constants.PlanStatus
		version uint
		wantErr string
	}{
		{name: "modeled plan reopens", status: constants.PlanModeled, version: 2, wantErr: ""},
		{name: "pending review plan reopens", status: constants.PlanPendingReview, version: 3, wantErr: ""},
		{name: "approved plan rejected", status: constants.PlanApprovedTraining, version: 4, wantErr: "PLAN_VERSION_CONFLICT"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := reopenTestDB(t)
			repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
			plan, assessment := reopenFixture(t, db, test.status, test.version)
			planEntry, assessmentEntry := reopenEntries()
			err := repo.ReopenAndSupersede(context.Background(), plan, assessment, "input assumption must change", planEntry, assessmentEntry)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %s, got nil", test.wantErr)
				}
				var appErr *util.AppError
				if !errors.As(err, &appErr) || appErr.Code != test.wantErr {
					t.Fatalf("expected %s, got %v", test.wantErr, err)
				}
				var unchanged model.DivePlan
				if err := db.First(&unchanged, plan.ID).Error; err != nil {
					t.Fatalf("reload plan: %v", err)
				}
				if unchanged.PlanStatus != test.status || unchanged.Version != test.version {
					t.Fatalf("plan changed on rejected reopen: status=%s version=%d", unchanged.PlanStatus, unchanged.Version)
				}
				var old model.DecompressionAssessment
				if err := db.First(&old, assessment.ID).Error; err != nil {
					t.Fatalf("reload assessment: %v", err)
				}
				if old.AssessmentStatus != string(test.status) || old.SupersededAt != nil {
					t.Fatalf("assessment changed on rejected reopen: status=%s superseded_at=%v", old.AssessmentStatus, old.SupersededAt)
				}
				return
			}
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			var reloaded model.DivePlan
			if err := db.First(&reloaded, plan.ID).Error; err != nil {
				t.Fatalf("reload plan: %v", err)
			}
			if reloaded.PlanStatus != constants.PlanDraft {
				t.Fatalf("plan status = %s, want draft", reloaded.PlanStatus)
			}
			if reloaded.Version != test.version+1 {
				t.Fatalf("plan version = %d, want %d", reloaded.Version, test.version+1)
			}
			if reloaded.LastReopenReason != "input assumption must change" {
				t.Fatalf("reopen reason = %q", reloaded.LastReopenReason)
			}
			var replaced model.DecompressionAssessment
			if err := db.First(&replaced, assessment.ID).Error; err != nil {
				t.Fatalf("reload assessment: %v", err)
			}
			if replaced.AssessmentStatus != constants.AssessmentSuperseded {
				t.Fatalf("assessment status = %s, want superseded", replaced.AssessmentStatus)
			}
			if replaced.SupersededAt == nil || replaced.SupersededReason != "input assumption must change" {
				t.Fatalf("superseded metadata missing: at=%v reason=%q", replaced.SupersededAt, replaced.SupersededReason)
			}
			// The immutable snapshot columns remain readable after supersession.
			if replaced.InputSnapshotJSON == "" || replaced.CompartmentLoadsJSON == "" {
				t.Fatalf("snapshot columns must remain readable, got %+v", replaced)
			}
			var events int64
			if err := db.Model(&audit.Event{}).Where("action IN ?", []string{"dive_plan.reopen", "decompression_assessment.superseded"}).Count(&events).Error; err != nil {
				t.Fatalf("count audit events: %v", err)
			}
			if events != 2 {
				t.Fatalf("audit events = %d, want 2", events)
			}
		})
	}
}

func TestReopenAndSupersedeStaleAndConcurrent(t *testing.T) {
	t.Run("stale version rejected atomically", func(t *testing.T) {
		db := reopenTestDB(t)
		repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
		plan, assessment := reopenFixture(t, db, constants.PlanModeled, 2)
		stale := plan
		stale.Version = 1
		planEntry, assessmentEntry := reopenEntries()
		err := repo.ReopenAndSupersede(context.Background(), stale, assessment, "stale client view", planEntry, assessmentEntry)
		var appErr *util.AppError
		if !errors.As(err, &appErr) || appErr.Code != "PLAN_VERSION_CONFLICT" {
			t.Fatalf("expected PLAN_VERSION_CONFLICT, got %v", err)
		}
		var unchanged model.DivePlan
		if err := db.First(&unchanged, plan.ID).Error; err != nil {
			t.Fatalf("reload plan: %v", err)
		}
		if unchanged.PlanStatus != constants.PlanModeled || unchanged.Version != 2 {
			t.Fatalf("plan changed on stale reopen: status=%s version=%d", unchanged.PlanStatus, unchanged.Version)
		}
		var old model.DecompressionAssessment
		if err := db.First(&old, assessment.ID).Error; err != nil {
			t.Fatalf("reload assessment: %v", err)
		}
		if old.AssessmentStatus != string(constants.PlanModeled) {
			t.Fatalf("assessment changed on stale reopen: %s", old.AssessmentStatus)
		}
	})
	t.Run("concurrent reopen rejected atomically", func(t *testing.T) {
		db := reopenTestDB(t)
		repo := NewDecompressionAssessmentRepository(db, audit.NewRepository(db))
		plan, assessment := reopenFixture(t, db, constants.PlanPendingReview, 3)
		planEntry, assessmentEntry := reopenEntries()
		if err := repo.ReopenAndSupersede(context.Background(), plan, assessment, "first reopen", planEntry, assessmentEntry); err != nil {
			t.Fatalf("first reopen: %v", err)
		}
		// The second caller still holds the pre-reopen plan/assessment view.
		secondPlan, secondAssessment := reopenEntries()
		err := repo.ReopenAndSupersede(context.Background(), plan, assessment, "second concurrent reopen", secondPlan, secondAssessment)
		var appErr *util.AppError
		if !errors.As(err, &appErr) {
			t.Fatalf("expected conflict error, got %v", err)
		}
		if appErr.Code != "PLAN_VERSION_CONFLICT" && appErr.Code != "ASSESSMENT_STATE_CONFLICT" {
			t.Fatalf("unexpected conflict code %s", appErr.Code)
		}
		var events int64
		if err := db.Model(&audit.Event{}).Where("action = ?", "decompression_assessment.superseded").Count(&events).Error; err != nil {
			t.Fatalf("count audit events: %v", err)
		}
		if events != 1 {
			t.Fatalf("superseded audit events = %d, want 1", events)
		}
	})
}
