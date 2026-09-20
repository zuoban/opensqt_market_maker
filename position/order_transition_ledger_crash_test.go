package position

import (
	"context"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"opensqt/internal/fillledger"
)

func correctionCrashView(t *testing.T, s *fillledger.SlotStore) fillledger.SlotView {
	t.Helper()
	v, err := s.Read([]string{"100"}, []int64{123, 456, 789})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Exercise real reducer/publication code, including corrections for an old
// terminal order while the same slot already belongs to a different order.
// os.Exit skips Close and all defers; this is process-crash, not power-loss QA.
func TestRealTerminalCorrectionCrashPreservesNewBinding(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      []replayEvent
		correction replayEvent
		qty, cost  float64
		buy, sell  float64
		pnl        float64
	}{
		{
			name: "late-buy-quantity",
			setup: []replayEvent{buyReplay("PARTIALLY_FILLED", "0.125", "12.375", 100),
				buyReplay("CANCELED", "0.25", "25", 200)},
			correction: buyReplay("CANCELED", "0.375", "37.875", 400),
			qty:        .375, cost: 37.875, buy: .375,
		},
		{
			name: "late-sell-quantity",
			setup: []replayEvent{buyReplay("FILLED", "0.5", "47.5", 100),
				sellReplay("CANCELED", "0.25", "25.5", "0.6", 200)},
			correction: sellReplay("CANCELED", "0.375", "39", "0.7", 400),
			qty:        .125, cost: 11.875, buy: .5, sell: .375, pnl: .7,
		},
		{
			name: "late-sell-pnl-only",
			setup: []replayEvent{buyReplay("FILLED", "0.5", "47.5", 100),
				sellReplay("CANCELED", "0.25", "25.5", "0.6", 200)},
			correction: sellReplay("CANCELED", "0.25", "25.5", "0.55", 400),
			qty:        .25, cost: 23.75, buy: .5, sell: .25, pnl: .55,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newOrder := sellReplay("NEW", "0", "0", "0", 300)
			newOrder.Update.OrderID, newOrder.Update.ClientOrderID = 789, "10000_S_1700000000003"
			setup := append(tc.setup, newOrder)
			prepare := func(t *testing.T) (*fillledger.SlotStore, string, *ledgerReplay) {
				t.Helper()
				s, path := newReplayLedger(t)
				r := restoreReplay(t, s)
				for _, event := range setup {
					if err := r.process(event); err != nil {
						t.Fatal(err)
					}
				}
				return s, path, r
			}
			// A non-crashing run supplies the complete expected durable records,
			// checked independently against known quantities/cost/PNL below.
			baseline, _, model := prepare(t)
			before, beforeSlot := correctionCrashView(t, baseline), replaySnapshot(model)
			if err := model.process(tc.correction); err != nil {
				t.Fatal(err)
			}
			after, afterSlot := correctionCrashView(t, baseline), replaySnapshot(model)
			assertClose(t, "corrected quantity", afterSlot.PositionQty, tc.qty)
			assertClose(t, "corrected cost", afterSlot.PositionCost, tc.cost)
			assertClose(t, "corrected buys", model.spm.GetTotalBuyQty(), tc.buy)
			assertClose(t, "corrected sells", model.spm.GetTotalSellQty(), tc.sell)
			assertClose(t, "corrected PNL", model.spm.GetRealizedPNL(), tc.pnl)
			binding := afterSlot
			binding.PositionQty, binding.PositionCost = beforeSlot.PositionQty, beforeSlot.PositionCost
			if !reflect.DeepEqual(binding, beforeSlot) || afterSlot.OrderID != 789 {
				t.Fatal("old correction changed the new order binding/progress")
			}
			if err := baseline.Close(); err != nil {
				t.Fatal(err)
			}
			for _, phase := range []string{"before-commit", "after-durable-before-publish", "after-publish"} {
				t.Run(phase, func(t *testing.T) {
					s, path, _ := prepare(t)
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
					ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRealTransitionLedgerCrashChild$")
					cmd.Env = append(os.Environ(), "OPENSQT_TEST_REPLAY_CRASH_PATH="+path,
						"OPENSQT_TEST_REPLAY_CRASH_PHASE="+phase, "OPENSQT_TEST_REPLAY_CRASH_EVENT="+string(replayJSON(tc.correction)))
					output, err := cmd.CombinedOutput()
					if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 39 {
						t.Fatalf("exit=%v output=%s", err, output)
					}
					recovered, err := fillledger.OpenSlots(path, replayScope())
					if err != nil {
						t.Fatal(err)
					}
					defer recovered.Close()
					want := after
					if phase == "before-commit" {
						want = before
					}
					if got := correctionCrashView(t, recovered); !reflect.DeepEqual(got, want) {
						t.Fatal("crash recovery mixed old/new slot, order or accounting records")
					}
					r := restoreReplay(t, recovered)
					// Repeated corrections, an older terminal event and an old NEW
					// receipt must leave the final durable revision unchanged.
					for _, event := range []replayEvent{tc.correction, tc.correction, tc.setup[1], newOrder} {
						if err := r.process(event); err != nil {
							t.Fatal(err)
						}
					}
					if !reflect.DeepEqual(correctionCrashView(t, recovered), after) || !reflect.DeepEqual(replaySnapshot(r), afterSlot) {
						t.Fatal("replay repeated correction or replaced the new binding")
					}
					assertClose(t, "replayed buys", r.spm.GetTotalBuyQty(), tc.buy)
					assertClose(t, "replayed sells", r.spm.GetTotalSellQty(), tc.sell)
					assertClose(t, "replayed PNL", r.spm.GetRealizedPNL(), tc.pnl)
					wantNotices := 0
					if phase == "before-commit" {
						wantNotices = 1
					}
					if r.notices != wantNotices || uint64(r.spm.filledOrderCount) != after.Totals.FilledOrders {
						t.Fatal("replay duplicated notification or completed order count")
					}
				})
			}
		})
	}
}
