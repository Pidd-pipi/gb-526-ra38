import { useEffect, useState, type FormEvent } from 'react'
import { Button, Dialog, DialogActions, DialogContent, DialogTitle, TextField } from '@mui/material'
import { Undo2 } from 'lucide-react'

interface ReopenDialogProps {
  open: boolean
  planCode: string
  version: number
  busy: boolean
  onClose: () => void
  onConfirm: (reason: string) => Promise<void>
}

export function ReopenDialog({ open, planCode, version, busy, onClose, onConfirm }: ReopenDialogProps) {
  const [reason, setReason] = useState('')
  useEffect(() => {
    if (open) setReason('')
  }, [open])
  const trimmed = reason.trim()
  const submit = async (event: FormEvent) => {
    event.preventDefault()
    if (trimmed.length < 3) return
    await onConfirm(trimmed)
  }
  return (
    <Dialog open={open} onClose={busy ? undefined : onClose} maxWidth="sm" fullWidth>
      <DialogTitle>Reopen and replace · {planCode} v{version}</DialogTitle>
      <form onSubmit={submit}>
        <DialogContent>
          <p style={{ margin: '0 0 16px', color: '#4a5753', fontSize: 13 }}>
            The plan returns to draft and its latest assessment is marked <strong>superseded</strong>. The old snapshot stays readable but can never be submitted or approved again.
          </p>
          <TextField
            label="Reopen reason"
            value={reason}
            onChange={(event) => setReason(event.target.value)}
            fullWidth
            required
            autoFocus
            multiline
            minRows={2}
            inputProps={{ minLength: 3, maxLength: 300 }}
            helperText={`${trimmed.length}/300 · recorded in the append-only audit trail`}
          />
        </DialogContent>
        <DialogActions>
          <Button onClick={onClose} disabled={busy}>Cancel</Button>
          <Button type="submit" variant="contained" startIcon={<Undo2 size={16} />} disabled={busy || trimmed.length < 3}>Reopen plan</Button>
        </DialogActions>
      </form>
    </Dialog>
  )
}
