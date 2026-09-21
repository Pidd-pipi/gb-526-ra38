import { Chip } from '@mui/material'
import type { PlanStatus } from '@/types/plan'

const labels: Record<PlanStatus, string> = {
  draft: 'DRAFT',
  modeled: 'MODELED',
  pending_supervisor_review: 'SUPERVISOR REVIEW',
  approved_for_training: 'TRAINING APPROVED',
  archived: 'ARCHIVED',
}

const extraLabels: Record<string, string> = {
  superseded: 'SUPERSEDED',
}

export function PlanStatusBadge({ status }: { status: PlanStatus | string }) {
  const value = status as PlanStatus
  const label = labels[value] ?? extraLabels[status] ?? status.replaceAll('_', ' ').toUpperCase()
  return <Chip size="small" className={`status-badge status-${value}`} label={label} />
}
