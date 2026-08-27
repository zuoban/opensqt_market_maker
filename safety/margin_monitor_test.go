package safety

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"opensqt/config"
	"opensqt/exchange"
)

type marginAccountSourceStub struct {
	mu      sync.Mutex
	account *exchange.Account
	err     error
	calls   int
}

func (s *marginAccountSourceStub) GetAccount(context.Context) (*exchange.Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if s.account == nil {
		return nil, nil
	}
	copyAccount := *s.account
	return &copyAccount, nil
}

func (s *marginAccountSourceStub) GetQuoteAsset() string { return "USDT" }

func TestCalculateMarginUsage(t *testing.T) {
	tests := []struct {
		name      string
		account   *exchange.Account
		wantUsed  float64
		wantUsage float64
		wantErr   string
	}{
		{
			name: "position and order margin",
			account: &exchange.Account{
				TotalWalletBalance: 100,
				TotalMarginBalance: 120,
				AvailableBalance:   75,
			},
			wantUsed:  45,
			wantUsage: 37.5,
		},
		{
			name: "available can exceed margin without negative usage",
			account: &exchange.Account{
				TotalWalletBalance: 100,
				TotalMarginBalance: 100,
				AvailableBalance:   105,
			},
		},
		{
			name:    "empty account",
			account: &exchange.Account{},
			wantErr: "必须大于0",
		},
		{
			name: "negative available can exceed one hundred percent",
			account: &exchange.Account{
				TotalWalletBalance: 100,
				TotalMarginBalance: 50,
				AvailableBalance:   -10,
			},
			wantUsed:  60,
			wantUsage: 120,
		},
		{
			name: "non-positive equity is critical",
			account: &exchange.Account{
				TotalWalletBalance: -1,
				TotalMarginBalance: -1,
				AvailableBalance:   -2,
			},
			wantErr: "必须大于0",
		},
		{name: "nil", wantErr: "空账户"},
		{
			name: "non finite",
			account: &exchange.Account{
				TotalWalletBalance: math.NaN(),
			},
			wantErr: "不是有限数值",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			used, usage, err := calculateMarginUsage(tt.account)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("calculateMarginUsage() error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if math.Abs(used-tt.wantUsed) > 1e-9 || math.Abs(usage-tt.wantUsage) > 1e-9 {
				t.Fatalf("calculateMarginUsage() = used %.12f usage %.12f, want %.12f / %.12f",
					used, usage, tt.wantUsed, tt.wantUsage)
			}
		})
	}
}

func TestMarginMonitorTriggersAboveLimitAndLatches(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trading.MaxMarginUsagePercent = 40
	source := &marginAccountSourceStub{account: &exchange.Account{
		TotalWalletBalance: 100,
		TotalMarginBalance: 100,
		AvailableBalance:   60,
	}}
	monitor := NewMarginMonitor(cfg, source)

	var handled []MarginSnapshot
	monitor.SetLimitHandler(func(snap MarginSnapshot) {
		handled = append(handled, snap)
	})
	if err := monitor.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := monitor.Snapshot()
	if !first.Ready || first.Triggered || first.UsagePercent != 40 || first.LimitPercent != 40 {
		t.Fatalf("first snapshot = %+v", first)
	}
	if len(handled) != 0 {
		t.Fatalf("handler snapshots = %+v", handled)
	}

	source.account.AvailableBalance = 59
	if err := monitor.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	triggered := monitor.Snapshot()
	if !triggered.Triggered || triggered.UsagePercent != 41 {
		t.Fatalf("triggered snapshot = %+v", triggered)
	}
	if len(handled) != 1 || !handled[0].Triggered {
		t.Fatalf("handler snapshots = %+v", handled)
	}

	// 即使全撤释放了挂单保证金，本次运行也不得自动恢复。
	source.account.AvailableBalance = 100
	if err := monitor.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	second := monitor.Snapshot()
	if !second.Triggered || second.UsagePercent != 0 {
		t.Fatalf("latched snapshot = %+v", second)
	}
	if len(handled) != 1 {
		t.Fatalf("handler called %d times, want once", len(handled))
	}
}

func TestMarginMonitorLateHandlerReceivesLatchedTrigger(t *testing.T) {
	cfg := &config.Config{}
	cfg.Trading.MaxMarginUsagePercent = 25
	source := &marginAccountSourceStub{account: &exchange.Account{
		TotalMarginBalance: 100,
		AvailableBalance:   50,
	}}
	monitor := NewMarginMonitor(cfg, source)
	if err := monitor.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	called := 0
	monitor.SetLimitHandler(func(snap MarginSnapshot) {
		called++
		if !snap.Triggered {
			t.Fatalf("late handler snapshot = %+v", snap)
		}
	})
	if called != 1 {
		t.Fatalf("late handler calls = %d, want 1", called)
	}
}

func TestMarginMonitorFailureKeepsLastReadingButBecomesStale(t *testing.T) {
	source := &marginAccountSourceStub{account: &exchange.Account{
		TotalMarginBalance: 100,
		AvailableBalance:   80,
	}}
	monitor := NewMarginMonitor(nil, source)
	monitor.staleAfter = time.Hour
	if err := monitor.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	wantErr := errors.New("account unavailable")
	source.err = wantErr
	if err := monitor.refresh(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("refresh() error = %v, want %v", err, wantErr)
	}
	snap := monitor.Snapshot()
	if !snap.Ready || !snap.Stale || snap.UsagePercent != 20 || snap.Error == "" {
		t.Fatalf("stale snapshot = %+v", snap)
	}

	monitor.mu.Lock()
	monitor.lastSuccess = time.Now().Add(-2 * time.Hour)
	monitor.mu.Unlock()
	if monitor.IsReady() {
		t.Fatal("expired account snapshot remained ready")
	}
}

func TestMarginMonitorStartRequiresSuccessfulInitialRead(t *testing.T) {
	source := &marginAccountSourceStub{err: errors.New("boom")}
	monitor := NewMarginMonitor(nil, source)
	if err := monitor.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "首次读取失败") {
		t.Fatalf("Start() error = %v", err)
	}
	if monitor.IsReady() {
		t.Fatal("failed initial read became ready")
	}
}
