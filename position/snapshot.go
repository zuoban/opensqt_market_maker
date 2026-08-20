package position

import (
	"sort"
	"time"
)

// SlotSnapshot 单个槽位的只读拷贝
type SlotSnapshot struct {
	Price             float64   `json:"price"`
	PriceText         string    `json:"priceText"`
	PositionStatus    string    `json:"positionStatus"`
	PositionQty       float64   `json:"positionQty"`
	PositionQtyText   string    `json:"positionQtyText"`
	OrderID           int64     `json:"orderId"`
	ClientOID         string    `json:"clientOid"`
	OrderSide         string    `json:"orderSide"`
	OrderStatus       string    `json:"orderStatus"`
	OrderPrice        float64   `json:"orderPrice"`
	OrderFilledQty    float64   `json:"orderFilledQty"`
	OrderCreatedAt    time.Time `json:"orderCreatedAt"`
	SlotStatus        string    `json:"slotStatus"`
	PostOnlyFailCount int       `json:"postOnlyFailCount"`
	InBuyWindow       bool      `json:"inBuyWindow"`
	InSellWindow      bool      `json:"inSellWindow"`
}

// PositionSnapshot 仓位管理器只读快照
type PositionSnapshot struct {
	Initialized          bool                `json:"initialized"`
	Symbol               string              `json:"symbol"`
	BaseAsset            string              `json:"baseAsset"`
	AnchorPrice          float64             `json:"anchorPrice"`
	LastPrice            float64             `json:"lastPrice"`
	GridPrice            float64             `json:"gridPrice"`
	PriceDecimals        int                 `json:"priceDecimals"`
	QuantityDecimals     int                 `json:"quantityDecimals"`
	PriceInterval        float64             `json:"priceInterval"`
	OrderQuantity        float64             `json:"orderQuantity"`
	BuyWindowSize        int                 `json:"buyWindowSize"`
	SellWindowSize       int                 `json:"sellWindowSize"`
	BuyWindowPrices      []float64           `json:"buyWindowPrices"`
	SellWindowPrices     []float64           `json:"sellWindowPrices"`
	Slots                []SlotSnapshot      `json:"slots"`
	FilledOrders         []FilledOrderRecord `json:"filledOrders"`
	FilledHourly         []HourlyFillBucket  `json:"filledHourly"`
	FilledOrderCount     int64               `json:"filledOrderCount"`
	FilledSlotCount      int                 `json:"filledSlotCount"`
	PositionQty          float64             `json:"positionQty"`
	ActiveBuyOrders      int                 `json:"activeBuyOrders"`
	ActiveSellOrders     int                 `json:"activeSellOrders"`
	TotalBuyQty          float64             `json:"totalBuyQty"`
	TotalSellQty         float64             `json:"totalSellQty"`
	EstimatedProfit      float64             `json:"estimatedProfit"`
	RealizedPNL          float64             `json:"realizedPnl"`
	ReconcileCount       int64               `json:"reconcileCount"`
	LastReconcileTime    time.Time           `json:"lastReconcileTime"`
	MarginLocked         bool                `json:"marginLocked"`
	MarginLockRemainingS float64             `json:"marginLockRemainingSec"`
}

func isActiveOrderStatus(status string) bool {
	return status == OrderStatusPlaced || status == OrderStatusConfirmed || status == OrderStatusPartiallyFilled
}

// GetLastReconcileTime 最近一次对账时间
func (spm *SuperPositionManager) GetLastReconcileTime() time.Time {
	if v := spm.lastReconcileTime.Load(); v != nil {
		if t, ok := v.(time.Time); ok {
			return t
		}
	}
	return time.Time{}
}

// MarginLockRemaining 保证金锁定剩余时间
func (spm *SuperPositionManager) MarginLockRemaining() time.Duration {
	spm.mu.RLock()
	defer spm.mu.RUnlock()
	if !spm.insufficientMargin {
		return 0
	}
	rem := spm.marginLockDuration - time.Since(spm.marginLockTime)
	if rem < 0 {
		return 0
	}
	return rem
}

// Snapshot 拷贝当前槽位与汇总，持锁期间只读字段，不做序列化。
func (spm *SuperPositionManager) Snapshot() PositionSnapshot {
	spm.mu.RLock()
	snap := PositionSnapshot{
		Initialized:      spm.isInitialized.Load(),
		AnchorPrice:      spm.anchorPrice,
		PriceDecimals:    spm.priceDecimals,
		QuantityDecimals: spm.quantityDecimals,
		MarginLocked:     spm.insufficientMargin,
	}
	if spm.insufficientMargin {
		rem := spm.marginLockDuration - time.Since(spm.marginLockTime)
		if rem > 0 {
			snap.MarginLockRemainingS = rem.Seconds()
		}
	}
	spm.mu.RUnlock()

	if spm.config != nil {
		snap.Symbol = spm.config.Trading.Symbol
		snap.PriceInterval = spm.config.Trading.PriceInterval
		snap.OrderQuantity = spm.config.Trading.OrderQuantity
		snap.BuyWindowSize = spm.config.Trading.BuyWindowSize
		snap.SellWindowSize = spm.config.Trading.SellWindowSize
	}
	if spm.exchange != nil {
		snap.BaseAsset = spm.exchange.GetBaseAsset()
	}

	spm.filledOrdersMu.RLock()
	snap.FilledOrders = make([]FilledOrderRecord, len(spm.filledOrders))
	for i := range spm.filledOrders {
		snap.FilledOrders[len(spm.filledOrders)-1-i] = spm.filledOrders[i]
	}
	snap.FilledHourly = spm.hourlyFillSnapshotLocked(time.Now())
	snap.FilledOrderCount = spm.filledOrderCount
	spm.filledOrdersMu.RUnlock()

	lastPrice, _ := spm.lastMarketPrice.Load().(float64)
	snap.LastPrice = lastPrice
	snap.TotalBuyQty = spm.GetTotalBuyQty()
	snap.TotalSellQty = spm.GetTotalSellQty()
	snap.EstimatedProfit = snap.TotalSellQty * snap.PriceInterval
	snap.RealizedPNL = spm.GetRealizedPNL()
	snap.ReconcileCount = spm.GetReconcileCount()
	snap.LastReconcileTime = spm.GetLastReconcileTime()

	buyWindow := map[string]bool{}
	sellWindow := map[string]bool{}
	if snap.AnchorPrice > 0 && lastPrice > 0 {
		grid := spm.findNearestGridPrice(lastPrice)
		snap.GridPrice = grid
		buyPrices := spm.calculateSlotPrices(grid, snap.BuyWindowSize, "down")
		sellPrices := spm.calculateSlotPrices(grid, snap.SellWindowSize, "up")
		snap.BuyWindowPrices = buyPrices
		snap.SellWindowPrices = sellPrices
		for _, p := range buyPrices {
			buyWindow[formatPrice(p, snap.PriceDecimals)] = true
		}
		for _, p := range sellPrices {
			sellWindow[formatPrice(p, snap.PriceDecimals)] = true
		}
	}

	slots := make([]SlotSnapshot, 0, 32)
	spm.slots.Range(func(key, value interface{}) bool {
		price := key.(float64)
		slot := value.(*InventorySlot)
		slot.mu.RLock()
		item := SlotSnapshot{
			Price:             price,
			PriceText:         formatPrice(price, snap.PriceDecimals),
			PositionStatus:    slot.PositionStatus,
			PositionQty:       slot.PositionQty,
			PositionQtyText:   formatPrice(slot.PositionQty, snap.QuantityDecimals),
			OrderID:           slot.OrderID,
			ClientOID:         slot.ClientOID,
			OrderSide:         slot.OrderSide,
			OrderStatus:       slot.OrderStatus,
			OrderPrice:        slot.OrderPrice,
			OrderFilledQty:    slot.OrderFilledQty,
			OrderCreatedAt:    slot.OrderCreatedAt,
			SlotStatus:        slot.SlotStatus,
			PostOnlyFailCount: slot.PostOnlyFailCount,
		}
		slot.mu.RUnlock()

		item.InBuyWindow = buyWindow[item.PriceText]
		item.InSellWindow = sellWindow[item.PriceText]
		if item.PositionStatus == PositionStatusFilled && item.PositionQty > 0.001 {
			snap.FilledSlotCount++
			snap.PositionQty += item.PositionQty
		}
		if isActiveOrderStatus(item.OrderStatus) {
			switch item.OrderSide {
			case "BUY":
				snap.ActiveBuyOrders++
			case "SELL":
				snap.ActiveSellOrders++
			}
		}
		slots = append(slots, item)
		return true
	})

	sort.Slice(slots, func(i, j int) bool {
		return slots[i].Price > slots[j].Price
	})
	snap.Slots = slots
	return snap
}
