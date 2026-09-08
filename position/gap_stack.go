package position

import (
	"math"

	"opensqt/logger"
)

const maxGapStackSlotsCap = 10

func (spm *SuperPositionManager) maxGapStackSlots() int {
	if spm == nil || spm.config == nil {
		return 0
	}
	n := spm.config.Execution.MaxGapStackSlots
	if n <= 0 {
		return 0
	}
	if n > maxGapStackSlotsCap {
		return maxGapStackSlotsCap
	}
	return n
}

func sameGridPrice(a, b float64) bool {
	return math.Abs(a-b) <= fillQtyTolerance
}

func (spm *SuperPositionManager) gapStackChildPrices(parentPrice float64, stackCount int) []float64 {
	if spm == nil || stackCount < 2 || parentPrice <= 0 {
		return nil
	}
	interval := 0.0
	if spm.config != nil {
		interval = spm.config.Trading.PriceInterval
	}
	if interval <= 0 {
		return nil
	}
	children := make([]float64, 0, stackCount-1)
	for i := 1; i < stackCount; i++ {
		price := roundPrice(parentPrice+float64(i)*interval, spm.priceDecimals)
		if price <= parentPrice {
			break
		}
		children = append(children, price)
	}
	return children
}

type gapSlotKind int

const (
	gapSlotEmpty gapSlotKind = iota
	gapSlotFilled
	gapSlotBlocked
)

func (spm *SuperPositionManager) gapSlotKindAt(price float64) gapSlotKind {
	if spm == nil {
		return gapSlotEmpty
	}
	raw, ok := spm.slots.Load(price)
	if !ok {
		return gapSlotEmpty
	}
	slot, _ := raw.(*InventorySlot)
	if slot == nil {
		return gapSlotEmpty
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	return gapSlotKindLocked(slot)
}

func gapSlotKindLocked(slot *InventorySlot) gapSlotKind {
	if slot == nil {
		return gapSlotEmpty
	}
	if slot.PositionQty > fillQtyTolerance || slot.PositionStatus == PositionStatusFilled {
		return gapSlotFilled
	}
	if slot.OrderStatus == OrderStatusCancelRequested &&
		slot.OrderSide == "BUY" &&
		slot.OrderFilledQty <= fillQtyTolerance &&
		slot.PositionQty <= fillQtyTolerance {
		// 正在撤销的窗口外买单视为可合并的空格，但不能占用其订单身份。
		return gapSlotEmpty
	}
	if slot.OrderID != 0 || slot.ClientOID != "" {
		return gapSlotBlocked
	}
	if slot.SlotStatus == SlotStatusPending || slot.SlotStatus == SlotStatusLocked {
		return gapSlotBlocked
	}
	return gapSlotEmpty
}

func (spm *SuperPositionManager) collectGapStackSlots(currentGridPrice float64) (children []float64, blocked bool) {
	max := spm.maxGapStackSlots()
	if max <= 0 || currentGridPrice <= 0 || spm.config == nil {
		return nil, false
	}
	interval := spm.config.Trading.PriceInterval
	if interval <= 0 {
		return nil, false
	}
	for i := 1; i <= max; i++ {
		price := roundPrice(currentGridPrice+float64(i)*interval, spm.priceDecimals)
		switch spm.gapSlotKindAt(price) {
		case gapSlotFilled:
			return children, false
		case gapSlotBlocked:
			return children, true
		default:
			children = append(children, price)
		}
	}
	return children, false
}

func (spm *SuperPositionManager) reserveGapStackChildren(parentPrice float64, children []float64) {
	if spm == nil || parentPrice <= 0 {
		return
	}
	for _, price := range children {
		if price <= parentPrice {
			continue
		}
		slot := spm.getOrCreateSlot(price)
		slot.mu.Lock()
		if gapSlotKindLocked(slot) == gapSlotEmpty &&
			slot.OrderID == 0 && slot.ClientOID == "" &&
			slot.SlotStatus == SlotStatusFree {
			slot.stackedParentPrice = parentPrice
		}
		slot.mu.Unlock()
	}
}

func (spm *SuperPositionManager) releaseGapStackChildren(parentPrice float64, stackCount int) {
	if spm == nil || parentPrice <= 0 || stackCount < 2 {
		return
	}
	for _, price := range spm.gapStackChildPrices(parentPrice, stackCount) {
		raw, ok := spm.slots.Load(price)
		if !ok {
			continue
		}
		slot, _ := raw.(*InventorySlot)
		if slot == nil {
			continue
		}
		slot.mu.Lock()
		if slot.stackedParentPrice == parentPrice &&
			slot.PositionQty <= fillQtyTolerance &&
			slot.OrderID == 0 && slot.ClientOID == "" {
			slot.stackedParentPrice = 0
		}
		slot.mu.Unlock()
	}
}

func (spm *SuperPositionManager) ensureGapStackChildReservations() {
	if spm == nil || spm.maxGapStackSlots() <= 0 {
		return
	}
	type stackedBuy struct {
		price float64
		count int
	}
	var parents []stackedBuy
	spm.forEachSlot(func(price float64, slot *InventorySlot) bool {
		slot.mu.RLock()
		count := slot.gapStackCount
		liveBuy := slot.OrderSide == "BUY" && orderMayExistLocked(slot)
		slot.mu.RUnlock()
		if liveBuy && count >= 2 {
			parents = append(parents, stackedBuy{price: price, count: count})
		}
		return true
	})
	for _, item := range parents {
		spm.reserveGapStackChildren(item.price, spm.gapStackChildPrices(item.price, item.count))
	}
}

func (spm *SuperPositionManager) undersizedGapStackBuy(currentGridPrice float64) (outOfWindowBuy, bool) {
	children, blocked := spm.collectGapStackSlots(currentGridPrice)
	desired := 1 + len(children)
	if blocked || desired < 2 {
		return outOfWindowBuy{}, false
	}
	raw, ok := spm.slots.Load(currentGridPrice)
	if !ok {
		return outOfWindowBuy{}, false
	}
	slot, _ := raw.(*InventorySlot)
	if slot == nil {
		return outOfWindowBuy{}, false
	}
	slot.mu.RLock()
	defer slot.mu.RUnlock()
	if !cancelableLiveBuyLocked(slot) || slot.PositionQty > fillQtyTolerance {
		return outOfWindowBuy{}, false
	}
	unitPrice := slot.OrderPrice
	if unitPrice <= 0 {
		unitPrice = currentGridPrice
	}
	unit := spm.gridBuyQuantity(unitPrice)
	if unit <= 0 {
		return outOfWindowBuy{}, false
	}
	want := roundPrice(unit*float64(desired), spm.quantityDecimals)
	if slot.OrderQuantity+fillQtyTolerance >= want {
		return outOfWindowBuy{}, false
	}
	return outOfWindowBuy{
		price:     currentGridPrice,
		orderID:   slot.OrderID,
		clientOID: slot.ClientOID,
	}, true
}

func (spm *SuperPositionManager) redistributeGapStackedInventory() {
	if spm == nil {
		return
	}
	type stackedSlot struct {
		price float64
		count int
	}
	var parents []stackedSlot
	max := spm.maxGapStackSlots()
	spm.forEachSlot(func(price float64, slot *InventorySlot) bool {
		slot.mu.RLock()
		defer slot.mu.RUnlock()
		if slot.PositionStatus != PositionStatusFilled || slot.PositionQty <= fillQtyTolerance {
			return true
		}
		if slot.OrderSide == "BUY" && orderMayExistLocked(slot) {
			return true
		}
		count := slot.gapStackCount
		if count < 2 && max > 0 {
			unit := spm.gridBuyQuantity(slot.Price)
			if unit > 0 && slot.PositionQty+fillQtyTolerance >= 2*unit {
				inferred := int(math.Floor((slot.PositionQty + fillQtyTolerance) / unit))
				if inferred > max+1 {
					inferred = max + 1
				}
				count = inferred
			}
		}
		if count >= 2 {
			parents = append(parents, stackedSlot{price: price, count: count})
		}
		return true
	})
	for _, item := range parents {
		spm.splitGapStackedSlot(item.price, item.count)
	}
}

func (spm *SuperPositionManager) splitGapStackedSlot(parentPrice float64, stackCount int) {
	children := spm.gapStackChildPrices(parentPrice, stackCount)
	if len(children) == 0 {
		return
	}
	parent := spm.getOrCreateSlot(parentPrice)
	parent.mu.Lock()
	defer parent.mu.Unlock()
	if parent.PositionStatus != PositionStatusFilled || parent.PositionQty <= fillQtyTolerance {
		return
	}
	if parent.OrderSide == "BUY" && orderMayExistLocked(parent) {
		return
	}

	share := roundPrice(parent.PositionQty/float64(stackCount), spm.quantityDecimals)
	if share <= fillQtyTolerance {
		parent.gapStackCount = 1
		return
	}
	interval := 0.0
	if spm.config != nil {
		interval = spm.config.Trading.PriceInterval
	}

	for _, childPrice := range children {
		if parent.PositionQty+fillQtyTolerance < 2*share {
			break
		}
		sellPrice := roundPrice(childPrice+interval, spm.priceDecimals)
		if sellPrice > 0 && spm.reduceOnlyNotionalTooSmall(sellPrice, share) {
			continue
		}
		child := spm.getOrCreateSlot(childPrice)
		child.mu.Lock()
		accept := child.PositionQty <= fillQtyTolerance &&
			child.PositionStatus != PositionStatusFilled &&
			child.OrderID == 0 && child.ClientOID == "" &&
			(child.SlotStatus == SlotStatusFree || child.stackedParentPrice == parentPrice) &&
			child.OrderStatus != OrderStatusPlaced &&
			child.OrderStatus != OrderStatusConfirmed &&
			child.OrderStatus != OrderStatusPartiallyFilled &&
			child.OrderStatus != OrderStatusCancelRequested
		if accept {
			child.PositionQty = share
			child.PositionStatus = PositionStatusFilled
			child.SlotStatus = SlotStatusFree
			child.stackedParentPrice = 0
			child.gapStackCount = 1
			parent.PositionQty = roundPrice(parent.PositionQty-share, spm.quantityDecimals)
			if parent.PositionQty < fillQtyTolerance {
				parent.PositionQty = 0
			}
			logger.Info("✅ [跳格分仓] %s 分出 %.4f 到 %s，剩余 %.4f；将分别在 %s / %s 挂卖",
				formatPrice(parentPrice, spm.priceDecimals), share,
				formatPrice(childPrice, spm.priceDecimals), parent.PositionQty,
				formatPrice(roundPrice(parentPrice+interval, spm.priceDecimals), spm.priceDecimals),
				formatPrice(sellPrice, spm.priceDecimals))
		} else if child.stackedParentPrice == parentPrice && child.PositionQty <= fillQtyTolerance &&
			child.OrderID == 0 && child.ClientOID == "" {
			child.stackedParentPrice = 0
		}
		child.mu.Unlock()
	}

	if parent.PositionQty <= fillQtyTolerance {
		parent.PositionStatus = PositionStatusEmpty
		parent.PositionQty = 0
	}
	parent.gapStackCount = 1
	for _, childPrice := range children {
		raw, ok := spm.slots.Load(childPrice)
		if !ok {
			continue
		}
		child, _ := raw.(*InventorySlot)
		if child == nil {
			continue
		}
		child.mu.Lock()
		if child.stackedParentPrice == parentPrice && child.PositionQty <= fillQtyTolerance &&
			child.OrderID == 0 && child.ClientOID == "" {
			child.stackedParentPrice = 0
		}
		child.mu.Unlock()
	}
}
