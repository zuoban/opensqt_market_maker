package position

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opensqt/config"
	"opensqt/logger"
	"opensqt/utils"
)

// OrderUpdate 订单更新事件（避免依赖 websocket 包）
type OrderUpdate struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Status        string
	ExecutedQty   float64
	Price         float64
	AvgPrice      float64
	Side          string
	Type          string
	UpdateTime    int64
	// RealizedPNL 交易所在成交推送里带回的已实现盈亏。
	// Incremental=true 表示本笔成交利润（如币安 rp），否则为该订单累计值。
	RealizedPNL            float64
	RealizedPNLIncremental bool
}

const (
	maxRecentFilledOrders = 20
	hourlyFillHours       = 24
	fillQtyTolerance      = 1e-12
	maxTerminalOrders     = 4096
)

var ErrAcceptedOrderPreconditionInvalid = errors.New("交易所已接受订单但槽位下单前提已失效")

type terminalOrderProgress struct {
	ExecutedQty float64
	// ReportedPNL 是交易所最后一次被接受的订单累计盈亏。
	ReportedPNL float64
	// AccountedPNL 是本地已经实际计入总盈亏的金额，可能来自成交价差回退。
	// 后续拿到交易所累计盈亏时必须以它为基线补差，避免回退值与权威值双计。
	AccountedPNL float64
	// UpdateTime 是终态进度的事件版本水位；仅 PNL 修正必须严格更新才会被接受。
	UpdateTime int64
}

// FilledOrderRecord 本次程序运行期间完成成交的订单。
type FilledOrderRecord struct {
	OrderID       int64     `json:"orderId"`
	ClientOrderID string    `json:"clientOrderId"`
	Symbol        string    `json:"symbol"`
	Side          string    `json:"side"`
	Price         float64   `json:"price"`
	Quantity      float64   `json:"quantity"`
	FilledAt      time.Time `json:"filledAt"`
	RealizedPNL   float64   `json:"realizedPnl"`
}

// HourlyFillBucket 本地时区下一小时的成交汇总，供订单汇总图使用。
type HourlyFillBucket struct {
	Hour    time.Time `json:"hour"`
	Buy     int       `json:"buy"`
	Sell    int       `json:"sell"`
	BuyQty  float64   `json:"buyQty"`
	SellQty float64   `json:"sellQty"`
	Pnl     float64   `json:"pnl"`
}

type hourlyFillAcc struct {
	buy     int
	sell    int
	buyQty  float64
	sellQty float64
	pnl     float64
}

// OrderExecutorInterface 订单执行器接口（避免循环导入）
type OrderExecutorInterface interface {
	PlaceOrder(req *OrderRequest) (*Order, error)
	BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error)
	BatchCancelOrders(orderIDs []int64) error
}

// OrderRequest 订单请求（避免循环导入）
type OrderRequest struct {
	Symbol        string
	Side          string
	Price         float64
	Quantity      float64
	PriceDecimals int    // 价格小数位数（用于格式化价格字符串）
	ReduceOnly    bool   // 是否只减仓（平仓单）
	PostOnly      bool   // 是否只做 Maker（Post Only）
	ClientOrderID string // 自定义订单ID

	// AcquireSubmissionLease 在每次真正调用交易所下单接口前执行。成功时返回的
	// release 会一直持有对应槽位锁，调用方必须在该次接口调用返回后立即释放。
	// 限流和重试等待期间不得持有 lease；同步重入同一槽位的订单回调也不受支持。
	AcquireSubmissionLease func() (release func(), ok bool)

	// submissionUncertain 表示至少一次交易所请求已经发出，但最终结果无法确认。
	// 这种 reservation 不能按普通失败释放，否则下一轮会换 ClientOrderID 重下。
	submissionUncertain atomic.Bool
}

// MarkSubmissionUncertain 由执行边界在请求结果不确定时调用。
func (r *OrderRequest) MarkSubmissionUncertain() {
	if r != nil {
		r.submissionUncertain.Store(true)
	}
}

func (r *OrderRequest) isSubmissionUncertain() bool {
	return r != nil && r.submissionUncertain.Load()
}

// Order 订单信息（避免循环导入）
type Order struct {
	OrderID       int64
	ClientOrderID string
	Symbol        string
	Side          string
	Type          string
	Price         float64
	Quantity      float64
	ExecutedQty   float64
	AvgPrice      float64
	Status        string
	CreatedAt     time.Time
	UpdateTime    int64
}

// 订单状态常量
const (
	OrderStatusNotPlaced       = "NOT_PLACED"       // 未下单
	OrderStatusPlaced          = "PLACED"           // 已下单
	OrderStatusConfirmed       = "CONFIRMED"        // 已确认（WebSocket确认）
	OrderStatusPartiallyFilled = "PARTIALLY_FILLED" // 部分成交
	OrderStatusFilled          = "FILLED"           // 全部成交
	OrderStatusCancelRequested = "CANCEL_REQUESTED" // 已申请撤单
	OrderStatusCanceled        = "CANCELED"         // 已撤单
)

// 持仓状态常量
const (
	PositionStatusEmpty  = "EMPTY"  // 空仓
	PositionStatusFilled = "FILLED" // 有仓
)

// 槽位锁定状态
const (
	SlotStatusFree    = "FREE"    // 空闲，可操作
	SlotStatusPending = "PENDING" // 等待下单确认
	SlotStatusLocked  = "LOCKED"  // 已锁定，有活跃订单
)

// InventorySlot 库存槽位（每个价格点一个）
type InventorySlot struct {
	Price float64 // 价格（作为key，支持高精度）

	// 持仓信息
	PositionStatus string  // 持仓状态：空仓/有仓
	PositionQty    float64 // 持仓数量（支持小数点后3位）

	// 订单信息 (买卖互斥)
	OrderID        int64     // 订单ID
	ClientOID      string    // 自定义订单ID
	OrderSide      string    // 订单方向 (BUY/SELL)
	OrderStatus    string    // 订单状态
	OrderPrice     float64   // 订单价格
	OrderQuantity  float64   // reservation/活动订单的提交数量
	OrderFilledQty float64   // 成交数量
	OrderCreatedAt time.Time // 创建时间

	// 🔥 新增：槽位锁定状态，防止并发重复操作
	SlotStatus string // FREE/PENDING/LOCKED

	// 历史字段：卖单撤销/拒绝计数（仅用于诊断，不会降级为普通单）
	PostOnlyFailCount int

	// 当前订单已计入的累计已实现盈亏（非增量推送用）
	orderReportedPNL float64
	// 当前订单从部分成交到完全成交累计的已实现盈亏（用于成交历史）。
	orderAccumulatedPNL float64

	mu sync.RWMutex // 槽位级别的锁（细粒度锁）
}

// PositionInfo 持仓信息（简化版，避免循环导入）
type PositionInfo struct {
	Symbol string
	Size   float64
}

// IExchange 交易所接口（避免循环导入）
// 注意：这里不能直接使用 exchange.IExchange，否则会循环导入
// 所以定义一个子集接口，只包含对账需要的方法
type IExchange interface {
	GetName() string // 获取交易所名称
	GetPositions(ctx context.Context, symbol string) (interface{}, error)
	GetOpenOrders(ctx context.Context, symbol string) (interface{}, error)
	GetOrder(ctx context.Context, symbol string, orderID int64) (interface{}, error)
	GetBaseAsset() string                                     // 获取基础资产（交易币种）
	CancelAllOrders(ctx context.Context, symbol string) error // 取消所有订单
}

// SuperPositionManager 超级仓位管理器
type SuperPositionManager struct {
	config   *config.Config
	executor OrderExecutorInterface
	exchange IExchange

	// 价格锚点（初始化时的市场价格）
	anchorPrice float64
	// 最后市场价格（用于打印状态）
	lastMarketPrice atomic.Value // float64
	// 价格精度（根据锚点价格检测得出的小数位数）
	priceDecimals int
	// 数量精度（从交易所获取）
	quantityDecimals int

	// 库存槽位：价格 -> 槽位
	slots sync.Map // map[float64]*InventorySlot

	// 保证金管理
	insufficientMargin bool
	marginLockTime     time.Time
	marginLockDuration time.Duration

	// 统计（注意：以下字段被 safety.Reconciler 和 PrintPositions 使用，不可删除）
	totalBuyQty       atomic.Value // float64 - 累计买入数量
	totalSellQty      atomic.Value // float64 - 累计卖出数量
	realizedPNL       atomic.Value // float64 - 卖单成交累计已实现盈亏
	reconcileCount    atomic.Int64 // 对账次数
	lastReconcileTime atomic.Value // time.Time - 最后对账时间
	pnlMu             sync.Mutex   // 保护 realizedPNL 累加

	// 本次运行期间的成交订单：列表面板只保留最近 maxRecentFilledOrders 笔，
	// 小时汇总单独累计近 hourlyFillHours 小时，避免列表截断影响图表。
	filledOrdersMu   sync.RWMutex
	filledOrders     []FilledOrderRecord
	filledOrderKeys  map[string]struct{}
	filledHourly     map[int64]*hourlyFillAcc
	filledOrderCount int64

	// 终态订单的最后累计成交进度。槽位清空后，交易所仍可能重放终态或乱序成交推送；
	// 按订单保留单调进度，避免同一成交被再次计入持仓、统计和盈亏。
	terminalOrdersMu  sync.RWMutex
	terminalOrders    map[string]terminalOrderProgress
	terminalOrderKeys []string

	// 初始化标志
	isInitialized atomic.Bool

	mu sync.RWMutex // 全局锁（用于关键操作）
}

// NewSuperPositionManager 创建超级仓位管理器
func NewSuperPositionManager(cfg *config.Config, executor OrderExecutorInterface, exchange IExchange, priceDecimals, quantityDecimals int) *SuperPositionManager {
	marginLockSec := cfg.Trading.MarginLockDurationSec
	if marginLockSec <= 0 {
		marginLockSec = 10 // 默认10秒
	}

	spm := &SuperPositionManager{
		config:             cfg,
		executor:           executor,
		exchange:           exchange,
		insufficientMargin: false,
		marginLockDuration: time.Duration(marginLockSec) * time.Second,
		priceDecimals:      priceDecimals,
		quantityDecimals:   quantityDecimals,
		filledOrderKeys:    make(map[string]struct{}),
		filledHourly:       make(map[int64]*hourlyFillAcc),
		terminalOrders:     make(map[string]terminalOrderProgress),
	}
	spm.totalBuyQty.Store(0.0)
	spm.totalSellQty.Store(0.0)
	spm.realizedPNL.Store(0.0)
	spm.lastReconcileTime.Store(time.Now())
	spm.lastMarketPrice.Store(0.0)
	return spm
}

// Initialize 初始化管理器（设置价格锚点并创建初始槽位）
func (spm *SuperPositionManager) Initialize(initialPrice float64, initialPriceStr string) error {
	spm.mu.Lock()
	defer spm.mu.Unlock()

	if initialPrice <= 0 {
		return fmt.Errorf("初始价格无效: %.2f", initialPrice)
	}

	// 1. 设置价格锚点（精度信息已经在构造函数中设置，从交易所获取）
	spm.anchorPrice = initialPrice
	spm.lastMarketPrice.Store(initialPrice) // 初始化最后市场价格
	logger.Info("✅ 价格锚点已设置: %s, 价格精度:%d, 数量精度:%d",
		formatPrice(initialPrice, spm.priceDecimals), spm.priceDecimals, spm.quantityDecimals)

	// 2. 直接使用锚点价格作为网格价格（不再对齐到整数）
	initialGridPrice := spm.anchorPrice
	logger.Info("✅ 初始网格价格: %s (使用锚点价格)", formatPrice(initialGridPrice, spm.priceDecimals))

	// 4. 使用统一的槽位价格计算方法创建初始槽位
	slotPrices := spm.calculateSlotPrices(initialGridPrice, spm.config.Trading.BuyWindowSize, "down")
	for _, price := range slotPrices {
		spm.getOrCreateSlot(price)
	}
	// 格式化槽位价格用于日志输出
	slotPricesStr := make([]string, len(slotPrices))
	for i, p := range slotPrices {
		slotPricesStr[i] = formatPrice(p, spm.priceDecimals)
	}
	logger.Info("✅ [初始化] 计算出的槽位价格: %v", slotPricesStr)

	// 5. 为初始槽位下买单
	err := spm.placeInitialBuyOrders()
	if err == nil {
		// 标记为已初始化
		spm.isInitialized.Store(true)
		logger.Info("✅ 初始化完成，网格价格: %s", formatPrice(initialGridPrice, spm.priceDecimals))
	}
	return err
}

// generateClientOrderID 生成自定义订单ID
// 使用新的紧凑格式，最大长度不超过18字符
// 格式: {price_int}_{side}_{timestamp}{seq}
// price_int: price * 10^decimals (转为整数)
// side: B=Buy, S=Sell
func (spm *SuperPositionManager) generateClientOrderID(price float64, side string) string {
	// 使用统一的 utils 包生成紧凑ID
	return utils.GenerateOrderID(price, side, spm.priceDecimals)
}

// parseClientOrderID 解析 ClientOrderID
// 返回: price, side, valid
func (spm *SuperPositionManager) parseClientOrderID(clientOrderID string) (float64, string, bool) {
	// 1. 先移除交易所前缀
	cleanID := spm.canonicalClientOrderID(clientOrderID)

	// 2. 使用统一的 utils 包解析
	price, side, _, valid := utils.ParseOrderID(cleanID, spm.priceDecimals)
	if !valid {
		return 0, "", false
	}

	// 🔥 关键修复：不要对从ClientOrderID解析出的价格进行四舍五入！
	// 因为价格本身就是从整数还原的，已经是精确的值
	// 如果再次四舍五入，可能因为浮点数精度问题导致多个不同价格被映射到同一个槽位
	// 例如: 3116.85 和 3114.85 可能都被四舍五入成同一个值

	return price, side, true
}

func (spm *SuperPositionManager) canonicalClientOrderID(clientOrderID string) string {
	exchangeName := strings.ToLower(spm.exchange.GetName())
	return utils.RemoveBrokerPrefix(exchangeName, clientOrderID)
}

func sameOrderQuantity(a, b float64) bool {
	return math.Abs(a-b) <= fillQtyTolerance
}

// reserveOrderLocked 将待提交订单的完整身份绑定到槽位。调用方必须持有 slot.mu。
// 仅有 PENDING 不足以证明某个请求仍属于该槽位；ClientOID、方向和数量共同组成
// reservation，失败清理和提交前复检都只能操作完全匹配的 reservation。
func (spm *SuperPositionManager) reserveOrderLocked(slot *InventorySlot, req *OrderRequest) {
	slot.OrderID = 0
	slot.ClientOID = req.ClientOrderID
	slot.OrderSide = req.Side
	slot.OrderStatus = OrderStatusNotPlaced
	slot.OrderPrice = req.Price
	slot.OrderQuantity = req.Quantity
	slot.OrderFilledQty = 0
	slot.OrderCreatedAt = time.Now()
	slot.orderReportedPNL = 0
	slot.orderAccumulatedPNL = 0
	slot.SlotStatus = SlotStatusPending

	req.AcquireSubmissionLease = func() (func(), bool) {
		slot.mu.Lock()
		if !spm.matchesReservationLocked(slot, req) || !spm.reservationStillValidLocked(slot, req) {
			// 只清理由本请求创建且尚未被订单流确认的 reservation。若身份已经
			// 变化，说明另一个时序已经接管槽位，绝不能碰它。
			if spm.matchesReservationLocked(slot, req) {
				spm.clearReservationLocked(slot)
			}
			slot.mu.Unlock()
			return nil, false
		}
		return slot.mu.Unlock, true
	}
}

func (spm *SuperPositionManager) matchesReservationLocked(slot *InventorySlot, req *OrderRequest) bool {
	return slot.SlotStatus == SlotStatusPending &&
		slot.OrderID == 0 &&
		slot.ClientOID == req.ClientOrderID &&
		slot.OrderSide == req.Side &&
		slot.OrderStatus == OrderStatusNotPlaced &&
		sameOrderQuantity(slot.OrderQuantity, req.Quantity)
}

func (spm *SuperPositionManager) reservationStillValidLocked(slot *InventorySlot, req *OrderRequest) bool {
	if req == nil || req.Quantity <= 0 || math.IsNaN(req.Quantity) || math.IsInf(req.Quantity, 0) {
		return false
	}
	switch req.Side {
	case "BUY":
		return !req.ReduceOnly && slot.PositionStatus == PositionStatusEmpty &&
			slot.PositionQty <= fillQtyTolerance
	case "SELL":
		return req.ReduceOnly && slot.PositionStatus == PositionStatusFilled &&
			slot.PositionQty > fillQtyTolerance && sameOrderQuantity(slot.PositionQty, req.Quantity)
	default:
		return false
	}
}

// acceptedOrderPreconditionStillValidLocked 验证一个已进入交易所边界的订单
// 是否仍与槽位库存一致。对活跃部分成交订单，BUY 的库存必须等于该单
// 已成交量，SELL 的剩余库存必须等于委托量减已成交量。这能识别旧订单
// 迟到终态在 REST 返回后对当前 reservation 造成的库存修正。
func (spm *SuperPositionManager) acceptedOrderPreconditionStillValidLocked(slot *InventorySlot, req *OrderRequest) bool {
	if slot == nil || req == nil || req.Quantity <= 0 ||
		math.IsNaN(req.Quantity) || math.IsInf(req.Quantity, 0) ||
		math.IsNaN(slot.PositionQty) || math.IsInf(slot.PositionQty, 0) || slot.PositionQty < 0 ||
		math.IsNaN(slot.OrderFilledQty) || math.IsInf(slot.OrderFilledQty, 0) || slot.OrderFilledQty < 0 {
		return false
	}
	if slot.OrderFilledQty > req.Quantity+fillQtyTolerance {
		return false
	}

	switch req.Side {
	case "BUY":
		return !req.ReduceOnly && slot.PositionStatus == PositionStatusEmpty &&
			sameOrderQuantity(slot.PositionQty, slot.OrderFilledQty)
	case "SELL":
		remaining := req.Quantity - slot.OrderFilledQty
		return req.ReduceOnly && slot.PositionStatus == PositionStatusFilled &&
			remaining > fillQtyTolerance && sameOrderQuantity(slot.PositionQty, remaining)
	default:
		return false
	}
}

func normalizedPlacementStatus(status string) string {
	status = strings.ToUpper(strings.TrimSpace(status))
	if status == "" || status == OrderStatusPlaced || status == OrderStatusConfirmed {
		return "NEW"
	}
	return status
}

func knownPlacementStatus(status string) bool {
	switch normalizedPlacementStatus(status) {
	case "NEW", OrderStatusPartiallyFilled, OrderStatusFilled,
		"CANCELED", "EXPIRED", "REJECTED":
		return true
	default:
		return false
	}
}

func placementStatusIsTerminal(status string) bool {
	status = normalizedPlacementStatus(status)
	return status == OrderStatusFilled || isTerminalOrderStatus(status)
}

func placementOrderUpdate(ord *Order, side string) OrderUpdate {
	return OrderUpdate{
		OrderID:       ord.OrderID,
		ClientOrderID: ord.ClientOrderID,
		Symbol:        ord.Symbol,
		Status:        normalizedPlacementStatus(ord.Status),
		ExecutedQty:   ord.ExecutedQty,
		Price:         ord.Price,
		AvgPrice:      ord.AvgPrice,
		Side:          side,
		Type:          ord.Type,
		UpdateTime:    ord.UpdateTime,
	}
}

// bindAcceptedOrderForCancellationLocked 把已被交易所接受、但槽位前提已失效的
// 非终态订单线性化为 CANCEL_REQUESTED。订单保持本地可见，后续对账固定
// fail-closed，直到订单流或权威回读给出终态。调用方必须持有 slot.mu。
func (spm *SuperPositionManager) bindAcceptedOrderForCancellationLocked(
	slot *InventorySlot,
	req *OrderRequest,
	ord *Order,
	side string,
) error {
	if ord == nil || ord.OrderID <= 0 {
		return fmt.Errorf("已接受订单缺少有效 OrderID")
	}
	if ord.ClientOrderID == "" ||
		spm.canonicalClientOrderID(ord.ClientOrderID) != spm.canonicalClientOrderID(req.ClientOrderID) {
		return fmt.Errorf("已接受订单 ClientOID=%q 与 reservation=%q 不匹配",
			ord.ClientOrderID, req.ClientOrderID)
	}
	quantity := ord.Quantity
	if quantity <= 0 || math.IsNaN(quantity) || math.IsInf(quantity, 0) {
		quantity = req.Quantity
	}

	slot.OrderID = ord.OrderID
	slot.ClientOID = ord.ClientOrderID
	slot.OrderSide = side
	slot.OrderPrice = ord.Price
	slot.OrderQuantity = quantity
	if ord.CreatedAt.IsZero() {
		slot.OrderCreatedAt = time.Now()
	} else {
		slot.OrderCreatedAt = ord.CreatedAt
	}
	slot.SlotStatus = SlotStatusLocked

	update := placementOrderUpdate(ord, side)
	_, executionInvalid := spm.applyOrderExecutionDelta(slot, update, side, slot.Price)
	slot.OrderStatus = OrderStatusCancelRequested
	if executionInvalid {
		return fmt.Errorf("已接受订单 %d 的累计成交量无法安全收敛", ord.OrderID)
	}
	return nil
}

func (spm *SuperPositionManager) clearReservationLocked(slot *InventorySlot) {
	slot.OrderID = 0
	slot.ClientOID = ""
	slot.OrderSide = ""
	slot.OrderStatus = OrderStatusNotPlaced
	slot.OrderPrice = 0
	slot.OrderQuantity = 0
	slot.OrderFilledQty = 0
	slot.OrderCreatedAt = time.Time{}
	slot.orderReportedPNL = 0
	slot.orderAccumulatedPNL = 0
	slot.SlotStatus = SlotStatusFree
}

// releaseFailedReservation 只释放明确未提交且身份仍完全匹配的 reservation。
// UNKNOWN 请求由调用方保留，等待订单流或对账给出确定结果。
func (spm *SuperPositionManager) releaseFailedReservation(req *OrderRequest) {
	price, _, valid := spm.parseClientOrderID(req.ClientOrderID)
	if !valid {
		return
	}
	slot := spm.getOrCreateSlot(price)
	slot.mu.Lock()
	if spm.matchesReservationLocked(slot, req) {
		spm.clearReservationLocked(slot)
		logger.Debug("🔓 [释放槽位] 明确未提交，释放槽位 %s 的 reservation (ClientOID: %s)",
			formatPrice(price, spm.priceDecimals), req.ClientOrderID)
	}
	slot.mu.Unlock()
}

// placeInitialBuyOrders 设定初始槽位（并恢复持仓槽位）
func (spm *SuperPositionManager) placeInitialBuyOrders() error {
	// 🔥 修改：只恢复持仓槽位，不再主动下单
	// 所有下单操作由 AdjustOrders 统一处理，避免时序问题
	existingPosition := spm.getExistingPosition()
	if existingPosition > 0 {
		logger.Info("🔄 [持仓恢复] 检测到现有持仓: %.4f，开始初始化卖单槽位", existingPosition)
		spm.initializeSellSlotsFromPosition(existingPosition)
	}

	logger.Info("✅ [初始化] 槽位已创建，订单下达将由 AdjustOrders 统一处理")
	return nil
}

// AdjustOrders 调整订单（交易入口）
func (spm *SuperPositionManager) AdjustOrders(currentPrice float64) error {
	// 🔥 移除初始化检查：现在完全由 AdjustOrders 控制所有下单
	// 初始化只负责恢复持仓状态，不再下单

	spm.mu.Lock()
	defer spm.mu.Unlock()

	// 验证价格有效性
	if currentPrice <= 0 {
		logger.Warn("⚠️ 收到无效价格: %.2f，跳过订单调整", currentPrice)
		return nil
	}

	// 对当前价格进行精度处理
	currentPrice = roundPrice(currentPrice, spm.priceDecimals)

	// 更新最后市场价格（用于打印状态）
	spm.lastMarketPrice.Store(currentPrice)

	// 检查保证金不足状态
	if spm.insufficientMargin {
		if time.Since(spm.marginLockTime) >= spm.marginLockDuration {
			logger.Info("✅ [保证金恢复] 锁定时间已过，恢复下单功能")
			spm.insufficientMargin = false
		} else {
			remainingTime := spm.marginLockDuration - time.Since(spm.marginLockTime)
			logger.Warn("⏸️ [暂停下单] 保证金不足，暂停下单中... (剩余时间: %.0f秒)", remainingTime.Seconds())
			return nil
		}
	}

	// 计算需要监控的价格范围
	buyWindowSize := spm.config.Trading.BuyWindowSize
	sellWindowSize := spm.config.Trading.SellWindowSize
	priceInterval := spm.config.Trading.PriceInterval

	// 动态计算网格价格
	currentGridPrice := spm.findNearestGridPrice(currentPrice)
	// logger.Debug("🔄 [实时调整] 当前价格: %s, 网格价格: %s, 买单窗口: %d, 卖单窗口: %d",
	// 	formatPrice(currentPrice, spm.priceDecimals), formatPrice(currentGridPrice, spm.priceDecimals), buyWindowSize, sellWindowSize)

	// 计算当前网格价格下方buy_window_size个价格
	slotPrices := spm.calculateSlotPrices(currentGridPrice, buyWindowSize, "down")

	var ordersToPlace []*OrderRequest
	var activeBuyOrdersInWindow int

	// 统计当前所有订单数量（分别统计买单和卖单）
	var currentOrderCount int
	var currentBuyOrderCount int
	var currentSellOrderCount int
	spm.slots.Range(func(key, value interface{}) bool {
		slot := value.(*InventorySlot)
		slot.mu.RLock()
		if slot.OrderStatus == OrderStatusPlaced || slot.OrderStatus == OrderStatusConfirmed ||
			slot.OrderStatus == OrderStatusPartiallyFilled {
			currentOrderCount++
			if slot.OrderSide == "BUY" {
				currentBuyOrderCount++
			} else if slot.OrderSide == "SELL" {
				currentSellOrderCount++
			}
		}
		slot.mu.RUnlock()
		return true
	})

	// 计算允许创建的订单数量上限
	threshold := spm.config.Trading.OrderCleanupThreshold
	if threshold <= 0 {
		threshold = 100
	}

	// 🔥 核心改进：不预留空间，允许订单数达到threshold上限
	// 剩余可用订单数 = 阈值 - 当前订单数
	remainingOrders := threshold - currentOrderCount
	if remainingOrders < 0 {
		remainingOrders = 0
	}

	// 买单允许的新增数量
	allowedNewBuyOrders := buyWindowSize
	if allowedNewBuyOrders > remainingOrders {
		allowedNewBuyOrders = remainingOrders
	}

	// 1. 处理买单
	buyOrdersToCreate := 0

	for _, price := range slotPrices {
		slot := spm.getOrCreateSlot(price)
		slot.mu.Lock()

		// 🔥 槽位锁定检查：如果槽位正在被操作，跳过
		if slot.SlotStatus != SlotStatusFree {
			slot.mu.Unlock()
			continue
		}

		// 检查是否已有有效订单
		hasActiveOrder := false
		if slot.OrderStatus == OrderStatusPlaced || slot.OrderStatus == OrderStatusConfirmed ||
			slot.OrderStatus == OrderStatusPartiallyFilled {
			hasActiveOrder = true
			if slot.OrderSide == "BUY" {
				activeBuyOrdersInWindow++
			}
		}

		// 🔥 买单条件：持仓状态=EMPTY + 槽位锁=FREE + 无订单ID + 无ClientOID
		if slot.PositionStatus != PositionStatusEmpty {
			slot.mu.Unlock()
			continue
		}

		// 🔥 新逻辑：只检查槽位锁状态、OrderID和ClientOID，不检查OrderSide
		shouldCreateBuyOrder := !hasActiveOrder &&
			slot.SlotStatus == SlotStatusFree &&
			slot.OrderID == 0 &&
			slot.ClientOID == "" &&
			buyOrdersToCreate < allowedNewBuyOrders

		if shouldCreateBuyOrder {
			// 安全检查：买单价格不应高于当前价格
			safetyBuffer := spm.config.Trading.PriceInterval * 0.1
			if price >= currentPrice-safetyBuffer {
				slot.mu.Unlock()
				continue
			}

			quantity := spm.config.Trading.OrderQuantity / price
			// 使用从交易所获取的数量精度
			quantity = roundPrice(quantity, spm.quantityDecimals)

			// 生成 ClientOrderID
			clientOID := spm.generateClientOrderID(price, "BUY")

			req := &OrderRequest{
				Symbol:        spm.config.Trading.Symbol,
				Side:          "BUY",
				Price:         price,
				Quantity:      quantity,
				PriceDecimals: spm.priceDecimals,
				PostOnly:      true,
				ClientOrderID: clientOID,
			}
			spm.reserveOrderLocked(slot, req)
			ordersToPlace = append(ordersToPlace, req)
			buyOrdersToCreate++
		}

		slot.mu.Unlock()
	}

	// 2. 处理卖单
	sellWindowMaxPrice := currentPrice + float64(sellWindowSize)*priceInterval
	sellWindowMaxPrice = roundPrice(sellWindowMaxPrice, spm.priceDecimals)

	type sellCandidate struct {
		SlotPrice     float64 // 槽位价格 (买入价)
		SellPrice     float64 // 目标卖出价
		DistanceToMid float64
	}
	var sellCandidates []sellCandidate

	spm.slots.Range(func(key, value interface{}) bool {
		slotPrice := key.(float64) // 槽位Key = 买入价
		slot := value.(*InventorySlot)
		slot.mu.Lock()
		defer slot.mu.Unlock()

		// 🔥 卖单条件：持仓状态=FILLED + 槽位锁=FREE + 无订单ID + 无ClientOID
		if slot.PositionStatus == PositionStatusFilled &&
			slot.SlotStatus == SlotStatusFree &&
			slot.OrderID == 0 &&
			slot.ClientOID == "" {

			sellPrice := slotPrice + priceInterval
			sellPrice = roundPrice(sellPrice, spm.priceDecimals)

			// 窗口检查
			if slotPrice > sellWindowMaxPrice {
				return true
			}

			// 最小名义价值检查
			orderValue := sellPrice * slot.PositionQty
			minValue := spm.config.Trading.MinOrderValue
			if minValue <= 0 {
				minValue = 6.0
			}

			if orderValue >= minValue {
				distance := math.Abs(slotPrice - currentPrice)
				sellCandidates = append(sellCandidates, sellCandidate{
					SlotPrice:     slotPrice,
					SellPrice:     sellPrice,
					DistanceToMid: distance,
				})
			}
		}
		return true
	})

	// 按距离排序
	sort.Slice(sellCandidates, func(i, j int) bool {
		return sellCandidates[i].DistanceToMid < sellCandidates[j].DistanceToMid
	})

	// 🔥 重新计算卖单的剩余配额（扣除新增买单后的剩余空间）
	remainingOrdersForSell := threshold - currentOrderCount - buyOrdersToCreate
	if remainingOrdersForSell < 0 {
		remainingOrdersForSell = 0
	}

	allowedNewSellOrders := sellWindowSize
	if allowedNewSellOrders > remainingOrdersForSell {
		allowedNewSellOrders = remainingOrdersForSell
	}

	// 生成卖单请求
	sellOrdersToCreate := 0
	// 🔥 调试日志: 显示订单配额计算详情（包含买卖单分布）
	logger.Debug("📊 [订单配额] 阈值:%d, 当前订单:%d(买:%d/卖:%d), 剩余:%d, 新增买单:%d, 卖单候选:%d, 允许卖单:%d",
		threshold, currentOrderCount, currentBuyOrderCount, currentSellOrderCount, remainingOrders, buyOrdersToCreate, len(sellCandidates), allowedNewSellOrders)
	if allowedNewSellOrders > 0 {
		for i := 0; i < len(sellCandidates) && sellOrdersToCreate < allowedNewSellOrders; i++ {
			candidate := sellCandidates[i]

			// 🔥 关键修复：最终验证PositionStatus必须为FILLED且有持仓，并且SlotStatus为FREE
			slot := spm.getOrCreateSlot(candidate.SlotPrice)
			slot.mu.Lock()

			// 🔥 双重检查：确保槽位仍然是FREE状态
			if slot.SlotStatus != SlotStatusFree {
				slot.mu.Unlock()
				continue
			}

			currentQty := slot.PositionQty
			if slot.PositionStatus != PositionStatusFilled || currentQty <= 0 {
				slot.mu.Unlock()
				continue
			}

			// 候选收集后持仓可能已被终态修正。名义价值和提交数量都必须使用
			// 当前锁内值，不能沿用候选快照。
			minValue := spm.config.Trading.MinOrderValue
			if minValue <= 0 {
				minValue = 6.0
			}
			if candidate.SellPrice*currentQty < minValue {
				slot.mu.Unlock()
				continue
			}

			// 生成 ClientOrderID (注意：使用 SlotPrice 即买入价作为标识)
			clientOID := spm.generateClientOrderID(candidate.SlotPrice, "SELL")

			req := &OrderRequest{
				Symbol:        spm.config.Trading.Symbol,
				Side:          "SELL",
				Price:         candidate.SellPrice,
				Quantity:      currentQty,
				PriceDecimals: spm.priceDecimals,
				ReduceOnly:    true,
				PostOnly:      true,
				ClientOrderID: clientOID, // 🔥
			}
			spm.reserveOrderLocked(slot, req)
			ordersToPlace = append(ordersToPlace, req)
			slot.mu.Unlock()
			sellOrdersToCreate++
		}
	}

	// 执行下单
	if len(ordersToPlace) > 0 {
		logger.Debug("🔄 [实时调整] 需要新增: %d 个订单", len(ordersToPlace))
		requestsByClientOID := make(map[string]*OrderRequest, len(ordersToPlace))
		for _, req := range ordersToPlace {
			requestsByClientOID[spm.canonicalClientOrderID(req.ClientOrderID)] = req
		}
		placedOrders, marginError, placementErr := spm.executor.BatchPlaceOrders(ordersToPlace)

		if marginError {
			logger.Warn("⚠️ [保证金不足] 检测到保证金不足错误，暂停下单 %d 秒", int(spm.marginLockDuration.Seconds()))
			spm.insufficientMargin = true
			spm.marginLockTime = time.Now()
			if err := spm.CancelAllBuyOrders(); err != nil {
				placementErr = errors.Join(placementErr, fmt.Errorf("保证金不足后的撤买单确认失败: %w", err))
			}
		}

		// 🔥 构建成功订单的ClientOrderID集合
		placedClientOIDs := make(map[string]bool)
		for _, ord := range placedOrders {
			if ord == nil {
				continue
			}
			placedClientOIDs[spm.canonicalClientOrderID(ord.ClientOrderID)] = true
		}

		// 🔥 释放未成功提交订单的槽位锁
		for _, req := range ordersToPlace {
			if !placedClientOIDs[spm.canonicalClientOrderID(req.ClientOrderID)] &&
				!req.isSubmissionUncertain() {
				spm.releaseFailedReservation(req)
			}
		}

		var acceptedOrderErrs []error
		acceptedOrdersToCancel := make(map[int64]struct{})
		for _, ord := range placedOrders {
			if ord == nil {
				acceptedOrderErrs = append(acceptedOrderErrs, fmt.Errorf("下单返回空订单"))
				continue
			}
			// 解析 ClientOrderID
			price, side, valid := spm.parseClientOrderID(ord.ClientOrderID)

			if !valid {
				logger.Warn("⚠️ [实时调整] 无法解析 ClientOID: %s", ord.ClientOrderID)
				acceptedOrderErrs = append(acceptedOrderErrs,
					fmt.Errorf("交易所已接受 OrderID=%d，但 ClientOID=%q 无法解析", ord.OrderID, ord.ClientOrderID))
				if ord.OrderID > 0 {
					acceptedOrdersToCancel[ord.OrderID] = struct{}{}
				}
				continue
			}
			req, expected := requestsByClientOID[spm.canonicalClientOrderID(ord.ClientOrderID)]
			if !expected || req.Side != side {
				logger.Warn("⚠️ [实时调整] 下单回包不属于本批 reservation: ClientOID=%s, Side=%s",
					ord.ClientOrderID, side)
				acceptedOrderErrs = append(acceptedOrderErrs,
					fmt.Errorf("交易所已接受 OrderID=%d，但不属于本批 reservation", ord.OrderID))
				if ord.OrderID > 0 {
					acceptedOrdersToCancel[ord.OrderID] = struct{}{}
				}
				continue
			}

			// 获取槽位 (注意：无论是买单还是卖单，ID中编码的都是 SlotPrice)
			slot := spm.getOrCreateSlot(price)
			slot.mu.Lock()

			// 用户数据流可能在 REST 下单返回前就推送取消类终态。此时槽位已经
			// 被终态处理释放，绝不能再用迟到的 REST 回包把旧订单复活为 LOCKED。
			if _, terminalSeen := spm.getTerminalOrderProgress(OrderUpdate{
				OrderID:       ord.OrderID,
				ClientOrderID: ord.ClientOrderID,
			}); terminalSeen {
				logger.Debug("⏭️ [忽略迟到下单回包] 终态订单不再回填: OrderID=%d, ClientOID=%s",
					ord.OrderID, ord.ClientOrderID)
				slot.mu.Unlock()
				continue
			}

			// 🔥 关键修复：检查是否是秒成交场景（买单或卖单都可能）
			// 秒成交的特征:
			// 1. 买单秒成交: PositionStatus=FILLED (刚成交) 且 OrderID=0 (已被WebSocket清空) 且 OrderSide=""
			// 2. 卖单秒成交: PositionStatus=EMPTY (已清空) 且 OrderID=0 (已被WebSocket清空) 且 OrderSide=""
			isInstantFill := false
			if side == "BUY" {
				// 买单秒成交: 有持仓但订单ID为0且OrderSide已清空
				isInstantFill = (slot.PositionStatus == PositionStatusFilled && slot.OrderID == 0 && slot.OrderSide == "")
			} else if side == "SELL" {
				// 🔥 卖单秒成交: 持仓已清空且订单ID为0且OrderSide已清空
				isInstantFill = (slot.PositionStatus == PositionStatusEmpty && slot.OrderID == 0 && slot.OrderSide == "" && slot.SlotStatus == SlotStatusFree)
			}

			if !isInstantFill {
				reservationPending := spm.matchesReservationLocked(slot, req)
				sameConfirmedOrder := slot.SlotStatus == SlotStatusLocked &&
					spm.canonicalClientOrderID(slot.ClientOID) == spm.canonicalClientOrderID(req.ClientOrderID) &&
					slot.OrderSide == req.Side
				if !reservationPending && !sameConfirmedOrder {
					logger.Warn("⚠️ [忽略过期下单回包] 槽位 %s 已不属于 ClientOID=%s",
						formatPrice(price, spm.priceDecimals), ord.ClientOrderID)
					slot.mu.Unlock()
					continue
				}

				statusKnown := knownPlacementStatus(ord.Status)
				preconditionValid := spm.acceptedOrderPreconditionStillValidLocked(slot, req)
				if !statusKnown || !preconditionValid {
					update := placementOrderUpdate(ord, side)
					if statusKnown && placementStatusIsTerminal(ord.Status) {
						// 终态回包已无远端活跃单，先按权威累计成交量
						// 收敛本地库存；仍向上返错，强制门禁重新对账。
						slot.mu.Unlock()
						spm.OnOrderUpdate(update)
					} else {
						bindErr := spm.bindAcceptedOrderForCancellationLocked(slot, req, ord, side)
						slot.mu.Unlock()
						if bindErr != nil {
							acceptedOrderErrs = append(acceptedOrderErrs,
								fmt.Errorf("订单 %d 无法安全绑定为待撤: %w", ord.OrderID, bindErr))
						}
						if ord.OrderID > 0 {
							acceptedOrdersToCancel[ord.OrderID] = struct{}{}
						}
					}

					if !preconditionValid {
						acceptedOrderErrs = append(acceptedOrderErrs, fmt.Errorf(
							"%w: 槽位 %s %s 订单 OrderID=%d 已进入交易所",
							ErrAcceptedOrderPreconditionInvalid,
							formatPrice(price, spm.priceDecimals), side, ord.OrderID))
					} else {
						acceptedOrderErrs = append(acceptedOrderErrs, fmt.Errorf(
							"交易所已接受 OrderID=%d，但返回未知状态 %q",
							ord.OrderID, ord.Status))
					}
					continue
				}

				// UNKNOWN 按 ClientOrderID 回读可能直接得到部分成交或
				// 终态。这些是权威状态，必须经统一成交通道收敛，不得回填
				// 为普通 PLACED/LOCKED。
				if normalizedPlacementStatus(ord.Status) != "NEW" {
					update := placementOrderUpdate(ord, side)
					slot.mu.Unlock()
					spm.OnOrderUpdate(update)
					continue
				}

				// 正常情况: 更新订单状态
				// 🔥 检查OrderID冲突：只有当ClientOID已设置且不匹配时才是真正的冲突
				// 如果ClientOID为空或匹配，说明是正常的WebSocket先到或批量处理顺序问题
				if slot.OrderID != 0 && slot.OrderID != ord.OrderID {
					if slot.ClientOID != "" && slot.ClientOID != ord.ClientOrderID {
						// 真正的冲突：槽位已被其他订单占用
						logger.Warn("⚠️ [OrderID冲突] 槽位 %.2f: 下单返回OrderID=%d (ClientOID=%s)，但槽位已被OrderID=%d (ClientOID=%s)占用",
							price, ord.OrderID, ord.ClientOrderID, slot.OrderID, slot.ClientOID)
					} else {
						// WebSocket推送先到达，这是正常现象
						logger.Debug("📝 [覆盖OrderID] 槽位 %.2f: WebSocket已设置OrderID=%d，现用下单返回的OrderID=%d (ClientOID: %s)",
							price, slot.OrderID, ord.OrderID, ord.ClientOrderID)
					}
				}

				slot.OrderID = ord.OrderID
				slot.ClientOID = ord.ClientOrderID
				slot.OrderSide = side // "BUY" or "SELL"
				if reservationPending {
					slot.OrderStatus = OrderStatusPlaced
				}
				slot.OrderPrice = ord.Price
				if ord.Quantity > 0 {
					slot.OrderQuantity = ord.Quantity
				}
				if ord.CreatedAt.IsZero() {
					slot.OrderCreatedAt = time.Now()
				} else {
					slot.OrderCreatedAt = ord.CreatedAt
				}
				// 🔥 订单提交成功，设置为LOCKED状态
				slot.SlotStatus = SlotStatusLocked
				// 注意：不在这里重置PostOnlyFailCount，因为订单可能立即被撤销
				// 撤销/拒绝诊断计数只在订单真正成交时重置

				logger.Debug("✅ [实时新增] 槽位价格: %s, %s订单, 订单价格: %s, 订单ID: %d, ClientOID: %s",
					formatPrice(price, spm.priceDecimals), side, formatPrice(ord.Price, spm.priceDecimals), ord.OrderID, ord.ClientOrderID)
			} else {
				// 🔍 秒成交场景：WebSocket已经处理了FILLED,跳过状态更新
				logger.Debug("🔍 [%s单秒成交] 槽位 %s 的订单已被WebSocket处理，跳过状态更新 (持仓: %.4f, SlotStatus: %s)",
					side, formatPrice(price, spm.priceDecimals), slot.PositionQty, slot.SlotStatus)
			}

			slot.mu.Unlock()
		}

		if len(acceptedOrdersToCancel) > 0 {
			orderIDs := make([]int64, 0, len(acceptedOrdersToCancel))
			for orderID := range acceptedOrdersToCancel {
				orderIDs = append(orderIDs, orderID)
			}
			sort.Slice(orderIDs, func(i, j int) bool { return orderIDs[i] < orderIDs[j] })
			if err := spm.executor.BatchCancelOrders(orderIDs); err != nil {
				acceptedOrderErrs = append(acceptedOrderErrs,
					fmt.Errorf("槽位前提失效后定向撤单失败，本地保持 CANCEL_REQUESTED: %w", err))
			}
		}
		placementErr = errors.Join(placementErr, errors.Join(acceptedOrderErrs...))

		if placementErr != nil {
			return fmt.Errorf("批量下单未完全成功（已确认 %d/%d 个）: %w",
				len(placedOrders), len(ordersToPlace), placementErr)
		}
	}

	return nil
}

// OnOrderUpdate 订单更新回调（异步订单同步流）
func (spm *SuperPositionManager) OnOrderUpdate(update OrderUpdate) {
	// 🔥 重构：完全依赖 ClientOrderID 解析
	// 订单流和 REST 回包对返佣前缀的保留方式可能不同。进入任何去重、终态
	// 进度或槽位身份判断前统一成程序自己的 ClientOrderID，避免同一订单被当成
	// 两个身份，也让 UNKNOWN reservation 能被订单流权威收敛。
	update.ClientOrderID = spm.canonicalClientOrderID(update.ClientOrderID)
	price, side, valid := spm.parseClientOrderID(update.ClientOrderID)

	if !valid {
		logger.Debug("⏳ [忽略] 无法识别的订单更新: ID=%d, ClientOID=%s", update.OrderID, update.ClientOrderID)
		return
	}

	slot := spm.getOrCreateSlot(price)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	// 完成成交后槽位会清空订单字段；该订单的任何后续重放都必须忽略，
	// 否则迟到的 PARTIALLY_FILLED/终态推送会重新绑定槽位并重复累计。
	if spm.wasFilledOrderRecorded(update) {
		logger.Debug("⏭️ [已完成订单更新被忽略] ID=%d, ClientOID=%s, Status=%s",
			update.OrderID, update.ClientOrderID, update.Status)
		return
	}

	terminalStatus := isTerminalOrderStatus(update.Status)
	terminalProgress, terminalSeen := spm.getTerminalOrderProgress(update)
	if terminalSeen {
		if !terminalStatus {
			logger.Debug("⏭️ [忽略终态后的乱序推送] ID=%d, ClientOID=%s, Status=%s",
				update.OrderID, update.ClientOrderID, update.Status)
			return
		}
		spm.applyTerminalOrderCorrection(slot, update, side, price, terminalProgress)
		return
	}

	// 校验：确保这个更新属于当前的订单 (防止旧订单的延迟推送干扰新订单)
	// 已记录终态的旧订单在上方走独立修正路径，不会重新绑定或清理当前订单。
	// 优先使用 ClientOrderID 匹配 (某些交易所如 Gate.io 的 OrderID 可能略有差异)
	if slot.ClientOID != "" &&
		spm.canonicalClientOrderID(slot.ClientOID) != update.ClientOrderID {
		// ClientOrderID 不匹配，忽略此更新
		logger.Info("⚠️ [订单更新被忽略] 槽位 %.2f: ClientOID不匹配 (槽位: %s, 推送: %s, OrderID: %d)",
			price, slot.ClientOID, update.ClientOrderID, update.OrderID)
		return
	}

	// 更新订单ID (如果是首个推送)
	if slot.OrderID == 0 {
		logger.Debug("📝 [首次设置OrderID] 槽位 %.2f: OrderID=%d, ClientOID=%s", price, update.OrderID, update.ClientOrderID)
		slot.OrderID = update.OrderID
		slot.ClientOID = update.ClientOrderID
		slot.OrderSide = side
	} else if slot.OrderID != update.OrderID {
		// OrderID 不一致但 ClientOrderID 匹配，更新 OrderID (Gate.io 批量下单可能出现此情况)
		logger.Debug("📝 [更新OrderID] 槽位 %.2f: %d -> %d (ClientOID: %s)", price, slot.OrderID, update.OrderID, update.ClientOrderID)
		slot.OrderID = update.OrderID
	}
	// REST 回包未知或订单流先到时，匹配的交易所事件就是 reservation 已被接受的
	// 权威证据。及时转为 LOCKED，避免 UNKNOWN 永久停在 PENDING。
	if slot.SlotStatus == SlotStatusPending &&
		spm.canonicalClientOrderID(slot.ClientOID) == spm.canonicalClientOrderID(update.ClientOrderID) &&
		slot.OrderSide == side && !isTerminalOrderStatus(update.Status) {
		slot.SlotStatus = SlotStatusLocked
	}

	// 处理状态转换
	switch update.Status {
	case "NEW":
		if slot.OrderStatus == OrderStatusPlaced || slot.OrderStatus == OrderStatusNotPlaced {
			slot.OrderStatus = OrderStatusConfirmed
		}

	case "PARTIALLY_FILLED", "FILLED":
		orderPrice := slot.OrderPrice
		_, stale := spm.applyOrderExecutionDelta(slot, update, side, price)
		if stale {
			return
		}

		if side == "BUY" {
			if update.Status == "FILLED" {
				slot.OrderStatus = OrderStatusNotPlaced // 重置订单状态
				slot.OrderID = 0
				slot.ClientOID = ""
				slot.OrderSide = "" // 🔥 清除订单方向，避免误判
				slot.OrderQuantity = 0
				slot.OrderFilledQty = 0

				slot.PositionStatus = PositionStatusFilled // 标记为有仓
				// 🔥 释放槽位锁：买单成交，允许后续挂卖单
				slot.SlotStatus = SlotStatusFree
				// 买单成交，重置撤销/拒绝诊断计数
				slot.PostOnlyFailCount = 0
				logger.Info("✅ [买单成交] 价格: %s, 持仓: %.4f, 槽位状态: %s -> %s, 订单状态: %s -> %s, SlotStatus: FREE",
					formatPrice(price, spm.priceDecimals), slot.PositionQty,
					PositionStatusEmpty, PositionStatusFilled,
					"FILLED", OrderStatusNotPlaced)
				logger.Debug("🔍 [买单成交后] 等待下次AdjustOrders调用时挂出卖单...")
			} else {
				slot.OrderStatus = OrderStatusPartiallyFilled
			}

		} else { // SELL
			if update.Status == "FILLED" {
				slot.OrderStatus = OrderStatusNotPlaced // 重置订单状态
				slot.OrderID = 0
				slot.ClientOID = ""
				slot.OrderSide = "" // 🔥 清除订单方向，避免误判
				slot.OrderQuantity = 0
				slot.OrderFilledQty = 0

				if slot.PositionQty < 0.000001 {
					slot.PositionStatus = PositionStatusEmpty // 标记为空仓
				}
				// 🔥 释放槽位锁：卖单成交，允许后续挂买单
				slot.SlotStatus = SlotStatusFree
				// 卖单成交，重置撤销/拒绝诊断计数
				slot.PostOnlyFailCount = 0
				logger.Info("✅ [卖单成交] 价格: %s, 剩余持仓: %.4f, 槽位状态: %s, 订单状态: %s, SlotStatus: FREE",
					formatPrice(price, spm.priceDecimals), slot.PositionQty, slot.PositionStatus, slot.OrderStatus)
			} else {
				slot.OrderStatus = OrderStatusPartiallyFilled
			}
		}

		if update.Status == OrderStatusFilled {
			realizedPNL := 0.0
			if side == "SELL" {
				realizedPNL = slot.orderAccumulatedPNL
			}
			spm.recordFilledOrder(update, side, orderPrice, price, realizedPNL)
			slot.orderReportedPNL = 0
			slot.orderAccumulatedPNL = 0
		}

	case "CANCELED", "EXPIRED", "REJECTED":
		_, _ = spm.applyOrderExecutionDelta(slot, update, side, price)
		logger.Info("⚠️ [订单%s] 价格: %s, 方向: %s, 原因: %s, 已成交: %.4f",
			update.Status, formatPrice(price, spm.priceDecimals), side, update.Status, slot.OrderFilledQty)

		// 🔥 核心修复：根据订单方向和成交情况处理槽位状态
		if side == "BUY" {
			// 买单被取消/拒绝
			if slot.PositionQty > 0 || slot.OrderFilledQty > 0 {
				// 部分成交后被取消：保留持仓，允许后续挂卖单
				logger.Info("💡 [买单部分成交后取消] 价格: %s, 持仓: %.4f, 转为有仓状态",
					formatPrice(price, spm.priceDecimals), slot.PositionQty)
				slot.PositionStatus = PositionStatusFilled
				slot.SlotStatus = SlotStatusFree // 允许挂卖单
			} else {
				// 完全未成交被取消：重置为空槽位
				logger.Info("🔄 [买单未成交取消] 价格: %s, 重置槽位为空闲",
					formatPrice(price, spm.priceDecimals))
				slot.PositionStatus = PositionStatusEmpty
				slot.SlotStatus = SlotStatusFree // 允许重新挂买单
			}
		} else if side == "SELL" {
			// 卖单被取消/拒绝：应该还持有币，保持持仓状态
			if slot.PositionQty > 0 {
				// 记录撤销/拒绝次数供诊断；主动撤单、过期与 PostOnly 拒绝都可能进入此分支。
				if !terminalSeen {
					slot.PostOnlyFailCount++
				}
				logger.Info("🔄 [卖单取消] 价格: %s, 保持持仓状态: %.4f, 等待重挂, 撤销/拒绝计数: %d",
					formatPrice(price, spm.priceDecimals), slot.PositionQty, slot.PostOnlyFailCount)
				slot.PositionStatus = PositionStatusFilled
				slot.SlotStatus = SlotStatusFree // 允许重新挂卖单
			} else {
				// 异常情况：卖单取消但没有持仓，重置为空
				logger.Warn("⚠️ [异常] 卖单取消但无持仓，价格: %s, 重置为空",
					formatPrice(price, spm.priceDecimals))
				slot.PositionStatus = PositionStatusEmpty
				slot.SlotStatus = SlotStatusFree
			}
		}

		spm.rememberTerminalOrderProgress(
			update,
			slot.OrderFilledQty,
			slot.orderReportedPNL,
			slot.orderAccumulatedPNL,
		)

		// 清空订单信息
		slot.OrderStatus = OrderStatusCanceled
		slot.OrderID = 0
		slot.ClientOID = ""
		slot.OrderQuantity = 0
		slot.OrderFilledQty = 0
		slot.orderReportedPNL = 0
		slot.orderAccumulatedPNL = 0
		// 保留 OrderSide 用于日志调试
	}
}

func isTerminalOrderStatus(status string) bool {
	switch status {
	case "CANCELED", "EXPIRED", "REJECTED":
		return true
	default:
		return false
	}
}

// applyOrderExecutionDelta 将交易所累计成交量转换成本地增量。
// 所有可能携带最终累计量的状态都必须走这里，包括撤销、过期和拒绝。
func (spm *SuperPositionManager) applyOrderExecutionDelta(slot *InventorySlot, update OrderUpdate, side string, slotPrice float64) (float64, bool) {
	if math.IsNaN(update.ExecutedQty) || math.IsInf(update.ExecutedQty, 0) || update.ExecutedQty < 0 {
		logger.Warn("⚠️ [忽略非法累计成交] 槽位 %s: 推送 %.12f, 状态 %s",
			formatPrice(slotPrice, spm.priceDecimals), update.ExecutedQty, update.Status)
		return 0, true
	}
	if update.ExecutedQty+fillQtyTolerance < slot.OrderFilledQty {
		logger.Warn("⚠️ [忽略乱序成交] 槽位 %s: 已成交 %.12f, 推送 %.12f, 状态 %s",
			formatPrice(slotPrice, spm.priceDecimals), slot.OrderFilledQty, update.ExecutedQty, update.Status)
		return 0, true
	}

	deltaQty := update.ExecutedQty - slot.OrderFilledQty
	if deltaQty < fillQtyTolerance {
		deltaQty = 0
	}
	if update.ExecutedQty > slot.OrderFilledQty {
		slot.OrderFilledQty = update.ExecutedQty
	}

	if side == "BUY" {
		if deltaQty > 0 {
			slot.PositionQty += deltaQty
			oldTotal := spm.totalBuyQty.Load().(float64)
			spm.totalBuyQty.Store(oldTotal + deltaQty)
		}
		return deltaQty, false
	}

	if deltaQty > 0 {
		slot.PositionQty -= deltaQty
		if slot.PositionQty < 0 {
			slot.PositionQty = 0
		}
		oldTotal := spm.totalSellQty.Load().(float64)
		spm.totalSellQty.Store(oldTotal + deltaQty)
	}
	spm.applySellRealizedPNL(slot, update, deltaQty)
	return deltaQty, false
}

func (spm *SuperPositionManager) getTerminalOrderProgress(update OrderUpdate) (terminalOrderProgress, bool) {
	key := filledOrderKey(update)
	if key == "" {
		return terminalOrderProgress{}, false
	}
	spm.terminalOrdersMu.RLock()
	progress, exists := spm.terminalOrders[key]
	spm.terminalOrdersMu.RUnlock()
	return progress, exists
}

func (spm *SuperPositionManager) rememberTerminalOrderProgress(update OrderUpdate, executedQty, reportedPNL, accountedPNL float64) {
	spm.storeTerminalOrderProgress(update, terminalOrderProgress{
		ExecutedQty:  executedQty,
		ReportedPNL:  reportedPNL,
		AccountedPNL: accountedPNL,
		UpdateTime:   update.UpdateTime,
	})
}

func (spm *SuperPositionManager) storeTerminalOrderProgress(update OrderUpdate, next terminalOrderProgress) {
	key := filledOrderKey(update)
	if key == "" {
		return
	}
	spm.terminalOrdersMu.Lock()
	_, exists := spm.terminalOrders[key]
	spm.terminalOrders[key] = next
	if !exists {
		spm.terminalOrderKeys = append(spm.terminalOrderKeys, key)
		if len(spm.terminalOrderKeys) > maxTerminalOrders {
			oldest := spm.terminalOrderKeys[0]
			spm.terminalOrderKeys = spm.terminalOrderKeys[1:]
			delete(spm.terminalOrders, oldest)
		}
	}
	spm.terminalOrdersMu.Unlock()
}

// applyTerminalOrderCorrection 只补记已终结旧订单的新增成交和可证明为更新版本的累计 PNL。
// 它绝不触碰当前槽位绑定的订单身份、OrderStatus 或 SlotStatus，避免旧推送干扰新订单。
func (spm *SuperPositionManager) applyTerminalOrderCorrection(
	slot *InventorySlot,
	update OrderUpdate,
	side string,
	slotPrice float64,
	progress terminalOrderProgress,
) {
	if math.IsNaN(update.ExecutedQty) || math.IsInf(update.ExecutedQty, 0) || update.ExecutedQty < 0 {
		logger.Warn("⚠️ [忽略非法终态累计成交] 槽位 %s: 推送 %.12f, 状态 %s",
			formatPrice(slotPrice, spm.priceDecimals), update.ExecutedQty, update.Status)
		return
	}
	if update.ExecutedQty+fillQtyTolerance < progress.ExecutedQty {
		logger.Debug("⏭️ [忽略回退的终态推送] ID=%d, ClientOID=%s, 已记录=%.12f, 推送=%.12f",
			update.OrderID, update.ClientOrderID, progress.ExecutedQty, update.ExecutedQty)
		return
	}

	deltaQty := update.ExecutedQty - progress.ExecutedQty
	if deltaQty < fillQtyTolerance {
		deltaQty = 0
	}

	// 数量不变时，只接受有严格事件版本的累计 PNL 修正。UpdateTime 缺失或
	// 乱序时无法证明新旧关系，因此宁可忽略，也不能让累计盈亏来回回退。
	hasCumulativePNL := side == "SELL" && !update.RealizedPNLIncremental && update.RealizedPNL != 0
	pnlChanged := hasCumulativePNL && math.Abs(update.RealizedPNL-progress.ReportedPNL) > fillQtyTolerance
	acceptPNLCorrection := pnlChanged && deltaQty == 0 &&
		progress.UpdateTime > 0 && update.UpdateTime > progress.UpdateTime
	if deltaQty == 0 && !acceptPNLCorrection {
		logger.Debug("⏭️ [重复终态被忽略] ID=%d, ClientOID=%s, Status=%s",
			update.OrderID, update.ClientOrderID, update.Status)
		return
	}

	if side == "BUY" {
		if deltaQty > 0 {
			slot.PositionQty += deltaQty
			oldTotal := spm.totalBuyQty.Load().(float64)
			spm.totalBuyQty.Store(oldTotal + deltaQty)
			slot.PositionStatus = PositionStatusFilled
		}
	} else {
		if deltaQty > 0 {
			slot.PositionQty -= deltaQty
			if slot.PositionQty < 0 {
				slot.PositionQty = 0
			}
			oldTotal := spm.totalSellQty.Load().(float64)
			spm.totalSellQty.Store(oldTotal + deltaQty)
		}

		pnlDelta := 0.0
		source := "成交价差"
		if update.RealizedPNLIncremental {
			// 同数量的增量 PNL 无法去重，上方已经拒绝；这里只处理新增成交。
			pnlDelta = update.RealizedPNL
			if pnlDelta != 0 {
				source = "成交推送"
			}
		} else if update.RealizedPNL != 0 {
			// 交易所累计值替换本地已经入账的回退/增量合计，只补二者差额。
			pnlDelta = update.RealizedPNL - progress.AccountedPNL
			progress.ReportedPNL = update.RealizedPNL
			source = "成交推送"
		}
		if pnlDelta == 0 && deltaQty > 0 && !hasCumulativePNL {
			sellPx := update.AvgPrice
			if sellPx <= 0 {
				sellPx = update.Price
			}
			if sellPx > 0 && slotPrice > 0 {
				pnlDelta = deltaQty * (sellPx - slotPrice)
			}
		}
		progress.AccountedPNL += pnlDelta
		if hasCumulativePNL {
			// 即使差额为零，权威累计值也定义了本订单最终已入账水位。
			progress.AccountedPNL = update.RealizedPNL
		}
		if pnlDelta != 0 {
			total := spm.addRealizedPNL(pnlDelta)
			logger.Info("💵 [终态盈亏修正] 价格: %s, 本笔: %.6f, 累计: %.6f (%s)",
				formatPrice(slotPrice, spm.priceDecimals), pnlDelta, total, source)
		}

		if slot.PositionQty < 0.000001 {
			slot.PositionStatus = PositionStatusEmpty
		} else {
			slot.PositionStatus = PositionStatusFilled
		}
	}

	if update.ExecutedQty > progress.ExecutedQty {
		progress.ExecutedQty = update.ExecutedQty
	}
	if update.UpdateTime > progress.UpdateTime {
		progress.UpdateTime = update.UpdateTime
	}
	spm.storeTerminalOrderProgress(update, progress)
}

func filledOrderKey(update OrderUpdate) string {
	if update.ClientOrderID != "" {
		return "client:" + update.ClientOrderID
	}
	if update.OrderID != 0 {
		return fmt.Sprintf("order:%d", update.OrderID)
	}
	return ""
}

func (spm *SuperPositionManager) wasFilledOrderRecorded(update OrderUpdate) bool {
	key := filledOrderKey(update)
	if key == "" {
		return false
	}
	spm.filledOrdersMu.RLock()
	_, exists := spm.filledOrderKeys[key]
	spm.filledOrdersMu.RUnlock()
	return exists
}

func (spm *SuperPositionManager) recordFilledOrder(update OrderUpdate, side string, orderPrice, slotPrice, realizedPNL float64) {
	key := filledOrderKey(update)
	if key == "" {
		return
	}

	price := update.AvgPrice
	if price <= 0 {
		price = update.Price
	}
	if price <= 0 {
		price = orderPrice
	}
	if price <= 0 {
		price = slotPrice
	}

	symbol := update.Symbol
	if symbol == "" && spm.config != nil {
		symbol = spm.config.Trading.Symbol
	}
	filledAt := time.Now()
	if update.UpdateTime > 0 {
		filledAt = timestampToTime(update.UpdateTime)
	}

	record := FilledOrderRecord{
		OrderID:       update.OrderID,
		ClientOrderID: update.ClientOrderID,
		Symbol:        symbol,
		Side:          side,
		Price:         price,
		Quantity:      update.ExecutedQty,
		FilledAt:      filledAt,
		RealizedPNL:   realizedPNL,
	}

	spm.filledOrdersMu.Lock()
	defer spm.filledOrdersMu.Unlock()
	if _, exists := spm.filledOrderKeys[key]; exists {
		return
	}
	spm.filledOrderKeys[key] = struct{}{}
	spm.filledOrders = append(spm.filledOrders, record)
	spm.filledOrderCount++
	if len(spm.filledOrders) > maxRecentFilledOrders {
		spm.filledOrders = append([]FilledOrderRecord(nil), spm.filledOrders[len(spm.filledOrders)-maxRecentFilledOrders:]...)
	}
	spm.addHourlyFillLocked(record, time.Now())
}

func startOfLocalHour(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	local := t.In(time.Local)
	return time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, local.Location())
}

func (spm *SuperPositionManager) addHourlyFillLocked(record FilledOrderRecord, now time.Time) {
	if spm.filledHourly == nil {
		spm.filledHourly = make(map[int64]*hourlyFillAcc)
	}
	hour := startOfLocalHour(record.FilledAt)
	if hour.IsZero() {
		hour = startOfLocalHour(now)
	}
	key := hour.Unix()
	acc := spm.filledHourly[key]
	if acc == nil {
		acc = &hourlyFillAcc{}
		spm.filledHourly[key] = acc
	}
	if record.Side == "SELL" {
		acc.sell++
		acc.sellQty += record.Quantity
		acc.pnl += record.RealizedPNL
	} else {
		acc.buy++
		acc.buyQty += record.Quantity
	}
	spm.pruneHourlyFillsLocked(now)
}

func (spm *SuperPositionManager) pruneHourlyFillsLocked(now time.Time) {
	oldest := startOfLocalHour(now).Add(-time.Duration(hourlyFillHours-1) * time.Hour).Unix()
	for hour := range spm.filledHourly {
		if hour < oldest {
			delete(spm.filledHourly, hour)
		}
	}
}

func (spm *SuperPositionManager) hourlyFillSnapshotLocked(now time.Time) []HourlyFillBucket {
	end := startOfLocalHour(now)
	start := end.Add(-time.Duration(hourlyFillHours-1) * time.Hour)
	buckets := make([]HourlyFillBucket, 0, hourlyFillHours)
	for hour := start; !hour.After(end); hour = hour.Add(time.Hour) {
		bucket := HourlyFillBucket{Hour: hour}
		if acc := spm.filledHourly[hour.Unix()]; acc != nil {
			bucket.Buy = acc.buy
			bucket.Sell = acc.sell
			bucket.BuyQty = acc.buyQty
			bucket.SellQty = acc.sellQty
			bucket.Pnl = acc.pnl
		}
		buckets = append(buckets, bucket)
	}
	return buckets
}

func timestampToTime(timestamp int64) time.Time {
	switch {
	case timestamp >= 1_000_000_000_000_000_000:
		return time.Unix(0, timestamp)
	case timestamp >= 1_000_000_000_000_000:
		return time.UnixMicro(timestamp)
	case timestamp >= 1_000_000_000_000:
		return time.UnixMilli(timestamp)
	default:
		return time.Unix(timestamp, 0)
	}
}

func (spm *SuperPositionManager) applySellRealizedPNL(slot *InventorySlot, update OrderUpdate, deltaQty float64) {
	var delta float64
	source := "成交价差"
	hasCumulativePNL := !update.RealizedPNLIncremental && update.RealizedPNL != 0
	if update.RealizedPNLIncremental {
		// 增量盈亏属于本笔成交；重复推送没有新增成交量时不得再次累加。
		if deltaQty <= 0 {
			return
		}
		delta = update.RealizedPNL
		if delta != 0 {
			source = "成交推送"
		}
	} else if hasCumulativePNL {
		// 累计值是该订单的权威总额。以本地实际入账值（包括价差回退）为
		// 基线补差，避免先回退 0.10、后累计 0.11 时最终变成 0.21。
		delta = update.RealizedPNL - slot.orderAccumulatedPNL
		slot.orderReportedPNL = update.RealizedPNL
		source = "成交推送"
	}
	if delta == 0 && deltaQty > 0 && !hasCumulativePNL {
		sellPx := update.AvgPrice
		if sellPx <= 0 {
			sellPx = update.Price
		}
		if sellPx > 0 && slot.Price > 0 {
			delta = deltaQty * (sellPx - slot.Price)
		}
	}
	slot.orderAccumulatedPNL += delta
	if delta == 0 {
		return
	}
	total := spm.addRealizedPNL(delta)
	logger.Info("💵 [已实现盈亏] 价格: %s, 本笔: %.6f, 累计: %.6f (%s)",
		formatPrice(slot.Price, spm.priceDecimals), delta, total, source)
}

func (spm *SuperPositionManager) addRealizedPNL(delta float64) float64 {
	spm.pnlMu.Lock()
	defer spm.pnlMu.Unlock()
	old, _ := spm.realizedPNL.Load().(float64)
	next := old + delta
	spm.realizedPNL.Store(next)
	return next
}

// GetRealizedPNL 卖单成交累计的已实现盈亏
func (spm *SuperPositionManager) GetRealizedPNL() float64 {
	if v := spm.realizedPNL.Load(); v != nil {
		if f, ok := v.(float64); ok {
			return f
		}
	}
	return 0
}

// getOrCreateSlot 获取或创建槽位
func (spm *SuperPositionManager) getOrCreateSlot(price float64) *InventorySlot {
	if slot, exists := spm.slots.Load(price); exists {
		return slot.(*InventorySlot)
	}

	// 创建新槽位
	slot := &InventorySlot{
		Price:          price,
		PositionStatus: PositionStatusEmpty,
		PositionQty:    0,
		OrderStatus:    OrderStatusNotPlaced,
		SlotStatus:     SlotStatusFree, // 🔥 初始化为FREE状态
	}
	spm.slots.Store(price, slot)
	return slot
}

// findNearestGridPrice 找到最近的网格价格
// 根据当前价格动态计算最近的网格对齐价格
func (spm *SuperPositionManager) findNearestGridPrice(currentPrice float64) float64 {
	// 计算当前价格相对于锚点的偏移量
	offset := currentPrice - spm.anchorPrice
	// 计算离当前价格最近的网格间隔数（四舍五入）
	intervals := math.Round(offset / spm.config.Trading.PriceInterval)
	// 计算最近的网格价格
	gridPrice := spm.anchorPrice + intervals*spm.config.Trading.PriceInterval
	// 使用检测到的价格精度进行舍入
	return roundPrice(gridPrice, spm.priceDecimals)
}

// calculateSlotPrices 计算槽位价格列表（统一的网格计算方法）
// 这个方法确保初始化和实时调整计算出完全相同的槽位价格
// 参数：
//   - gridPrice: 网格价格（使用锚点价格）
//   - count: 需要计算的槽位数量
//   - direction: 方向，"down"表示向下（买单），"up"表示向上（卖单）
//
// 返回：槽位价格列表，从网格价格开始，按价格间隔递减或递增，使用检测到的价格精度
func (spm *SuperPositionManager) calculateSlotPrices(gridPrice float64, count int, direction string) []float64 {
	var prices []float64
	priceInterval := spm.config.Trading.PriceInterval

	for i := 0; i < count; i++ {
		var price float64
		if direction == "down" {
			// 向下：网格价格 - i * 间隔
			price = gridPrice - float64(i)*priceInterval
		} else {
			// 向上：网格价格 + i * 间隔
			price = gridPrice + float64(i)*priceInterval
		}
		// 使用检测到的价格精度进行舍入
		price = roundPrice(price, spm.priceDecimals)
		prices = append(prices, price)
	}

	return prices
}

// ===== IPositionManager 接口实现（供 safety.Reconciler 使用）=====
// 注意：以下方法是 safety/reconciler.go 中 IPositionManager 接口的实现，
// 被 Reconciler 对账器调用，不可删除或修改签名

// SlotData 槽位数据结构（用于传递给外部）
type SlotData struct {
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

// IterateSlots 遍历所有槽位（封装 sync.Map.Range）
// 注意：为了避免类型冲突，这里使用 interface{} 返回槽位数据
// 调用者需要将其转换为具体的槽位信息
func (spm *SuperPositionManager) IterateSlots(fn func(price float64, slot interface{}) bool) {
	spm.slots.Range(func(key, value interface{}) bool {
		price := key.(float64)
		slot := value.(*InventorySlot)
		slot.mu.RLock()
		defer slot.mu.RUnlock()

		// 构造槽位数据
		data := SlotData{
			Price:          price,
			PositionStatus: slot.PositionStatus,
			PositionQty:    slot.PositionQty,
			OrderID:        slot.OrderID,
			ClientOID:      slot.ClientOID,
			OrderSide:      slot.OrderSide,
			OrderStatus:    slot.OrderStatus,
			OrderPrice:     slot.OrderPrice,
			OrderQuantity:  slot.OrderQuantity,
			OrderFilledQty: slot.OrderFilledQty,
			SlotStatus:     slot.SlotStatus,
			OrderCreatedAt: slot.OrderCreatedAt,
		}

		// 返回槽位数据
		return fn(price, data)
	})
}

// GetTotalBuyQty 获取累计买入数量（IPositionManager 接口方法，供 Reconciler 使用）
func (spm *SuperPositionManager) GetTotalBuyQty() float64 {
	return spm.totalBuyQty.Load().(float64)
}

// GetTotalSellQty 获取累计卖出数量（IPositionManager 接口方法，供 Reconciler 使用）
func (spm *SuperPositionManager) GetTotalSellQty() float64 {
	return spm.totalSellQty.Load().(float64)
}

// GetReconcileCount 获取对账次数（IPositionManager 接口方法，供 Reconciler 使用）
func (spm *SuperPositionManager) GetReconcileCount() int64 {
	return spm.reconcileCount.Load()
}

// IncrementReconcileCount 增加对账次数（IPositionManager 接口方法，供 Reconciler 使用）
func (spm *SuperPositionManager) IncrementReconcileCount() {
	spm.reconcileCount.Add(1)
}

// UpdateLastReconcileTime 更新最后对账时间（IPositionManager 接口方法，供 Reconciler 使用）
func (spm *SuperPositionManager) UpdateLastReconcileTime(t time.Time) {
	spm.lastReconcileTime.Store(t)
}

// GetSymbol 获取交易符号
func (spm *SuperPositionManager) GetSymbol() string {
	return spm.config.Trading.Symbol
}

// GetPriceInterval 获取价格间隔
func (spm *SuperPositionManager) GetPriceInterval() float64 {
	return spm.config.Trading.PriceInterval
}

// ===== 订单清理功能已迁移到 safety.OrderCleaner =====
// StartOrderCleanup 和 cleanupOrders 方法已移至 safety/order_cleaner.go

// CompareAndSwapSlotOrderStatus 仅在槽位仍属于指定订单、且订单状态与清理快照
// 一致时更新状态。撤单 REST 返回前，订单流可能已经清槽或让同价新订单接管；
// 这些情况下必须拒绝旧清理结果的迟到反写。
func (spm *SuperPositionManager) CompareAndSwapSlotOrderStatus(
	price float64,
	orderID int64,
	clientOID, expectedStatus, newStatus string,
) bool {
	value, ok := spm.slots.Load(price)
	if !ok {
		return false
	}
	slot := value.(*InventorySlot)
	slot.mu.Lock()
	defer slot.mu.Unlock()

	if slot.OrderID != orderID || slot.OrderStatus != expectedStatus {
		return false
	}
	if clientOID != "" && spm.canonicalClientOrderID(slot.ClientOID) != spm.canonicalClientOrderID(clientOID) {
		return false
	}

	slot.OrderStatus = newStatus
	return true
}

// CancelAllBuyOrders 撤销并确认所有受管买单。只有交易所终态已经回读、且本地槽位
// 已应用最终累计成交量后才返回成功；任何不确定状态都会向调用方传播错误。
func (spm *SuperPositionManager) CancelAllBuyOrders() error {
	ctx, cancel := context.WithTimeout(context.Background(), buyCancelTimeout)
	defer cancel()
	return spm.cancelAllBuyOrders(ctx)
}

// ===== 对账功能已迁移到 safety.Reconciler =====
// StartReconciliation 和 Reconcile 方法已移至 safety/reconciler.go
// SetPauseChecker 也已移至 Reconciler

// CancelAllOrders 撤销所有订单（退出时使用）
// 委托给交易所适配器实现具体逻辑
func (spm *SuperPositionManager) CancelAllOrders() {
	ctx := context.Background()
	if err := spm.exchange.CancelAllOrders(ctx, spm.config.Trading.Symbol); err != nil {
		logger.Error("❌ [%s] 撤销所有订单失败: %v", spm.exchange.GetName(), err)
	} else {
		logger.Info("✅ [%s] 撤销所有订单完成", spm.exchange.GetName())
	}
}

// getExistingPosition 获取当前持仓数量（容错处理）
func (spm *SuperPositionManager) getExistingPosition() float64 {
	ctx := context.Background()
	positionsInterface, err := spm.exchange.GetPositions(ctx, spm.config.Trading.Symbol)
	if err != nil || positionsInterface == nil {
		logger.Debug("🔍 [持仓恢复] 无法获取持仓信息: %v", err)
		return 0
	}

	// 尝试类型断言 - 假设返回的是包含 Size 字段的结构体切片
	// 我们使用反射来安全地提取持仓数量
	switch positions := positionsInterface.(type) {
	case []*PositionInfo:
		// PositionInfo 切片（简化版）
		for _, pos := range positions {
			if pos != nil && pos.Symbol == spm.config.Trading.Symbol {
				logger.Debug("🔍 [持仓恢复] 找到持仓 (PositionInfo): %.4f", pos.Size)
				return pos.Size
			}
		}
	case []interface{}:
		// 通用接口数组 - 尝试解析为持仓结构
		for _, pos := range positions {
			// 尝试直接类型断言为 PositionInfo
			if posInfo, ok := pos.(*PositionInfo); ok {
				if posInfo.Symbol == spm.config.Trading.Symbol {
					logger.Debug("🔍 [持仓恢复] 找到持仓 (interface->PositionInfo): %.4f", posInfo.Size)
					return posInfo.Size
				}
			}
			// 尝试解析为 map
			if posMap, ok := pos.(map[string]interface{}); ok {
				if symbol, ok := posMap["Symbol"].(string); ok && symbol == spm.config.Trading.Symbol {
					if size, ok := posMap["Size"].(float64); ok {
						logger.Debug("🔍 [持仓恢复] 找到持仓 (map): %.4f", size)
						return size
					}
				}
			}
		}
	default:
		// 其他情况：使用反射尝试提取 Size 字段
		logger.Debug("🔍 [持仓恢复] 持仓类型: %T，尝试使用反射提取", positionsInterface)
		// 尝试使用反射处理未知类型
		// 注意：实际上 exchange 返回的是 []*exchange.Position，但因为接口返回 interface{}，所以需要特殊处理
		return 0
	}

	logger.Debug("🔍 [持仓恢复] 未找到匹配的持仓")
	return 0
}

// initializeSellSlotsFromPosition 从现有持仓初始化卖单槽位（用于程序重启后恢复状态）
func (spm *SuperPositionManager) initializeSellSlotsFromPosition(totalPosition float64) {
	if totalPosition <= 0 {
		return
	}

	// 1. 计算每单的理论数量（基于当前价格）
	// 使用锚点价格作为参考价格，使用从交易所获取的数量精度

	// 每单的理论数量 = 目标金额 / 锚点价格
	theoryQtyPerSlot := spm.config.Trading.OrderQuantity / spm.anchorPrice
	theoryQtyPerSlot = roundPrice(theoryQtyPerSlot, spm.quantityDecimals)

	// 2. 计算需要创建的总槽位数
	totalSlotsNeeded := int(math.Ceil(totalPosition / theoryQtyPerSlot))
	logger.Info("🔄 [持仓恢复] 总持仓: %.4f，每单理论数量: %.4f，需要创建 %d 个槽位",
		totalPosition, theoryQtyPerSlot, totalSlotsNeeded)

	// 3. 确定窗口大小（前N个槽位可以立即挂卖单）
	sellWindowSize := spm.config.Trading.SellWindowSize
	if sellWindowSize <= 0 {
		sellWindowSize = spm.config.Trading.BuyWindowSize // 默认与买单窗口相同
	}

	// 4. 计算卖单槽位价格（从锚点价格 + 价格间隔开始）
	// 卖单最低价 = 锚点价格 + 价格间隔（避免与买单最高价冲突）
	sellStartPrice := spm.anchorPrice + spm.config.Trading.PriceInterval
	sellPrices := spm.calculateSlotPrices(sellStartPrice, totalSlotsNeeded, "up")

	logger.Info("🔄 [持仓恢复] 从价格 %s 向上创建 %d 个槽位（前 %d 个将挂卖单）",
		formatPrice(sellStartPrice, spm.priceDecimals), totalSlotsNeeded, sellWindowSize)

	// 5. 先计算所有槽位的理论数量总和（固定金额模式）
	var totalTheoryQty float64
	theoryQtys := make([]float64, len(sellPrices))
	for i, price := range sellPrices {
		theoryQty := spm.config.Trading.OrderQuantity / price
		theoryQty = roundPrice(theoryQty, spm.quantityDecimals)
		theoryQtys[i] = theoryQty
		totalTheoryQty += theoryQty
	}

	logger.Debug("🔍 [持仓恢复] 理论总数量: %.4f, 实际持仓: %.4f, 比例: %.4f",
		totalTheoryQty, totalPosition, totalPosition/totalTheoryQty)

	// 6. 按比例分配实际持仓到各个槽位
	var allocatedQty float64

	for i, price := range sellPrices {
		// 计算这个槽位应该分配的数量
		var slotQty float64
		if i == len(sellPrices)-1 {
			// 最后一个槽位：分配剩余的所有持仓（避免舍入误差）
			slotQty = totalPosition - allocatedQty
		} else {
			// 按比例分配：实际数量 = 理论数量 × (总持仓 / 理论总数量)
			slotQty = theoryQtys[i] * (totalPosition / totalTheoryQty)
			slotQty = roundPrice(slotQty, spm.quantityDecimals)

			// 确保不超过剩余持仓
			remaining := totalPosition - allocatedQty
			if slotQty > remaining {
				slotQty = remaining
			}
		}

		if slotQty <= 0 {
			logger.Warn("⚠️ [持仓恢复] 槽位 %s 分配数量过小 %.4f，跳过（已分配: %.4f / 总计: %.4f）",
				formatPrice(price, spm.priceDecimals), slotQty, allocatedQty, totalPosition)
			continue
		}

		// 7. 创建或更新槽位
		slot := spm.getOrCreateSlot(price)
		slot.mu.Lock()

		// 设置为有仓状态
		slot.PositionStatus = PositionStatusFilled
		slot.PositionQty = slotQty

		// 清空订单信息，但设置方向为SELL（因为这是恢复的持仓，将来要挂卖单）
		slot.OrderID = 0
		slot.OrderStatus = OrderStatusNotPlaced
		slot.OrderSide = "SELL" // 恢复持仓时标记为卖单方向
		slot.ClientOID = ""
		slot.OrderFilledQty = 0

		slot.mu.Unlock()

		allocatedQty += slotQty

		// 日志标记：是否在窗口内（只打印前10个和最后10个）
		if i < 10 || i >= len(sellPrices)-10 {
			inWindow := ""
			if i < sellWindowSize {
				inWindow = " [可挂单]"
			} else {
				inWindow = " [暂不挂单]"
			}
			logger.Info("✅ [持仓恢复] 槽位 %s: 分配持仓 %.4f (理论: %.4f)%s",
				formatPrice(price, spm.priceDecimals), slotQty, theoryQtys[i], inWindow)
		} else if i == 10 {
			logger.Info("... （省略中间 %d 个槽位）", len(sellPrices)-20)
		}
	}

	logger.Info("✅ [持仓恢复] 完成持仓恢复，总持仓: %.4f，已分配: %.4f，差异: %.4f",
		totalPosition, allocatedQty, totalPosition-allocatedQty)

	// 8. 提示用户后续会自动下卖单
	logger.Info("💡 [持仓恢复] 前 %d 个槽位的卖单将在价格调整时自动创建", sellWindowSize)
	logger.Info("💡 [持仓恢复] 其余 %d 个槽位保持有仓状态，价格接近时自动挂单", totalSlotsNeeded-sellWindowSize)
}

// ===== 状态打印功能 =====

// PrintPositions 打印持仓状态（由 main.go 定期调用和退出时调用）
// 注意：该方法内部使用 totalBuyQty 和 totalSellQty 统计数据
func (spm *SuperPositionManager) PrintPositions() {
	logger.Info("📊 ===== 当前持仓 =====")
	total := 0.0
	count := 0

	// 收集所有持仓数据
	type positionInfo struct {
		Price       float64
		Qty         float64
		OrderStatus string
		OrderSide   string
		OrderID     int64
		SlotStatus  string
	}
	var positions []positionInfo

	spm.slots.Range(func(key, value interface{}) bool {
		price := key.(float64)
		slot := value.(*InventorySlot)
		slot.mu.RLock()
		if slot.PositionStatus == PositionStatusFilled && slot.PositionQty > 0.001 {
			positions = append(positions, positionInfo{
				Price:       price,
				Qty:         slot.PositionQty,
				OrderStatus: slot.OrderStatus,
				OrderSide:   slot.OrderSide,
				OrderID:     slot.OrderID,
				SlotStatus:  slot.SlotStatus,
			})
			total += slot.PositionQty
			count++
		}
		slot.mu.RUnlock()
		return true
	})

	// 按价格从高到低排序
	sort.Slice(positions, func(i, j int) bool {
		return positions[i].Price > positions[j].Price
	})

	// 从交易所接口获取基础币种（支持U本位和币本位合约）
	baseCurrency := spm.exchange.GetBaseAsset()

	// 打印持仓（从高到低）
	for _, pos := range positions {
		statusIcon := "🟢" // 有持仓
		priceStr := formatPrice(pos.Price, spm.priceDecimals)
		positionDesc := fmt.Sprintf("持仓: %.4f %s", pos.Qty, baseCurrency)

		orderInfo := ""
		if pos.OrderStatus != OrderStatusNotPlaced && pos.OrderStatus != "" {
			orderInfo = fmt.Sprintf(", 订单: %s/%s (ID:%d)", pos.OrderSide, pos.OrderStatus, pos.OrderID)
		}

		// 🔥 总是显示槽位状态,便于调试
		slotStatusInfo := ""
		if pos.SlotStatus != "" {
			slotStatusInfo = fmt.Sprintf(" [槽位:%s]", pos.SlotStatus)
		} else {
			slotStatusInfo = " [槽位:空]"
		}

		logger.Info("  %s %s: %s%s%s",
			statusIcon, priceStr, positionDesc, orderInfo, slotStatusInfo)
	}

	logger.Info("持仓统计: %.4f %s (%d 个槽位)", total, baseCurrency, count)
	totalBuyQty := spm.totalBuyQty.Load().(float64)
	totalSellQty := spm.totalSellQty.Load().(float64)
	// 预计盈利 = 累计卖出数量 × 价格间距（每笔盈利 = 价格间距 × 数量）
	estimatedProfit := totalSellQty * spm.config.Trading.PriceInterval
	logger.Info("累计买入: %.2f, 累计卖出: %.2f, 预计盈利: %.2f U, 已实现盈亏: %.4f U",
		totalBuyQty, totalSellQty, estimatedProfit, spm.GetRealizedPNL())

	// === 新增：打印买单窗口详细信息 ===
	logger.Info("🔍 ===== 买单窗口状态 =====")

	// 获取最后的市场价格
	lastPrice, ok := spm.lastMarketPrice.Load().(float64)
	if !ok || lastPrice <= 0 {
		lastPrice = spm.anchorPrice // 如果没有更新过，使用锚点价格
	}
	logger.Info("当前市场价格: %s", formatPrice(lastPrice, spm.priceDecimals))

	// 收集所有槽位信息（包括买单和空槽位）
	type slotInfo struct {
		Price          float64
		PositionStatus string
		PositionQty    float64
		OrderSide      string
		OrderStatus    string
		OrderID        int64
		ClientOID      string
		SlotStatus     string
	}
	var allSlots []slotInfo

	spm.slots.Range(func(key, value interface{}) bool {
		price := key.(float64)
		slot := value.(*InventorySlot)
		slot.mu.RLock()
		allSlots = append(allSlots, slotInfo{
			Price:          price,
			PositionStatus: slot.PositionStatus,
			PositionQty:    slot.PositionQty,
			OrderSide:      slot.OrderSide,
			OrderStatus:    slot.OrderStatus,
			OrderID:        slot.OrderID,
			ClientOID:      slot.ClientOID,
			SlotStatus:     slot.SlotStatus,
		})
		slot.mu.RUnlock()
		return true
	})

	// 按价格从高到低排序
	sort.Slice(allSlots, func(i, j int) bool {
		return allSlots[i].Price > allSlots[j].Price
	})

	// 找到最接近当前价格的网格价格
	currentGridPrice := spm.findNearestGridPrice(lastPrice)
	logger.Info("当前网格价格: %s", formatPrice(currentGridPrice, spm.priceDecimals))

	// 计算买单窗口范围（当前网格价格下方的买单窗口）
	buyWindowSize := spm.config.Trading.BuyWindowSize
	buyWindowPrices := spm.calculateSlotPrices(currentGridPrice, buyWindowSize, "down")

	// 创建价格查找表
	buyWindowPriceMap := make(map[string]bool)
	for _, p := range buyWindowPrices {
		buyWindowPriceMap[formatPrice(p, spm.priceDecimals)] = true
	}

	// 打印买单窗口内的所有槽位
	logger.Info("买单窗口大小: %d 个槽位 (当前网格价格下方)", buyWindowSize)
	buyOrderCount := 0
	emptySlotCount := 0
	filledSlotCount := 0

	for _, slot := range allSlots {
		priceStr := formatPrice(slot.Price, spm.priceDecimals)
		// 只打印买单窗口内的槽位
		if buyWindowPriceMap[priceStr] {
			statusIcon := "⚪" // 空槽位
			statusDesc := ""

			if slot.PositionStatus == PositionStatusFilled {
				statusIcon = "🟢" // 有持仓
				statusDesc = fmt.Sprintf("持仓: %.4f %s", slot.PositionQty, baseCurrency)
				filledSlotCount++
			} else {
				statusDesc = "无持仓"
				emptySlotCount++
			}

			orderInfo := ""
			if slot.OrderStatus != OrderStatusNotPlaced && slot.OrderStatus != "" {
				orderInfo = fmt.Sprintf(", 订单: %s/%s (ID:%d)", slot.OrderSide, slot.OrderStatus, slot.OrderID)
				if slot.OrderSide == "BUY" && (slot.OrderStatus == OrderStatusPlaced ||
					slot.OrderStatus == OrderStatusConfirmed ||
					slot.OrderStatus == OrderStatusPartiallyFilled) {
					buyOrderCount++
				}
			}

			// 🔥 总是显示槽位状态,便于调试
			slotStatusInfo := ""
			if slot.SlotStatus != "" {
				slotStatusInfo = fmt.Sprintf(" [槽位:%s]", slot.SlotStatus)
			} else {
				slotStatusInfo = " [槽位:空]"
			}

			logger.Info("  %s %s: %s%s%s",
				statusIcon, priceStr, statusDesc, orderInfo, slotStatusInfo)
		}
	}

	logger.Info("窗口统计: %d 个买单活跃, %d 个已持仓, %d 个空槽位",
		buyOrderCount, filledSlotCount, emptySlotCount)
	logger.Info("==========================")
}

// 辅助函数
// roundPrice 价格四舍五入
func roundPrice(price float64, decimals int) float64 {
	multiplier := math.Pow(10, float64(decimals))
	return math.Round(price*multiplier) / multiplier
}

// formatPrice 格式化价格字符串，使用指定的小数位数
func formatPrice(price float64, decimals int) string {
	return fmt.Sprintf("%.*f", decimals, price)
}
