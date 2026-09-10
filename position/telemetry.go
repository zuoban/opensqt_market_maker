package position

import "opensqt/telemetry"

func (spm *SuperPositionManager) SetTelemetry(recorder *telemetry.Recorder) {
	spm.performance.Store(recorder)
}

// TelemetryStateCounts reads container sizes without traversing or locking slots.
// Counts are individually consistent; they are not a transactional trading view.
func (spm *SuperPositionManager) TelemetryStateCounts() telemetry.StateCounts {
	s := telemetry.StateCounts{Slots: len(spm.slotIndex.snapshot())}
	spm.filledOrdersMu.RLock()
	s.FilledDedupKeys = len(spm.filledOrderKeys)
	s.FilledOrders = spm.filledOrderCount
	spm.filledOrdersMu.RUnlock()
	spm.terminalOrdersMu.RLock()
	s.TerminalOrderKeys = len(spm.terminalOrders)
	spm.terminalOrdersMu.RUnlock()
	spm.resolvedAbsentOrdersMu.Lock()
	s.PendingAbsenceKeys = len(spm.resolvedAbsentOrders)
	spm.resolvedAbsentOrdersMu.Unlock()
	return s
}
