package telemetry

import (
	"sync/atomic"
	"time"
)

// Monitor tracks lightweight lock-free runtime counters with zero lock contention
// on the request hot path.
type Monitor struct {
	started         time.Time
	active          atomic.Int64
	activeStreams   atomic.Int64
	total           atomic.Uint64
	success         atomic.Uint64
	errors          atomic.Uint64
	attemptsTotal   atomic.Uint64
	attemptsSuccess atomic.Uint64
	attemptsFailed  atomic.Uint64
	inputTokens     atomic.Uint64
	outputTokens    atomic.Uint64
	cachedTokens    atomic.Uint64
	reasoningTokens atomic.Uint64
}

func NewMonitor() *Monitor {
	return &Monitor{started: time.Now().UTC()}
}

func (m *Monitor) BeginStream() { m.activeStreams.Add(1) }

func (m *Monitor) EndStream() { m.activeStreams.Add(-1) }

func (m *Monitor) Record(status int, meta *RequestMeta) {
	succeeded := status >= 200 && status < 400 && (meta == nil || meta.Outcome == "")
	m.total.Add(1)
	if succeeded {
		m.success.Add(1)
	} else {
		m.errors.Add(1)
	}
	if meta != nil && meta.UsageReported {
		if meta.Usage.Input > 0 {
			m.inputTokens.Add(uint64(meta.Usage.Input))
		}
		if meta.Usage.Output > 0 {
			m.outputTokens.Add(uint64(meta.Usage.Output))
		}
		if meta.Usage.Cached > 0 {
			m.cachedTokens.Add(uint64(meta.Usage.Cached))
		}
		if meta.Usage.Reasoning > 0 {
			m.reasoningTokens.Add(uint64(meta.Usage.Reasoning))
		}
	}
}

func (m *Monitor) RecordAttempt(success bool) {
	m.attemptsTotal.Add(1)
	if success {
		m.attemptsSuccess.Add(1)
	} else {
		m.attemptsFailed.Add(1)
	}
}
