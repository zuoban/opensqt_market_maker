package position

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"opensqt/exchange"
	"opensqt/logger"
)

const (
	buyCancelTimeout         = 10 * time.Second
	buyCancelConfirmInterval = 100 * time.Millisecond
)

type buyCancelTarget struct {
	price         float64
	orderID       int64
	clientOrderID string
	orderPrice    float64
	orderQuantity float64
	executedQty   float64
}

type openBuyEvidence struct {
	update   OrderUpdate
	quantity float64
}

// cancelAllBuyOrders 是可注入截止时间的实现，供门禁和确定性测试共享。
// GetOpenOrders 证明订单已不再挂单，GetOrder 再提供最终累计成交量；二者缺一
// 都不能安全释放槽位。
func (spm *SuperPositionManager) cancelAllBuyOrders(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("撤销买单上下文不能为空")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	targets := spm.markBuyOrdersCancelRequested()
	if len(targets) == 0 {
		return nil
	}
	if spm.executor == nil || spm.exchange == nil {
		return fmt.Errorf("撤销买单依赖未初始化")
	}

	pending := make(map[string]buyCancelTarget, len(targets))
	for _, target := range targets {
		pending[buyCancelTargetKey(target)] = target
	}
	logger.Info("🔄 [撤销买单] 准备撤销并确认 %d 个买单", len(pending))

	var lastCancelErr error
	var lastQueryErr error
	var lastDetailErr error
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return buyCancelConfirmationError(err, pending, lastCancelErr, lastQueryErr, lastDetailErr)
		}

		// 对无 OrderID 的 UNKNOWN reservation 先按原始 ClientOID 做保守恢复。
		// 找到活跃单后会补齐 OrderID；反复明确不存在时会安全释放槽位。
		if _, supported := spm.exchange.(clientOrderLookup); supported {
			if _, err := spm.ResolvePendingOrders(ctx); err != nil {
				lastDetailErr = fmt.Errorf("恢复待确认订单失败: %w", err)
			}
			spm.refreshPendingBuyTargets(pending)
		}

		orderIDs := knownPendingBuyOrderIDs(pending)
		if len(orderIDs) == 0 {
			lastCancelErr = nil
		} else if err := spm.executor.BatchCancelOrders(orderIDs); err != nil {
			lastCancelErr = fmt.Errorf("批量撤单失败: %w", err)
		} else {
			lastCancelErr = nil
		}

		openRaw, err := spm.exchange.GetOpenOrders(ctx, spm.config.Trading.Symbol)
		if err != nil {
			lastQueryErr = fmt.Errorf("查询远端挂单失败: %w", err)
		} else {
			openOrders, parseErr := spm.extractOpenBuyEvidence(openRaw, pending)
			if parseErr != nil {
				lastQueryErr = fmt.Errorf("解析远端挂单失败: %w", parseErr)
			} else {
				lastQueryErr = nil
				lastDetailErr = spm.convergeCanceledBuyTargets(ctx, pending, openOrders)
			}
		}

		if len(pending) == 0 {
			logger.Info("✅ [撤销买单] 远端撤单已确认，本地槽位已收敛")
			return nil
		}

		timer := time.NewTimer(buyCancelConfirmInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return buyCancelConfirmationError(ctx.Err(), pending, lastCancelErr, lastQueryErr, lastDetailErr)
		case <-timer.C:
		}
	}
	return nil
}

func (spm *SuperPositionManager) refreshPendingBuyTargets(pending map[string]buyCancelTarget) {
	for key, target := range pending {
		value, ok := spm.slots.Load(target.price)
		if !ok {
			continue
		}
		slot, _ := value.(*InventorySlot)
		if slot == nil {
			continue
		}
		slot.mu.RLock()
		matches := slot.ClientOID != "" && target.clientOrderID != "" &&
			spm.canonicalClientOrderID(slot.ClientOID) == spm.canonicalClientOrderID(target.clientOrderID) &&
			slot.OrderSide == "BUY"
		if matches {
			target.orderID = slot.OrderID
			target.orderPrice = slot.OrderPrice
			target.orderQuantity = slot.OrderQuantity
			target.executedQty = slot.OrderFilledQty
		}
		slot.mu.RUnlock()
		if matches {
			pending[key] = target
		}
	}
}

func (spm *SuperPositionManager) markBuyOrdersCancelRequested() []buyCancelTarget {
	targets := make([]buyCancelTarget, 0)
	spm.forEachSlot(func(price float64, slot *InventorySlot) bool {
		slot.mu.Lock()
		knownOrder := slot.OrderSide == "BUY" && slot.OrderID > 0
		unknownReservation := slot.OrderSide == "BUY" && slot.OrderID == 0 &&
			slot.ClientOID != "" && slot.SlotStatus == SlotStatusPending
		if knownOrder || unknownReservation {
			targets = append(targets, buyCancelTarget{
				price:         price,
				orderID:       slot.OrderID,
				clientOrderID: slot.ClientOID,
				orderPrice:    slot.OrderPrice,
				orderQuantity: slot.OrderQuantity,
				executedQty:   slot.OrderFilledQty,
			})
			slot.OrderStatus = OrderStatusCancelRequested
		}
		slot.mu.Unlock()
		return true
	})
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].price != targets[j].price {
			return targets[i].price < targets[j].price
		}
		return targets[i].clientOrderID < targets[j].clientOrderID
	})
	return targets
}

func buyCancelTargetKey(target buyCancelTarget) string {
	return fmt.Sprintf("%.12f|%s|%d", target.price, target.clientOrderID, target.orderID)
}

func knownPendingBuyOrderIDs(pending map[string]buyCancelTarget) []int64 {
	ids := make([]int64, 0, len(pending))
	seen := make(map[int64]struct{}, len(pending))
	for _, target := range pending {
		if target.orderID <= 0 {
			continue
		}
		if _, duplicate := seen[target.orderID]; duplicate {
			continue
		}
		seen[target.orderID] = struct{}{}
		ids = append(ids, target.orderID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sortedPendingBuyTargetKeys(pending map[string]buyCancelTarget) []string {
	keys := make([]string, 0, len(pending))
	for key := range pending {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func pendingBuyTargetLabels(pending map[string]buyCancelTarget) []string {
	labels := make([]string, 0, len(pending))
	for _, key := range sortedPendingBuyTargetKeys(pending) {
		target := pending[key]
		if target.orderID > 0 {
			labels = append(labels, fmt.Sprintf("OrderID=%d/ClientOID=%s", target.orderID, target.clientOrderID))
		} else {
			labels = append(labels, fmt.Sprintf("UNKNOWN/ClientOID=%s", target.clientOrderID))
		}
	}
	return labels
}

func buyCancelConfirmationError(
	cause error,
	pending map[string]buyCancelTarget,
	cancelErr error,
	queryErr error,
	detailErr error,
) error {
	return errors.Join(
		fmt.Errorf("撤销买单未能确认，本地保持关闭状态，待确认订单=%v: %w",
			pendingBuyTargetLabels(pending), cause),
		cancelErr,
		queryErr,
		detailErr,
	)
}

func (spm *SuperPositionManager) convergeCanceledBuyTargets(
	ctx context.Context,
	pending map[string]buyCancelTarget,
	openOrders map[string]openBuyEvidence,
) error {
	var errs []error
	for _, key := range sortedPendingBuyTargetKeys(pending) {
		target := pending[key]
		if evidence, stillOpen := openOrders[key]; stillOpen {
			if err := spm.validateOpenBuyEvidence(target, evidence); err != nil {
				errs = append(errs, fmt.Errorf("远端挂单 ClientOID=%s 无效: %w", target.clientOrderID, err))
				continue
			}
			localUpdate := evidence.update
			localUpdate.ClientOrderID = target.clientOrderID
			spm.OnOrderUpdate(localUpdate)
			target.orderID = evidence.update.OrderID
			target.executedQty = evidence.update.ExecutedQty
			pending[key] = target
			continue
		}
		if spm.buyCancelTargetConverged(target) {
			delete(pending, key)
			continue
		}
		if target.orderID <= 0 {
			errs = append(errs, fmt.Errorf(
				"UNKNOWN 买单 ClientOID=%s 未在远端挂单中找到，且无明确终态证据", target.clientOrderID))
			continue
		}

		raw, err := spm.exchange.GetOrder(ctx, spm.config.Trading.Symbol, target.orderID)
		if err != nil {
			errs = append(errs, fmt.Errorf("查询订单 %d 终态失败: %w", target.orderID, err))
			continue
		}
		update, quantity, err := spm.extractCanceledBuyUpdate(raw, target)
		if err != nil {
			errs = append(errs, fmt.Errorf("订单 %d 终态无效: %w", target.orderID, err))
			continue
		}
		if err := spm.validateCanceledBuyUpdate(target, update, quantity); err != nil {
			errs = append(errs, fmt.Errorf("订单 %d 终态校验失败: %w", target.orderID, err))
			continue
		}

		update.ClientOrderID = target.clientOrderID
		spm.OnOrderUpdate(update)
		if !spm.buyCancelTargetConverged(target) {
			errs = append(errs, fmt.Errorf("订单 %d 已确认终态但本地槽位未收敛", target.orderID))
			continue
		}
		delete(pending, key)
	}
	return errors.Join(errs...)
}

func (spm *SuperPositionManager) buyCancelTargetConverged(target buyCancelTarget) bool {
	if value, ok := spm.slots.Load(target.price); ok {
		if slot, ok := value.(*InventorySlot); ok && slot != nil {
			slot.mu.RLock()
			stillBound := (target.orderID > 0 && slot.OrderID == target.orderID) ||
				(target.clientOrderID != "" && slot.ClientOID != "" &&
					spm.canonicalClientOrderID(slot.ClientOID) == spm.canonicalClientOrderID(target.clientOrderID))
			slot.mu.RUnlock()
			if stillBound {
				return false
			}
		}
	}
	if converged, recorded := spm.resolvedAbsentOrderConverged(target.clientOrderID, time.Now()); recorded {
		return converged
	}
	identity := OrderUpdate{
		OrderID:       target.orderID,
		ClientOrderID: spm.canonicalClientOrderID(target.clientOrderID),
	}
	if _, ok := spm.getTerminalOrderProgress(identity); ok {
		return true
	}
	return spm.wasFilledOrderRecorded(identity)
}

func (spm *SuperPositionManager) extractCanceledBuyUpdate(raw interface{}, target buyCancelTarget) (OrderUpdate, float64, error) {
	if order, ok := asExchangeOrder(raw); ok {
		return exchangeOrderToUpdate(order), order.Quantity, nil
	}
	v := dereferenceCancelOrder(reflect.ValueOf(raw))
	if !v.IsValid() || v.Kind() != reflect.Struct {
		return OrderUpdate{}, 0, fmt.Errorf("订单响应类型无效: %T", raw)
	}

	orderID, ok := cancelInt64Field(v, "OrderID")
	if !ok || orderID <= 0 {
		return OrderUpdate{}, 0, fmt.Errorf("缺少有效 OrderID")
	}
	clientOrderID, ok := cancelStringField(v, "ClientOrderID")
	if !ok || clientOrderID == "" {
		return OrderUpdate{}, 0, fmt.Errorf("缺少 ClientOrderID")
	}
	side, ok := cancelStringField(v, "Side")
	if !ok || side == "" {
		return OrderUpdate{}, 0, fmt.Errorf("缺少 Side")
	}
	status, ok := cancelStringField(v, "Status")
	if !ok || status == "" {
		return OrderUpdate{}, 0, fmt.Errorf("缺少 Status")
	}
	executedQty, ok := cancelFloat64Field(v, "ExecutedQty")
	if !ok {
		return OrderUpdate{}, 0, fmt.Errorf("缺少 ExecutedQty")
	}
	quantity, ok := cancelFloat64Field(v, "Quantity")
	if !ok {
		return OrderUpdate{}, 0, fmt.Errorf("缺少 Quantity")
	}
	price, _ := cancelFloat64Field(v, "Price")
	avgPrice, _ := cancelFloat64Field(v, "AvgPrice")
	symbol, _ := cancelStringField(v, "Symbol")
	orderType, _ := cancelStringField(v, "Type")
	updateTime, _ := cancelInt64Field(v, "UpdateTime")

	return OrderUpdate{
		OrderID:       orderID,
		ClientOrderID: clientOrderID,
		Symbol:        symbol,
		Status:        strings.ToUpper(status),
		Quantity:      quantity,
		ExecutedQty:   executedQty,
		Price:         price,
		AvgPrice:      avgPrice,
		Side:          strings.ToUpper(side),
		Type:          orderType,
		UpdateTime:    updateTime,
	}, quantity, nil
}

func asExchangeOrder(raw interface{}) (*exchange.Order, bool) {
	switch order := raw.(type) {
	case *exchange.Order:
		return order, order != nil
	case exchange.Order:
		copied := order
		return &copied, true
	default:
		return nil, false
	}
}

func exchangeOrderToUpdate(order *exchange.Order) OrderUpdate {
	return OrderUpdate{
		OrderID:       order.OrderID,
		ClientOrderID: order.ClientOrderID,
		Symbol:        order.Symbol,
		Status:        strings.ToUpper(string(order.Status)),
		Quantity:      order.Quantity,
		ExecutedQty:   order.ExecutedQty,
		Price:         order.Price,
		AvgPrice:      order.AvgPrice,
		Side:          strings.ToUpper(string(order.Side)),
		Type:          string(order.Type),
		UpdateTime:    order.UpdateTime,
	}
}

func typedOpenOrders(raw interface{}) ([]*exchange.Order, bool) {
	switch orders := raw.(type) {
	case []*exchange.Order:
		return orders, true
	case []exchange.Order:
		out := make([]*exchange.Order, len(orders))
		for i := range orders {
			item := orders[i]
			out[i] = &item
		}
		return out, true
	default:
		return nil, false
	}
}

func (spm *SuperPositionManager) collectOpenBuyEvidence(
	orders []*exchange.Order,
	pending map[string]buyCancelTarget,
) (map[string]openBuyEvidence, error) {
	result := make(map[string]openBuyEvidence)
	for i, order := range orders {
		if order == nil {
			return nil, fmt.Errorf("第 %d 个挂单为空", i)
		}
		matchedKey := ""
		for key, target := range pending {
			matchesID := target.orderID > 0 && target.orderID == order.OrderID
			matchesClientID := target.orderID == 0 &&
				spm.canceledBuyClientIDMatches(target.clientOrderID, order.ClientOrderID)
			if !matchesID && !matchesClientID {
				continue
			}
			if matchedKey != "" {
				return nil, fmt.Errorf("远端订单 %d 同时匹配多个本地买单 reservation", order.OrderID)
			}
			matchedKey = key
		}
		if matchedKey == "" {
			continue
		}
		if _, duplicate := result[matchedKey]; duplicate {
			return nil, fmt.Errorf("本地 reservation %s 匹配多个远端订单", matchedKey)
		}
		result[matchedKey] = openBuyEvidence{
			update:   exchangeOrderToUpdate(order),
			quantity: order.Quantity,
		}
	}
	return result, nil
}

func (spm *SuperPositionManager) extractOpenBuyEvidence(
	raw interface{},
	pending map[string]buyCancelTarget,
) (map[string]openBuyEvidence, error) {
	result := make(map[string]openBuyEvidence)
	if orders, ok := typedOpenOrders(raw); ok {
		return spm.collectOpenBuyEvidence(orders, pending)
	}
	v := dereferenceCancelOrder(reflect.ValueOf(raw))
	if !v.IsValid() {
		return result, nil
	}
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return nil, fmt.Errorf("挂单响应类型不是切片: %T", raw)
	}
	for i := 0; i < v.Len(); i++ {
		item := dereferenceCancelOrder(v.Index(i))
		if !item.IsValid() || item.Kind() != reflect.Struct {
			return nil, fmt.Errorf("第 %d 个挂单类型无效", i)
		}
		orderID, ok := cancelInt64Field(item, "OrderID")
		if !ok || orderID <= 0 {
			return nil, fmt.Errorf("第 %d 个挂单缺少有效 OrderID", i)
		}
		clientOrderID, _ := cancelStringField(item, "ClientOrderID")

		matchedKey := ""
		for key, target := range pending {
			matchesID := target.orderID > 0 && target.orderID == orderID
			matchesClientID := target.orderID == 0 &&
				spm.canceledBuyClientIDMatches(target.clientOrderID, clientOrderID)
			if !matchesID && !matchesClientID {
				continue
			}
			if matchedKey != "" {
				return nil, fmt.Errorf("远端订单 %d 同时匹配多个本地买单 reservation", orderID)
			}
			matchedKey = key
		}
		if matchedKey == "" {
			continue
		}
		if _, duplicate := result[matchedKey]; duplicate {
			return nil, fmt.Errorf("本地 reservation %s 匹配多个远端订单", matchedKey)
		}
		target := pending[matchedKey]
		update, quantity, err := spm.extractCanceledBuyUpdate(item.Interface(), target)
		if err != nil {
			return nil, fmt.Errorf("解析远端挂单 %d 失败: %w", orderID, err)
		}
		result[matchedKey] = openBuyEvidence{update: update, quantity: quantity}
	}
	return result, nil
}

func (spm *SuperPositionManager) canceledBuyClientIDMatches(local, remote string) bool {
	if local == "" || remote == "" {
		return local == remote
	}
	return spm.canonicalClientOrderID(local) == spm.canonicalClientOrderID(remote)
}

func (spm *SuperPositionManager) validateOpenBuyEvidence(target buyCancelTarget, evidence openBuyEvidence) error {
	update := evidence.update
	if target.orderID > 0 && update.OrderID != target.orderID {
		return fmt.Errorf("OrderID=%d，本地=%d", update.OrderID, target.orderID)
	}
	if update.OrderID <= 0 {
		return fmt.Errorf("OrderID=%d 非法", update.OrderID)
	}
	if !spm.canceledBuyClientIDMatches(target.clientOrderID, update.ClientOrderID) {
		return fmt.Errorf("ClientOrderID=%q，本地=%q", update.ClientOrderID, target.clientOrderID)
	}
	if update.Side != "BUY" {
		return fmt.Errorf("Side=%s，期望 BUY", update.Side)
	}
	if update.Symbol != "" && update.Symbol != spm.config.Trading.Symbol {
		return fmt.Errorf("Symbol=%s，期望 %s", update.Symbol, spm.config.Trading.Symbol)
	}
	if math.IsNaN(evidence.quantity) || math.IsInf(evidence.quantity, 0) || evidence.quantity <= 0 {
		return fmt.Errorf("Quantity=%v 非法", evidence.quantity)
	}
	quantityTolerance := math.Max(fillQtyTolerance, math.Max(math.Abs(evidence.quantity), math.Abs(target.orderQuantity))*1e-8)
	if target.orderQuantity > 0 && math.Abs(evidence.quantity-target.orderQuantity) > quantityTolerance {
		return fmt.Errorf("Quantity=%.12f，本地=%.12f", evidence.quantity, target.orderQuantity)
	}
	if math.IsNaN(update.ExecutedQty) || math.IsInf(update.ExecutedQty, 0) || update.ExecutedQty < 0 {
		return fmt.Errorf("ExecutedQty=%v 非法", update.ExecutedQty)
	}
	if update.ExecutedQty+fillQtyTolerance < target.executedQty || update.ExecutedQty > evidence.quantity+quantityTolerance {
		return fmt.Errorf("ExecutedQty=%.12f 超出本地累计/订单数量 [%.12f, %.12f]",
			update.ExecutedQty, target.executedQty, evidence.quantity)
	}
	if target.orderPrice > 0 && update.Price > 0 {
		priceTolerance := math.Max(fillQtyTolerance, math.Max(math.Abs(target.orderPrice), math.Abs(update.Price))*1e-8)
		if math.Abs(target.orderPrice-update.Price) > priceTolerance {
			return fmt.Errorf("Price=%.12f，本地=%.12f", update.Price, target.orderPrice)
		}
	}
	switch update.Status {
	case "NEW":
		if update.ExecutedQty > quantityTolerance {
			return fmt.Errorf("Status=NEW 但 ExecutedQty=%.12f", update.ExecutedQty)
		}
	case OrderStatusPartiallyFilled:
		if update.ExecutedQty <= quantityTolerance || update.ExecutedQty >= evidence.quantity-quantityTolerance {
			return fmt.Errorf("PARTIALLY_FILLED 与 ExecutedQty=%.12f/Quantity=%.12f 不一致",
				update.ExecutedQty, evidence.quantity)
		}
	default:
		return fmt.Errorf("Status=%s 不是远端挂单状态", update.Status)
	}
	return nil
}

func (spm *SuperPositionManager) validateCanceledBuyUpdate(target buyCancelTarget, update OrderUpdate, quantity float64) error {
	if update.OrderID != target.orderID {
		return fmt.Errorf("OrderID=%d，本地=%d", update.OrderID, target.orderID)
	}
	if !spm.canceledBuyClientIDMatches(target.clientOrderID, update.ClientOrderID) {
		return fmt.Errorf("ClientOrderID=%q，本地=%q", update.ClientOrderID, target.clientOrderID)
	}
	if update.Side != "BUY" {
		return fmt.Errorf("Side=%s，期望 BUY", update.Side)
	}
	if update.Symbol != "" && update.Symbol != spm.config.Trading.Symbol {
		return fmt.Errorf("Symbol=%s，期望 %s", update.Symbol, spm.config.Trading.Symbol)
	}
	switch update.Status {
	case OrderStatusFilled, OrderStatusCanceled, "EXPIRED", "REJECTED":
	default:
		return fmt.Errorf("Status=%s 不是可收敛终态", update.Status)
	}
	if math.IsNaN(update.ExecutedQty) || math.IsInf(update.ExecutedQty, 0) || update.ExecutedQty < 0 {
		return fmt.Errorf("ExecutedQty=%v 非法", update.ExecutedQty)
	}
	if math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity <= 0 {
		return fmt.Errorf("Quantity=%v 非法", quantity)
	}
	tolerance := math.Max(fillQtyTolerance, math.Max(math.Abs(quantity), math.Abs(update.ExecutedQty))*1e-8)
	if update.ExecutedQty+fillQtyTolerance < target.executedQty {
		return fmt.Errorf("ExecutedQty=%.12f 小于本地累计 %.12f", update.ExecutedQty, target.executedQty)
	}
	if update.ExecutedQty > quantity+tolerance {
		return fmt.Errorf("ExecutedQty=%.12f 大于 Quantity=%.12f", update.ExecutedQty, quantity)
	}
	if target.orderQuantity > 0 && math.Abs(target.orderQuantity-quantity) > tolerance {
		return fmt.Errorf("Quantity=%.12f，本地=%.12f", quantity, target.orderQuantity)
	}
	if target.orderPrice > 0 && update.Price > 0 {
		priceTolerance := math.Max(fillQtyTolerance, math.Max(math.Abs(target.orderPrice), math.Abs(update.Price))*1e-8)
		if math.Abs(target.orderPrice-update.Price) > priceTolerance {
			return fmt.Errorf("Price=%.12f，本地=%.12f", update.Price, target.orderPrice)
		}
	}
	if update.Status == OrderStatusFilled && math.Abs(update.ExecutedQty-quantity) > tolerance {
		return fmt.Errorf("FILLED 的 ExecutedQty=%.12f 与 Quantity=%.12f 不一致", update.ExecutedQty, quantity)
	}
	return nil
}

func dereferenceCancelOrder(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func cancelStringField(v reflect.Value, name string) (string, bool) {
	field := v.FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.String {
		return "", false
	}
	return field.String(), true
}

func cancelFloat64Field(v reflect.Value, name string) (float64, bool) {
	field := v.FieldByName(name)
	if !field.IsValid() || !field.CanFloat() {
		return 0, false
	}
	return field.Float(), true
}

func cancelInt64Field(v reflect.Value, name string) (int64, bool) {
	field := v.FieldByName(name)
	if !field.IsValid() || !field.CanInt() {
		return 0, false
	}
	return field.Int(), true
}
