package service

import (
	"context"
	"fmt"
	"strings"

	"commercial-diving-decompression-control/backend/internal/audit"
	"commercial-diving-decompression-control/backend/internal/constants"
	"commercial-diving-decompression-control/backend/internal/decompression"
	"commercial-diving-decompression-control/backend/internal/dto"
	"commercial-diving-decompression-control/backend/internal/model"
	"commercial-diving-decompression-control/backend/internal/repository"
	"commercial-diving-decompression-control/backend/internal/util"
)

type DecompressionAssessmentService struct {
	assessments  *repository.DecompressionAssessmentRepository
	plans        *repository.DivePlanRepository
	profiles     *repository.DiverProfileRepository
	segments     *repository.ExposureSegmentRepository
	modelVersion string
	maxSegments  int
}

func NewDecompressionAssessmentService(assessments *repository.DecompressionAssessmentRepository, plans *repository.DivePlanRepository, profiles *repository.DiverProfileRepository, segments *repository.ExposureSegmentRepository, modelVersion string, maxSegments int) *DecompressionAssessmentService {
	return &DecompressionAssessmentService{assessments: assessments, plans: plans, profiles: profiles, segments: segments, modelVersion: modelVersion, maxSegments: maxSegments}
}

func (s *DecompressionAssessmentService) List(ctx context.Context, planID uint, status string, page, size int) ([]dto.AssessmentResponse, int64, error) {
	items, total, err := s.assessments.List(ctx, planID, status, page, size)
	if err != nil {
		return nil, 0, err
	}
	responses := make([]dto.AssessmentResponse, 0, len(items))
	for _, item := range items {
		response, decodeErr := dto.DecodeAssessment(item)
		if decodeErr != nil {
			return nil, 0, decodeErr
		}
		responses = append(responses, response)
	}
	return responses, total, nil
}

func (s *DecompressionAssessmentService) Get(ctx context.Context, id uint) (dto.AssessmentResponse, error) {
	item, err := s.assessments.Get(ctx, id)
	if err != nil {
		return dto.AssessmentResponse{}, err
	}
	return dto.DecodeAssessment(item)
}

func (s *DecompressionAssessmentService) Run(ctx context.Context, planID uint, req dto.RunAssessmentRequest, actor audit.Entry) (dto.AssessmentResponse, error) {
	plan, err := s.plans.Get(ctx, planID)
	if err != nil {
		return dto.AssessmentResponse{}, err
	}
	if plan.Version != req.PlanVersion {
		return dto.AssessmentResponse{}, util.Conflict("PLAN_VERSION_CONFLICT", "dive plan was changed by another user", nil)
	}
	if plan.PlanStatus != constants.PlanDraft {
		return dto.AssessmentResponse{}, util.Conflict("PLAN_NOT_DRAFT", "only a draft plan can run a new immutable assessment", nil)
	}
	profile, err := s.profiles.Get(ctx, plan.DiverProfileID)
	if err != nil {
		return dto.AssessmentResponse{}, err
	}
	segments, err := s.segments.ListByPlan(ctx, planID)
	if err != nil {
		return dto.AssessmentResponse{}, err
	}
	result, modelErr := decompression.Run(plan, profile, segments, s.modelVersion, s.maxSegments)
	if modelErr != nil {
		actor.EntityType = "dive_plan"
		_ = s.plans.ResetDraftAfterFailure(ctx, plan, modelErr.Error(), actor)
		return dto.AssessmentResponse{}, util.Unprocessable("MODEL_INPUT_INVALID", modelErr.Error(), modelErr)
	}
	snapshotJSON, curvesJSON, flagsJSON, assumptionsJSON, err := decompression.MarshalResult(result)
	if err != nil {
		return dto.AssessmentResponse{}, util.Internal(err)
	}
	item := model.DecompressionAssessment{PlanID: planID, AssessmentStatus: string(constants.PlanModeled), AlgorithmVersion: s.modelVersion, InputSnapshotJSON: snapshotJSON, CompartmentLoadsJSON: curvesJSON, RiskFlagsJSON: flagsJSON, HighestRiskBand: decompression.HighestRiskBand(result.RiskFlags), ComparativeScore: result.ComparativeScore, AssumptionsJSON: assumptionsJSON}
	actor.Action = "decompression_assessment.run"
	actor.EntityType = "decompression_assessment"
	actor.BeforeSummary = fmt.Sprintf("plan=%d version=%d algorithm=%s segments=%d", planID, plan.Version, s.modelVersion, len(segments))
	actor.AfterSummary = fmt.Sprintf("score=%.2f compartments=%d flags=%d immutable=true", result.ComparativeScore, len(result.Curves), len(result.RiskFlags))
	if err := s.assessments.CreateModeled(ctx, plan, &item, actor); err != nil {
		return dto.AssessmentResponse{}, err
	}
	return dto.DecodeAssessment(item)
}

func (s *DecompressionAssessmentService) Submit(ctx context.Context, id uint, req dto.TransitionPlanRequest, actor audit.Entry) (dto.AssessmentResponse, error) {
	return s.transition(ctx, id, req, constants.PlanPendingReview, actor)
}

func (s *DecompressionAssessmentService) Approve(ctx context.Context, id uint, req dto.TransitionPlanRequest, actor audit.Entry) (dto.AssessmentResponse, error) {
	return s.transition(ctx, id, req, constants.PlanApprovedTraining, actor)
}

// Reopen carries a modeled or pending-review plan back to draft and supersedes
// its latest assessment in one transaction. The planner must present the
// current plan version and a reason; approved plans, stale versions and
// concurrent attempts reject the whole request unchanged.
func (s *DecompressionAssessmentService) Reopen(ctx context.Context, planID uint, req dto.ReopenPlanRequest, actor audit.Entry) (dto.ReopenPlanResponse, error) {
	reason := strings.TrimSpace(req.Reason)
	plan, err := s.plans.Get(ctx, planID)
	if err != nil {
		return dto.ReopenPlanResponse{}, err
	}
	if plan.Version != req.Version {
		return dto.ReopenPlanResponse{}, util.Conflict("PLAN_VERSION_CONFLICT", "dive plan was changed by another user", nil)
	}
	if !constants.CanReopenPlan(plan.PlanStatus) {
		return dto.ReopenPlanResponse{}, util.Unprocessable("INVALID_PLAN_TRANSITION", fmt.Sprintf("only modeled or pending_supervisor_review plans can reopen, current status %s", plan.PlanStatus), nil)
	}
	assessment, err := s.assessments.LatestByPlan(ctx, planID)
	if err != nil {
		return dto.ReopenPlanResponse{}, err
	}
	if string(plan.PlanStatus) != assessment.AssessmentStatus {
		return dto.ReopenPlanResponse{}, util.Conflict("ASSESSMENT_STATE_CONFLICT", "latest assessment and plan review states do not match", nil)
	}
	actor.Action = "dive_plan.reopen"
	actor.EntityType = "dive_plan"
	actor.BeforeSummary = string(plan.PlanStatus)
	actor.AfterSummary = fmt.Sprintf("%s reason=%s", constants.PlanDraft, reason)
	updatedPlan, updatedAssessment, err := s.assessments.Reopen(ctx, plan, assessment, reason, actor)
	if err != nil {
		return dto.ReopenPlanResponse{}, err
	}
	profile, err := s.profiles.Get(ctx, updatedPlan.DiverProfileID)
	if err != nil {
		return dto.ReopenPlanResponse{}, err
	}
	planResponse, err := dto.NewDivePlanResponse(updatedPlan, profile.ProfileCode)
	if err != nil {
		return dto.ReopenPlanResponse{}, err
	}
	assessmentResponse, err := dto.DecodeAssessment(updatedAssessment)
	if err != nil {
		return dto.ReopenPlanResponse{}, err
	}
	return dto.ReopenPlanResponse{Plan: planResponse, Assessment: assessmentResponse}, nil
}

func (s *DecompressionAssessmentService) transition(ctx context.Context, id uint, req dto.TransitionPlanRequest, target constants.PlanStatus, actor audit.Entry) (dto.AssessmentResponse, error) {
	if req.TargetStatus != target {
		return dto.AssessmentResponse{}, util.Unprocessable("INVALID_PLAN_TRANSITION", fmt.Sprintf("endpoint requires target_status %s", target), nil)
	}
	assessment, err := s.assessments.Get(ctx, id)
	if err != nil {
		return dto.AssessmentResponse{}, err
	}
	if assessment.AssessmentStatus == string(constants.AssessmentSuperseded) {
		return dto.AssessmentResponse{}, util.Conflict("ASSESSMENT_SUPERSEDED", "this assessment was superseded by a plan reopen and can no longer be submitted or approved", nil)
	}
	plan, err := s.plans.Get(ctx, assessment.PlanID)
	if err != nil {
		return dto.AssessmentResponse{}, err
	}
	if plan.Version != req.Version {
		return dto.AssessmentResponse{}, util.Conflict("PLAN_VERSION_CONFLICT", "dive plan was changed by another user", nil)
	}
	if !constants.CanTransitionPlan(plan.PlanStatus, target) {
		return dto.AssessmentResponse{}, util.Unprocessable("INVALID_PLAN_TRANSITION", fmt.Sprintf("cannot transition from %s to %s", plan.PlanStatus, target), nil)
	}
	if assessment.AssessmentStatus != string(plan.PlanStatus) {
		return dto.AssessmentResponse{}, util.Conflict("ASSESSMENT_STATE_CONFLICT", "assessment and plan review states do not match", nil)
	}
	actor.Action = "decompression_assessment.submit_review"
	if target == constants.PlanApprovedTraining {
		actor.Action = "decompression_assessment.approve_training"
	}
	actor.EntityType = "decompression_assessment"
	actor.BeforeSummary = string(plan.PlanStatus)
	actor.AfterSummary = fmt.Sprintf("%s reason=%s human_review=true", target, strings.TrimSpace(req.Reason))
	if err := s.assessments.Transition(ctx, plan, assessment, target, actor.ActorID, actor); err != nil {
		return dto.AssessmentResponse{}, err
	}
	return s.Get(ctx, id)
}

func (s *DecompressionAssessmentService) Compare(ctx context.Context, leftID, rightID uint) (dto.AssessmentComparison, error) {
	left, err := s.Get(ctx, leftID)
	if err != nil {
		return dto.AssessmentComparison{}, err
	}
	right, err := s.Get(ctx, rightID)
	if err != nil {
		return dto.AssessmentComparison{}, err
	}
	return dto.AssessmentComparison{
		Left: left, Right: right, ScoreDelta: right.ComparativeScore - left.ComparativeScore,
		FlagDelta: len(right.RiskFlags) - len(left.RiskFlags),
		Summary: []string{
			fmt.Sprintf("Comparative index changed by %.2f points", right.ComparativeScore-left.ComparativeScore),
			fmt.Sprintf("Risk flag count changed from %d to %d", len(left.RiskFlags), len(right.RiskFlags)),
			"Differences describe deterministic training assumptions, not relative dive safety.",
		}, Disclaimer: dto.SafetyDisclaimer,
	}, nil
}
