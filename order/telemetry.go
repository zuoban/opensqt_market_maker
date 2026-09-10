package order

import (
	"context"

	"opensqt/telemetry"
)

func (oe *ExchangeOrderExecutor) SetTelemetry(recorder *telemetry.Recorder) {
	oe.performance.Store(recorder)
}

func (oe *ExchangeOrderExecutor) waitForRateLimit(ctx context.Context) error {
	performance := oe.performance.Load()
	started := performance.Start()
	err := oe.rateLimiter.Wait(ctx)
	performance.ObserveSince(telemetry.RateLimitWait, started, err != nil)
	return err
}
