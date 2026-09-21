package repository

import (
	"context"
	"fmt"
	"time"

	"commercial-diving-decompression-control/backend/internal/audit"
	"commercial-diving-decompression-control/backend/internal/constants"
	"commercial-diving-decompression-control/backend/internal/model"
	"commercial-diving-decompression-control/backend/internal/util"
	"gorm.io/gorm"
)

type DecompressionAssessmentRepository struct {
	db    *gorm.DB
	audit *audit.Repository
}

func NewDecompressionAssessmentRepository(db *gorm.DB, auditRepo *audit.Repository) *DecompressionAssessmentRepository {
	return &DecompressionAssessmentRepository{db: db, audit: auditRepo}
}

func (r *DecompressionAssessmentRepository) List(ctx context.Context, planID uint, status string, page, size int) ([]model.DecompressionAssessment, int64, error) {
	query := r.db.WithContext(ctx).Model(&model.DecompressionAssessment{})
	if planID > 0 {
		query = query.Where("plan_id = ?", planID)
	}
	if status != "" {
		query = query.Where("assessment_status = ?", status)
	}
	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, fmt.Errorf("count assessments: %w", err)
	}
	var items []model.DecompressionAssessment
	if err := query.Order("created_at DESC, id DESC").Offset((page - 1) * size).Limit(size).Find(&items).Error; err != nil {
		return nil, 0, fmt.Errorf("list assessments: %w", err)
	}
	return items, total, nil
}

func (r *DecompressionAssessmentRepository) Get(ctx context.Context, id uint) (model.DecompressionAssessment, error) {
	var item model.DecompressionAssessment
	if err := r.db.WithContext(ctx).First(&item, id).Error; err != nil {
		return model.DecompressionAssessment{}, fmt.Errorf("get assessment %d: %w", id, err)
	}
	return item, nil
}

func (r *DecompressionAssessmentRepository) LatestByPlan(ctx context.Context, planID uint) (model.DecompressionAssessment, error) {
	var item model.DecompressionAssessment
	if err := r.db.WithContext(ctx).Where("plan_id = ?", planID).Order("created_at DESC, id DESC").First(&item).Error; err != nil {
		return model.DecompressionAssessment{}, fmt.Errorf("get latest assessment for plan %d: %w", planID, err)
	}
	return item, nil
}

func (r *DecompressionAssessmentRepository) CreateModeled(ctx context.Context, plan model.DivePlan, item *model.DecompressionAssessment, entry audit.Entry) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&model.DivePlan{}).Where("id = ? AND version = ? AND plan_status = ?", plan.ID, plan.Version, constants.PlanDraft).Updates(map[string]any{"plan_status": constants.PlanModeled, "version": gorm.Expr("version + 1")})
		if result.Error != nil {
			return fmt.Errorf("mark plan modeled: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return util.Conflict("PLAN_VERSION_CONFLICT", "plan must remain at the requested draft version", nil)
		}
		if err := tx.Create(item).Error; err != nil {
			return fmt.Errorf("create immutable assessment: %w", err)
		}
		entry.EntityID = item.ID
		if err := r.audit.RecordWithDB(ctx, tx, entry); err != nil {
			return err
		}
		planEntry := entry
		planEntry.Action = "dive_plan.transition"
		planEntry.EntityType = "dive_plan"
		planEntry.EntityID = plan.ID
		planEntry.BeforeSummary = string(constants.PlanDraft)
		planEntry.AfterSummary = fmt.Sprintf("%s assessment=%d", constants.PlanModeled, item.ID)
		return r.audit.RecordWithDB(ctx, tx, planEntry)
	})
	if err != nil {
		return fmt.Errorf("create modeled assessment transaction: %w", err)
	}
	return nil
}

func (r *DecompressionAssessmentRepository) Transition(ctx context.Context, plan model.DivePlan, assessment model.DecompressionAssessment, target constants.PlanStatus, actorID uint, entry audit.Entry) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		planChanges := map[string]any{"plan_status": target, "version": gorm.Expr("version + 1")}
		assessmentChanges := map[string]any{"assessment_status": string(target)}
		if target == constants.PlanApprovedTraining {
			now := time.Now().UTC()
			planChanges["reviewed_by"] = actorID
			assessmentChanges["reviewed_at"] = now
		}
		planResult := tx.Model(&model.DivePlan{}).Where("id = ? AND version = ? AND plan_status = ?", plan.ID, plan.Version, plan.PlanStatus).Updates(planChanges)
		if planResult.Error != nil {
			return fmt.Errorf("transition assessment plan: %w", planResult.Error)
		}
		if planResult.RowsAffected != 1 {
			return util.Conflict("PLAN_VERSION_CONFLICT", "plan state or version changed concurrently", nil)
		}
		// The assessment row can only move when it still represents the plan's
		// current review state, so a superseded snapshot can never be
		// submitted or approved, even if a caller bypasses service guards.
		assessmentResult := tx.Model(&model.DecompressionAssessment{}).Where("id = ? AND assessment_status = ?", assessment.ID, string(plan.PlanStatus)).Updates(assessmentChanges)
		if assessmentResult.Error != nil {
			return fmt.Errorf("transition assessment metadata: %w", assessmentResult.Error)
		}
		if assessmentResult.RowsAffected != 1 {
			return util.Conflict("ASSESSMENT_STATE_CONFLICT", "assessment review state changed concurrently or the result was superseded", nil)
		}
		entry.EntityID = assessment.ID
		if err := r.audit.RecordWithDB(ctx, tx, entry); err != nil {
			return err
		}
		planEntry := entry
		planEntry.EntityType = "dive_plan"
		planEntry.EntityID = plan.ID
		planEntry.Action = "dive_plan.transition"
		return r.audit.RecordWithDB(ctx, tx, planEntry)
	})
	if err != nil {
		return fmt.Errorf("transition assessment transaction: %w", err)
	}
	return nil
}

// Reopen returns a modeled/review plan to draft and, in the same transaction,
// marks its latest assessment superseded with the planner's reason. Both the
// plan version/status and the assessment status are used as optimistic guards,
// so an approved plan, a stale client version, or a concurrent reopen rejects
// the whole transaction without changing either row.
func (r *DecompressionAssessmentRepository) Reopen(ctx context.Context, plan model.DivePlan, assessment model.DecompressionAssessment, reason string, entry audit.Entry) (model.DivePlan, model.DecompressionAssessment, error) {
	updatedPlan := model.DivePlan{}
	updatedAssessment := model.DecompressionAssessment{}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest model.DecompressionAssessment
		if err := tx.Where("plan_id = ?", plan.ID).Order("created_at DESC, id DESC").First(&latest).Error; err != nil {
			return fmt.Errorf("load latest assessment for reopen %d: %w", plan.ID, err)
		}
		if latest.ID != assessment.ID || string(plan.PlanStatus) != latest.AssessmentStatus {
			return util.Conflict("ASSESSMENT_STATE_CONFLICT", "latest assessment no longer matches the plan review state", nil)
		}
		planResult := tx.Model(&model.DivePlan{}).
			Where("id = ? AND version = ? AND plan_status IN ?", plan.ID, plan.Version, []constants.PlanStatus{constants.PlanModeled, constants.PlanPendingReview}).
			Updates(map[string]any{"plan_status": constants.PlanDraft, "version": gorm.Expr("version + 1"), "reviewed_by": nil})
		if planResult.Error != nil {
			return fmt.Errorf("reopen dive plan: %w", planResult.Error)
		}
		if planResult.RowsAffected != 1 {
			return util.Conflict("PLAN_VERSION_CONFLICT", "plan state or version changed concurrently", nil)
		}
		assessmentResult := tx.Model(&model.DecompressionAssessment{}).
			Where("id = ? AND assessment_status = ? AND (supersede_reason = '' OR supersede_reason IS NULL)", latest.ID, latest.AssessmentStatus).
			Updates(map[string]any{"assessment_status": string(constants.AssessmentSuperseded), "supersede_reason": reason})
		if assessmentResult.Error != nil {
			return fmt.Errorf("supersede latest assessment: %w", assessmentResult.Error)
		}
		if assessmentResult.RowsAffected != 1 {
			return util.Conflict("ASSESSMENT_STATE_CONFLICT", "assessment review state changed concurrently", nil)
		}
		if err := tx.First(&updatedPlan, plan.ID).Error; err != nil {
			return fmt.Errorf("reload reopened plan: %w", err)
		}
		if err := tx.First(&updatedAssessment, latest.ID).Error; err != nil {
			return fmt.Errorf("reload superseded assessment: %w", err)
		}
		assessmentEntry := entry
		assessmentEntry.Action = "decompression_assessment.supersede"
		assessmentEntry.EntityType = "decompression_assessment"
		assessmentEntry.EntityID = latest.ID
		assessmentEntry.BeforeSummary = latest.AssessmentStatus
		assessmentEntry.AfterSummary = string(constants.AssessmentSuperseded) + " reason=" + reason
		if err := r.audit.RecordWithDB(ctx, tx, assessmentEntry); err != nil {
			return err
		}
		planEntry := entry
		planEntry.Action = "dive_plan.reopen"
		planEntry.EntityType = "dive_plan"
		planEntry.EntityID = plan.ID
		planEntry.BeforeSummary = string(plan.PlanStatus)
		planEntry.AfterSummary = fmt.Sprintf("%s assessment=%d reason=%s", constants.PlanDraft, latest.ID, reason)
		return r.audit.RecordWithDB(ctx, tx, planEntry)
	})
	if err != nil {
		return model.DivePlan{}, model.DecompressionAssessment{}, fmt.Errorf("reopen plan transaction: %w", err)
	}
	return updatedPlan, updatedAssessment, nil
}
