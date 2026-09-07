package safety

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opensqt/config"
	"opensqt/exchange"
	"opensqt/logger"
)

// IExchange 定义对账所需的交易所接口方法
type IExchange interface {
	GetPositions(ctx context.Context, symbol string) (interface{}, error)
	GetOpenOrders(ctx context.Context, symbol string) (interface{}, error)
	GetBaseAsset() string // 获取基础资产（交易币种）
}

// SlotInfo 槽位信息（避免直接依赖 position 包的内部结构）
type SlotInfo struct {
	Price          float64
	PositionStatus string
	PositionQty    float64
	OrderID        int64
	ClientOID      string
	OrderSide      string
	OrderStatus    string
	OrderPrice     float64
	OrderQuantity  float64
	OrderFilledQty float64
	SlotStatus     string
	OrderCreatedAt time.Time
}

type managedOrderSnapshot struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Status        string
	Type          string
	Price         float64
	Quantity      float64
	ExecutedQty   float64
}

// IPositionManager 定义对账所需的仓位管理器接口方法
type IPositionManager interface {
	// 遍历所有槽位。回调收到只读拷贝。
	IterateSlots(fn func(price float64, slot SlotInfo) bool)
	// 获取统计数据
	GetTotalBuyQty() float64
	GetTotalSellQty() float64
	GetReconcileCount() int64
	// 更新统计数据
	IncrementReconcileCount()
	UpdateLastReconcileTime(t time.Time)
	// 获取配置信息
	GetSymbol() string
	GetPriceInterval() float64
}

// Reconciler 持仓对账器
type Reconciler struct {
	cfg          *config.Config
	exchange     IExchange
	pm           IPositionManager
	pauseChecker func() bool
	healthy      atomic.Bool
	healthMu     sync.RWMutex
	healthChange func(healthy bool, err error)
	reconcileMu  sync.Mutex
}

type pendingOrderResolver interface {
	ResolvePendingOrders(ctx context.Context) (remaining int, err error)
}

// NewReconciler 创建对账器
func NewReconciler(cfg *config.Config, exchange IExchange, pm IPositionManager) *Reconciler {
	return &Reconciler{
		cfg:      cfg,
		exchange: exchange,
		pm:       pm,
	}
}

// SetPauseChecker 设置暂停检查函数（用于风控暂停）
func (r *Reconciler) SetPauseChecker(checker func() bool) {
	r.pauseChecker = checker
}

// SetHealthHandler 注册对账健康状态变化回调。回调不得阻塞。
func (r *Reconciler) SetHealthHandler(handler func(healthy bool, err error)) {
	r.healthMu.Lock()
	r.healthChange = handler
	r.healthMu.Unlock()
}

// IsHealthy 返回最近一次完整对账是否通过。首次对账前固定为 false。
func (r *Reconciler) IsHealthy() bool {
	return r.healthy.Load()
}

// Invalidate 在订单流断线等可能丢事件的场景立即使对账失效。
// 恢复交易前必须重新执行一次完整 Reconcile。
func (r *Reconciler) Invalidate(err error) {
	if err == nil {
		err = fmt.Errorf("对账状态已失效")
	}
	r.setHealthy(false, err)
}

func (r *Reconciler) setHealthy(healthy bool, err error) {
	changed := r.healthy.Swap(healthy) != healthy
	if !changed && err == nil {
		return
	}
	r.healthMu.RLock()
	handler := r.healthChange
	r.healthMu.RUnlock()
	if handler != nil {
		handler(healthy, err)
	}
}

// Start 启动对账协程
func (r *Reconciler) Start(ctx context.Context) {
	go func() {
		interval := time.Duration(r.cfg.Trading.ReconcileInterval) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				logger.Info("⏹️ 持仓对账协程已停止")
				return
			case <-ticker.C:
				if err := r.Reconcile(); err != nil {
					logger.Error("❌ [对账失败] %v", err)
				}
			}
		}
	}()
	logger.Info("✅ 持仓对账已启动 (间隔: %d秒)", r.cfg.Trading.ReconcileInterval)
}

// Reconcile 执行对账（通用实现，支持所有交易所）
func (r *Reconciler) Reconcile() (retErr error) {
	r.reconcileMu.Lock()
	defer r.reconcileMu.Unlock()
	defer func() {
		if retErr != nil {
			r.setHealthy(false, retErr)
		}
	}()

	// 风控期间仍必须继续核对真实订单；仅降低普通日志噪声。
	quiet := r.pauseChecker != nil && r.pauseChecker()

	if !quiet {
		logger.Debugln("🔍 ===== 开始持仓对账 =====")
	}

	symbol := r.pm.GetSymbol()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 先用 ClientOrderID 收敛下单结果不确定的 reservation。只有交易所反复
	// 明确返回“不存在”时 position 层才会释放；未解决时仍保持 fail-closed。
	if resolver, ok := r.pm.(pendingOrderResolver); ok {
		remaining, err := resolver.ResolvePendingOrders(ctx)
		if err != nil {
			return fmt.Errorf("恢复待确认订单失败: %w", err)
		}
		if remaining > 0 {
			return fmt.Errorf("本地有 %d 个槽位正在提交订单，无法完成权威对账", remaining)
		}
	}

	// 1. 查询交易所持仓信息（使用通用接口）
	positionsRaw, err := r.exchange.GetPositions(ctx, symbol)
	if err != nil {
		return fmt.Errorf("查询持仓失败: %w", err)
	}

	// 2. 查询所有挂单（使用通用接口）
	openOrdersRaw, err := r.exchange.GetOpenOrders(ctx, symbol)
	if err != nil {
		return fmt.Errorf("查询挂单失败: %w", err)
	}

	// 3. 解析持仓和挂单信息（通用处理）
	logger.Debug("📊 交易所持仓信息类型: %T", positionsRaw)
	logger.Debug("📊 交易所挂单信息类型: %T", openOrdersRaw)

	// 4. 计算本地持仓统计
	var localTotal float64
	var localPendingSellQty float64
	var activeBuyOrders int
	var activeSellOrders int
	localOrders := make(map[int64]managedOrderSnapshot)
	pendingOperations := 0
	var localStateErr error

	// 订单状态常量（与 position 包保持一致）
	const (
		OrderStatusPlaced          = "PLACED"
		OrderStatusConfirmed       = "CONFIRMED"
		OrderStatusPartiallyFilled = "PARTIALLY_FILLED"
		OrderStatusCancelRequested = "CANCEL_REQUESTED"
		PositionStatusFilled       = "FILLED"
	)

	r.pm.IterateSlots(func(price float64, slot SlotInfo) bool {
		positionStatus := slot.PositionStatus
		positionQty := slot.PositionQty
		orderSide := slot.OrderSide
		orderStatus := slot.OrderStatus
		slotStatus := slot.SlotStatus
		orderID := slot.OrderID
		clientOrderID := slot.ClientOID
		orderPrice := slot.OrderPrice
		orderQuantity := slot.OrderQuantity
		orderFilledQty := slot.OrderFilledQty

		if math.IsNaN(positionQty) || math.IsInf(positionQty, 0) || positionQty < 0 {
			localStateErr = fmt.Errorf("槽位 %.12f 的 PositionQty=%v 非法", price, positionQty)
			return false
		}

		// 部分成交的买单在 PositionStatus 仍可能是 EMPTY，但真实持仓已经增加。
		// 对账必须统计所有正的槽位数量，而不是只统计 FILLED 状态。
		if positionQty > 0 {
			localTotal += positionQty
			if math.IsNaN(localTotal) || math.IsInf(localTotal, 0) {
				localStateErr = fmt.Errorf("本地持仓合计溢出: %v", localTotal)
				return false
			}
		}
		if slotStatus == "PENDING" {
			pendingOperations++
		}

		if positionStatus == PositionStatusFilled {
			if orderSide == "SELL" && (orderStatus == OrderStatusPlaced || orderStatus == OrderStatusConfirmed ||
				orderStatus == OrderStatusPartiallyFilled || orderStatus == OrderStatusCancelRequested) {
				localPendingSellQty += positionQty
				activeSellOrders++
			}
		}

		if orderSide == "BUY" && (orderStatus == OrderStatusPlaced || orderStatus == OrderStatusConfirmed ||
			orderStatus == OrderStatusPartiallyFilled) {
			activeBuyOrders++
		}

		activeOrder := isLocallyActiveOrder(orderStatus)
		if activeOrder {
			if slotStatus != "LOCKED" {
				localStateErr = fmt.Errorf("订单 %d 处于活跃状态 %s，但槽位状态=%s（应为 LOCKED）",
					orderID, orderStatus, slotStatus)
				return false
			}
			if orderStatus == OrderStatusCancelRequested {
				localStateErr = fmt.Errorf("订单 %d 仍处于 CANCEL_REQUESTED，终态尚未确认", orderID)
				return false
			}
			if orderID <= 0 {
				localStateErr = fmt.Errorf("槽位 %.12f 的活跃订单缺少有效 OrderID", price)
				return false
			}
			orderSide = strings.ToUpper(orderSide)
			if orderSide != "BUY" && orderSide != "SELL" {
				localStateErr = fmt.Errorf("订单 %d 的 Side=%q 非法", orderID, orderSide)
				return false
			}
			if clientOrderID == "" {
				localStateErr = fmt.Errorf("订单 %d 缺少 ClientOID", orderID)
				return false
			}
			if !isOpenSQTClientOrderID(clientOrderID) {
				localStateErr = fmt.Errorf("订单 %d 的 ClientOID=%q 不是受管订单格式", orderID, clientOrderID)
				return false
			}
			if math.IsNaN(orderPrice) || math.IsInf(orderPrice, 0) || orderPrice <= 0 {
				localStateErr = fmt.Errorf("订单 %d 的 OrderPrice=%v 非法", orderID, orderPrice)
				return false
			}
			if math.IsNaN(orderFilledQty) || math.IsInf(orderFilledQty, 0) || orderFilledQty < 0 {
				localStateErr = fmt.Errorf("订单 %d 的 OrderFilledQty=%v 非法", orderID, orderFilledQty)
				return false
			}
			if math.IsNaN(orderQuantity) || math.IsInf(orderQuantity, 0) || orderQuantity <= 0 {
				localStateErr = fmt.Errorf("订单 %d 的 OrderQuantity=%v 非法", orderID, orderQuantity)
				return false
			}
			if orderFilledQty > orderQuantity+orderQtyTolerance(orderFilledQty, orderQuantity) {
				localStateErr = fmt.Errorf("订单 %d 的 OrderFilledQty=%.12f 大于 OrderQuantity=%.12f",
					orderID, orderFilledQty, orderQuantity)
				return false
			}
			switch orderStatus {
			case OrderStatusPlaced, OrderStatusConfirmed:
				if orderFilledQty > orderQtyTolerance(orderFilledQty, orderQuantity) {
					localStateErr = fmt.Errorf("订单 %d 状态=%s 但 OrderFilledQty=%.12f",
						orderID, orderStatus, orderFilledQty)
					return false
				}
			case OrderStatusPartiallyFilled:
				tolerance := orderQtyTolerance(orderFilledQty, orderQuantity)
				if orderFilledQty <= tolerance || orderFilledQty >= orderQuantity-tolerance {
					localStateErr = fmt.Errorf("订单 %d PARTIALLY_FILLED 与 %.12f/%.12f 不一致",
						orderID, orderFilledQty, orderQuantity)
					return false
				}
			}

			// 活跃单必须与单槽库存严格对应。BUY 只能占用原本空仓的
			// 槽位，其当前库存等于本单累计成交；SELL 的剩余库存等于
			// 委托量减本单累计成交。否则即使远端净持仓和挂单 ID 都能对上，
			// 也可能是旧终态修正与新单共存，必须 fail-closed。
			inventoryTolerance := orderQtyTolerance(positionQty, orderQuantity)
			switch orderSide {
			case "BUY":
				if positionStatus != "EMPTY" || math.Abs(positionQty-orderFilledQty) > inventoryTolerance {
					localStateErr = fmt.Errorf(
						"BUY 订单 %d 与槽位库存不一致: PositionStatus=%s PositionQty=%.12f OrderFilledQty=%.12f",
						orderID, positionStatus, positionQty, orderFilledQty)
					return false
				}
			case "SELL":
				remaining := orderQuantity - orderFilledQty
				if positionStatus != PositionStatusFilled || remaining <= inventoryTolerance ||
					math.Abs(positionQty-remaining) > inventoryTolerance {
					localStateErr = fmt.Errorf(
						"SELL 订单 %d 与槽位库存不一致: PositionStatus=%s PositionQty=%.12f RemainingQty=%.12f",
						orderID, positionStatus, positionQty, remaining)
					return false
				}
			}
			if _, duplicate := localOrders[orderID]; duplicate {
				localStateErr = fmt.Errorf("本地存在重复 OrderID=%d", orderID)
				return false
			}
			localOrders[orderID] = managedOrderSnapshot{
				OrderID:       orderID,
				ClientOrderID: clientOrderID,
				Symbol:        symbol,
				Side:          orderSide,
				Status:        orderStatus,
				Price:         orderPrice,
				Quantity:      orderQuantity,
				ExecutedQty:   orderFilledQty,
			}
		}
		if slotStatus == "LOCKED" && !activeOrder {
			localStateErr = fmt.Errorf("槽位 %.12f 处于 LOCKED，但订单状态=%s 不是活跃状态", price, orderStatus)
			return false
		}

		return true
	})
	if localStateErr != nil {
		return fmt.Errorf("本地槽位状态不确定: %w", localStateErr)
	}

	// 正常下单提交期间远端与本地存在极短的确认窗口，延后本轮对账，避免误报。
	if pendingOperations > 0 {
		return fmt.Errorf("本地有 %d 个槽位正在提交订单，无法完成权威对账", pendingOperations)
	}

	remotePosition, err := extractRemotePosition(positionsRaw, symbol)
	if err != nil {
		return fmt.Errorf("解析远端持仓失败: %w", err)
	}
	remoteOrders, err := extractManagedRemoteOrders(openOrdersRaw, localOrders)
	if err != nil {
		return fmt.Errorf("解析远端挂单失败: %w", err)
	}

	qtyTolerance := math.Max(1e-9, math.Abs(remotePosition)*1e-8)
	if math.Abs(remotePosition-localTotal) > qtyTolerance {
		return fmt.Errorf("持仓不一致: Binance=%.12f, 本地=%.12f", remotePosition, localTotal)
	}
	if missing, extra := compareOrderIDSets(localOrders, remoteOrders); len(missing) > 0 || len(extra) > 0 {
		return fmt.Errorf("挂单不一致: Binance缺少本地订单=%v, Binance存在孤儿订单=%v", missing, extra)
	}
	if err := compareManagedOrderDetails(localOrders, remoteOrders); err != nil {
		return fmt.Errorf("挂单详情不一致: %w", err)
	}

	logger.Debug("📊 [对账统计] 本地持仓: %.4f, 挂单卖单: %d 个 (%.4f), 挂单买单: %d 个",
		localTotal, activeSellOrders, localPendingSellQty, activeBuyOrders)

	r.pm.IncrementReconcileCount()

	// 5. 输出对账统计（从交易所接口获取基础币种，支持U本位和币本位合约）
	baseCurrency := r.exchange.GetBaseAsset()
	if !quiet {
		logger.Info("✅ [对账完成] 远端/本地持仓: %.4f %s, 挂单卖单: %d 个 (%.4f), 挂单买单: %d 个",
			localTotal, baseCurrency, activeSellOrders, localPendingSellQty, activeBuyOrders)
	}

	r.pm.UpdateLastReconcileTime(time.Now())

	totalBuyQty := r.pm.GetTotalBuyQty()
	totalSellQty := r.pm.GetTotalSellQty()
	priceInterval := r.pm.GetPriceInterval()
	estimatedProfit := totalSellQty * priceInterval
	if !quiet {
		logger.Info("📊 [统计] 对账次数: %d, 累计买入: %.2f, 累计卖出: %.2f, 预计盈利: %.2f U",
			r.pm.GetReconcileCount(), totalBuyQty, totalSellQty, estimatedProfit)
		logger.Debugln("🔍 ===== 对账完成 =====")
	}
	r.setHealthy(true, nil)
	return nil
}

func sumTypedPositions(raw interface{}, symbol string) (float64, bool, error) {
	add := func(itemSymbol string, size float64) (float64, error) {
		if itemSymbol != symbol {
			return 0, nil
		}
		if math.IsNaN(size) || math.IsInf(size, 0) {
			return 0, fmt.Errorf("持仓 Size=%v 非法", size)
		}
		return size, nil
	}
	var total float64
	switch positions := raw.(type) {
	case []*exchange.Position:
		for i, pos := range positions {
			if pos == nil {
				continue
			}
			size, err := add(pos.Symbol, pos.Size)
			if err != nil {
				return 0, true, fmt.Errorf("第 %d 个持仓无效: %w", i, err)
			}
			total += size
		}
		return total, true, nil
	case []exchange.Position:
		for i, pos := range positions {
			size, err := add(pos.Symbol, pos.Size)
			if err != nil {
				return 0, true, fmt.Errorf("第 %d 个持仓无效: %w", i, err)
			}
			total += size
		}
		return total, true, nil
	default:
		return 0, false, nil
	}
}

type remoteOrderFields struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Status        string
	Type          string
	Price         float64
	Quantity      float64
	ExecutedQty   float64
}

func typedOrderSlice(raw interface{}) ([]remoteOrderFields, bool) {
	switch orders := raw.(type) {
	case []*exchange.Order:
		out := make([]remoteOrderFields, 0, len(orders))
		for _, order := range orders {
			if order == nil {
				continue
			}
			out = append(out, remoteOrderFields{
				OrderID:       order.OrderID,
				ClientOrderID: order.ClientOrderID,
				Symbol:        order.Symbol,
				Side:          string(order.Side),
				Status:        string(order.Status),
				Type:          string(order.Type),
				Price:         order.Price,
				Quantity:      order.Quantity,
				ExecutedQty:   order.ExecutedQty,
			})
		}
		return out, true
	case []exchange.Order:
		out := make([]remoteOrderFields, 0, len(orders))
		for _, order := range orders {
			out = append(out, remoteOrderFields{
				OrderID:       order.OrderID,
				ClientOrderID: order.ClientOrderID,
				Symbol:        order.Symbol,
				Side:          string(order.Side),
				Status:        string(order.Status),
				Type:          string(order.Type),
				Price:         order.Price,
				Quantity:      order.Quantity,
				ExecutedQty:   order.ExecutedQty,
			})
		}
		return out, true
	default:
		return nil, false
	}
}

func collectManagedRemoteOrders(
	orders []remoteOrderFields,
	localOrders map[int64]managedOrderSnapshot,
) (map[int64]managedOrderSnapshot, error) {
	result := make(map[int64]managedOrderSnapshot)
	for i, item := range orders {
		if item.OrderID <= 0 {
			return nil, fmt.Errorf("第 %d 个挂单缺少有效 OrderID", i)
		}
		_, isLocal := localOrders[item.OrderID]
		if !isLocal && !isOpenSQTClientOrderID(item.ClientOrderID) {
			continue
		}
		if _, duplicate := result[item.OrderID]; duplicate {
			return nil, fmt.Errorf("远端存在重复 OrderID=%d", item.OrderID)
		}
		remote := managedOrderSnapshot{
			OrderID:       item.OrderID,
			ClientOrderID: item.ClientOrderID,
			Symbol:        item.Symbol,
			Side:          strings.ToUpper(item.Side),
			Status:        strings.ToUpper(item.Status),
			Type:          strings.ToUpper(item.Type),
			Price:         item.Price,
			Quantity:      item.Quantity,
			ExecutedQty:   item.ExecutedQty,
		}
		if err := validateRemoteManagedOrder(remote); err != nil {
			return nil, fmt.Errorf("受管订单 %d 无效: %w", item.OrderID, err)
		}
		result[item.OrderID] = remote
	}
	return result, nil
}

func getInt64Field(v reflect.Value, name string) int64 {
	field := v.FieldByName(name)
	if field.IsValid() && field.CanInt() {
		return field.Int()
	}
	return 0
}

func isLocallyActiveOrder(status string) bool {
	switch status {
	case "PLACED", "CONFIRMED", "PARTIALLY_FILLED", "CANCEL_REQUESTED":
		return true
	default:
		return false
	}
}

func dereference(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}

func extractRemotePosition(raw interface{}, symbol string) (float64, error) {
	if total, ok, err := sumTypedPositions(raw, symbol); ok {
		return total, err
	}
	v := dereference(reflect.ValueOf(raw))
	if !v.IsValid() {
		return 0, nil
	}
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return 0, fmt.Errorf("持仓响应类型不是切片: %T", raw)
	}
	var total float64
	for i := 0; i < v.Len(); i++ {
		item := dereference(v.Index(i))
		if !item.IsValid() || item.Kind() != reflect.Struct {
			return 0, fmt.Errorf("第 %d 个持仓类型无效", i)
		}
		symbolField := item.FieldByName("Symbol")
		sizeField := item.FieldByName("Size")
		if !symbolField.IsValid() || symbolField.Kind() != reflect.String || !sizeField.IsValid() || !sizeField.CanFloat() {
			return 0, fmt.Errorf("第 %d 个持仓缺少 Symbol/Size", i)
		}
		if symbolField.String() == symbol {
			size := sizeField.Float()
			if math.IsNaN(size) || math.IsInf(size, 0) {
				return 0, fmt.Errorf("第 %d 个持仓 Size=%v 非法", i, size)
			}
			total += size
			if math.IsNaN(total) || math.IsInf(total, 0) {
				return 0, fmt.Errorf("持仓合计溢出: %v", total)
			}
		}
	}
	return total, nil
}

func extractManagedRemoteOrders(
	raw interface{},
	localOrders map[int64]managedOrderSnapshot,
) (map[int64]managedOrderSnapshot, error) {
	if orders, ok := typedOrderSlice(raw); ok {
		return collectManagedRemoteOrders(orders, localOrders)
	}
	v := dereference(reflect.ValueOf(raw))
	result := make(map[int64]managedOrderSnapshot)
	if !v.IsValid() {
		return result, nil
	}
	if v.Kind() != reflect.Slice && v.Kind() != reflect.Array {
		return nil, fmt.Errorf("挂单响应类型不是切片: %T", raw)
	}
	for i := 0; i < v.Len(); i++ {
		item := dereference(v.Index(i))
		if !item.IsValid() || item.Kind() != reflect.Struct {
			return nil, fmt.Errorf("第 %d 个挂单类型无效", i)
		}
		orderID := getInt64Field(item, "OrderID")
		if orderID <= 0 {
			return nil, fmt.Errorf("第 %d 个挂单缺少有效 OrderID", i)
		}
		clientID, _ := getStringReflectField(item, "ClientOrderID")
		_, isLocal := localOrders[orderID]
		if !isLocal && !isOpenSQTClientOrderID(clientID) {
			continue
		}
		if _, duplicate := result[orderID]; duplicate {
			return nil, fmt.Errorf("远端存在重复 OrderID=%d", orderID)
		}

		side, ok := getStringReflectField(item, "Side")
		if !ok || side == "" {
			return nil, fmt.Errorf("受管订单 %d 缺少 Side", orderID)
		}
		status, ok := getStringReflectField(item, "Status")
		if !ok || status == "" {
			return nil, fmt.Errorf("受管订单 %d 缺少 Status", orderID)
		}
		price, ok := getFloatReflectField(item, "Price")
		if !ok {
			return nil, fmt.Errorf("受管订单 %d 缺少 Price", orderID)
		}
		quantity, ok := getFloatReflectField(item, "Quantity")
		if !ok {
			return nil, fmt.Errorf("受管订单 %d 缺少 Quantity", orderID)
		}
		executedQty, ok := getFloatReflectField(item, "ExecutedQty")
		if !ok {
			return nil, fmt.Errorf("受管订单 %d 缺少 ExecutedQty", orderID)
		}
		symbol, ok := getStringReflectField(item, "Symbol")
		if !ok || symbol == "" {
			return nil, fmt.Errorf("受管订单 %d 缺少 Symbol", orderID)
		}
		orderType, ok := getStringReflectField(item, "Type")
		if !ok || orderType == "" {
			return nil, fmt.Errorf("受管订单 %d 缺少 Type", orderID)
		}
		remote := managedOrderSnapshot{
			OrderID:       orderID,
			ClientOrderID: clientID,
			Symbol:        symbol,
			Side:          strings.ToUpper(side),
			Status:        strings.ToUpper(status),
			Type:          strings.ToUpper(orderType),
			Price:         price,
			Quantity:      quantity,
			ExecutedQty:   executedQty,
		}
		if err := validateRemoteManagedOrder(remote); err != nil {
			return nil, fmt.Errorf("受管订单 %d 无效: %w", orderID, err)
		}
		result[orderID] = remote
	}
	return result, nil
}

func getStringReflectField(v reflect.Value, name string) (string, bool) {
	field := v.FieldByName(name)
	if !field.IsValid() || field.Kind() != reflect.String {
		return "", false
	}
	return field.String(), true
}

func getFloatReflectField(v reflect.Value, name string) (float64, bool) {
	field := v.FieldByName(name)
	if !field.IsValid() || !field.CanFloat() {
		return 0, false
	}
	return field.Float(), true
}

func validateRemoteManagedOrder(order managedOrderSnapshot) error {
	if order.ClientOrderID == "" {
		return fmt.Errorf("缺少 ClientOrderID")
	}
	if order.Side != "BUY" && order.Side != "SELL" {
		return fmt.Errorf("Side=%q 非法", order.Side)
	}
	if math.IsNaN(order.Price) || math.IsInf(order.Price, 0) || order.Price <= 0 {
		return fmt.Errorf("Price=%v 非法", order.Price)
	}
	if math.IsNaN(order.Quantity) || math.IsInf(order.Quantity, 0) || order.Quantity <= 0 {
		return fmt.Errorf("Quantity=%v 非法", order.Quantity)
	}
	if math.IsNaN(order.ExecutedQty) || math.IsInf(order.ExecutedQty, 0) || order.ExecutedQty < 0 {
		return fmt.Errorf("ExecutedQty=%v 非法", order.ExecutedQty)
	}
	tolerance := orderQtyTolerance(order.Quantity, order.ExecutedQty)
	if order.ExecutedQty > order.Quantity+tolerance {
		return fmt.Errorf("ExecutedQty=%.12f 大于 Quantity=%.12f", order.ExecutedQty, order.Quantity)
	}
	switch order.Status {
	case "NEW":
		if order.ExecutedQty > tolerance {
			return fmt.Errorf("Status=NEW 但 ExecutedQty=%.12f", order.ExecutedQty)
		}
	case "PARTIALLY_FILLED":
		if order.ExecutedQty <= tolerance || order.ExecutedQty >= order.Quantity-tolerance {
			return fmt.Errorf("Status=PARTIALLY_FILLED 与 ExecutedQty=%.12f/Quantity=%.12f 不一致",
				order.ExecutedQty, order.Quantity)
		}
	default:
		return fmt.Errorf("挂单 Status=%q 非法", order.Status)
	}
	if order.Symbol == "" {
		return fmt.Errorf("缺少 Symbol")
	}
	if order.Type != "LIMIT" {
		return fmt.Errorf("Type=%q，期望 LIMIT", order.Type)
	}
	return nil
}

func compareManagedOrderDetails(
	local map[int64]managedOrderSnapshot,
	remote map[int64]managedOrderSnapshot,
) error {
	ids := make([]int64, 0, len(local))
	for id := range local {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		localOrder := local[id]
		remoteOrder, ok := remote[id]
		if !ok {
			continue
		}
		if canonicalManagedClientOrderID(localOrder.ClientOrderID) != canonicalManagedClientOrderID(remoteOrder.ClientOrderID) {
			return fmt.Errorf("订单 %d ClientOrderID: 远端=%q, 本地=%q",
				id, remoteOrder.ClientOrderID, localOrder.ClientOrderID)
		}
		if remoteOrder.Symbol != "" && localOrder.Symbol != "" && remoteOrder.Symbol != localOrder.Symbol {
			return fmt.Errorf("订单 %d Symbol: 远端=%s, 本地=%s", id, remoteOrder.Symbol, localOrder.Symbol)
		}
		if remoteOrder.Side != localOrder.Side {
			return fmt.Errorf("订单 %d Side: 远端=%s, 本地=%s", id, remoteOrder.Side, localOrder.Side)
		}
		priceTolerance := orderQtyTolerance(remoteOrder.Price, localOrder.Price)
		if math.Abs(remoteOrder.Price-localOrder.Price) > priceTolerance {
			return fmt.Errorf("订单 %d Price: 远端=%.12f, 本地=%.12f", id, remoteOrder.Price, localOrder.Price)
		}
		quantityTolerance := orderQtyTolerance(remoteOrder.Quantity, localOrder.Quantity)
		if math.Abs(remoteOrder.Quantity-localOrder.Quantity) > quantityTolerance {
			return fmt.Errorf("订单 %d Quantity: 远端=%.12f, 本地=%.12f",
				id, remoteOrder.Quantity, localOrder.Quantity)
		}
		qtyTolerance := orderQtyTolerance(remoteOrder.ExecutedQty, localOrder.ExecutedQty)
		if math.Abs(remoteOrder.ExecutedQty-localOrder.ExecutedQty) > qtyTolerance {
			return fmt.Errorf("订单 %d ExecutedQty: 远端=%.12f, 本地=%.12f",
				id, remoteOrder.ExecutedQty, localOrder.ExecutedQty)
		}
	}
	return nil
}

func orderQtyTolerance(a, b float64) float64 {
	return math.Max(1e-12, math.Max(math.Abs(a), math.Abs(b))*1e-8)
}

func isOpenSQTClientOrderID(clientID string) bool {
	clientID = canonicalManagedClientOrderID(clientID)
	parts := strings.Split(clientID, "_")
	if len(parts) != 3 || (parts[1] != "B" && parts[1] != "S") || len(parts[2]) < 10 {
		return false
	}
	if _, err := strconv.ParseInt(parts[0], 10, 64); err != nil {
		return false
	}
	_, err := strconv.ParseInt(parts[2], 10, 64)
	return err == nil
}

func canonicalManagedClientOrderID(clientID string) string {
	for {
		original := clientID
		for _, prefix := range []string{"x-zdfVM8vY", "t-"} {
			clientID = strings.TrimPrefix(clientID, prefix)
		}
		if clientID == original {
			return clientID
		}
	}
}

func compareOrderIDSets(
	local map[int64]managedOrderSnapshot,
	remote map[int64]managedOrderSnapshot,
) (missingRemote, extraRemote []int64) {
	for id := range local {
		if _, ok := remote[id]; !ok {
			missingRemote = append(missingRemote, id)
		}
	}
	for id := range remote {
		if _, ok := local[id]; !ok {
			extraRemote = append(extraRemote, id)
		}
	}
	sort.Slice(missingRemote, func(i, j int) bool { return missingRemote[i] < missingRemote[j] })
	sort.Slice(extraRemote, func(i, j int) bool { return extraRemote[i] < extraRemote[j] })
	return missingRemote, extraRemote
}
