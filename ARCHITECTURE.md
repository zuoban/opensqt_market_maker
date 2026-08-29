# OpenSQT 做市商系统架构说明

> **版本**: v3.5.3
> **最近更新**: 2026-08-29
> **目的**: 说明当前运行架构、交易安全边界与扩展约束

---

## 📋 目录

1. [系统概述](#系统概述)
2. [核心设计原则](#核心设计原则)
3. [模块架构](#模块架构)
4. [数据流分析](#数据流分析)
5. [关键组件详解](#关键组件详解)
6. [接口与依赖关系](#接口与依赖关系)
7. [并发模型](#并发模型)
8. [风险控制机制](#风险控制机制)
---

## 系统概述

### 系统定位
OpenSQT 是一个 WebSocket 驱动的加密货币永续合约**单向做多网格做市系统**。每个网格使用固定报价货币金额，并以严格 PostOnly 限价单执行。

### 核心功能
- ✅ 多交易所支持（Binance、Bitget、Gate.io、Bybit、Backpack）
- ✅ 基于网格的自动做市策略
- ✅ WebSocket 实时价格和订单流
- ✅ 智能仓位管理（超级槽位系统）
- ✅ 主动风控监控（成交量异常检测）
- ✅ 订单清理与对账机制
- ✅ 持仓安全性检查

### 技术栈
- **语言**: Go 1.25
- **配置管理**: YAML
- **WebSocket**: gorilla/websocket
- **限流**: golang.org/x/time/rate
- **并发模型**: goroutine + channel + sync.Map

---

## 核心设计原则

### 1. 单一价格源原则
```
✅ 全局唯一的价格流（PriceMonitor）
✅ WebSocket 是唯一的价格来源（不使用 REST API 轮询）
✅ 所有组件通过 priceMonitor.GetLastPrice() 获取价格
❌ 禁止在其他地方独立启动价格流
```

**架构意义**:
- 避免价格不一致
- 减少 API 调用，防止触发限流
- 毫秒级系统无法容忍 REST API 延迟

### 2. 订单流优先原则
```
启动顺序:
1️⃣ 启动订单流（StartOrderStream）
2️⃣ 下单（PlaceOrder）
3️⃣ 避免错过成交推送
```

**反模式**:
```go
❌ 先下单，后启动订单流 → 可能错过成交推送
✅ 先启动订单流，再下单 → 确保成交推送不丢失
```

### 3. 固定金额模式
```
传统网格: 固定数量买入（如每次0.01 BTC）
OpenSQT: 固定金额买入（如每次30 USDT）
```

**优势**:
- 资金利用率更可控
- 适配不同价格区间
- 方便资金管理和风控

### 4. 槽位锁定机制
```
槽位状态:
- FREE: 空闲，可操作
- PENDING: 等待下单确认
- LOCKED: 已锁定，有活跃订单
```

**作用**:
- 防止并发重复下单
- 避免同一槽位重复买入/卖出
- 确保订单与持仓的一致性

---

## 模块架构

```
opensqt_platform/
├── main.go                    # 主程序入口，组件编排
│
├── config/                    # 配置管理
│   └── config.go              # YAML配置加载与验证
│
├── exchange/                  # 交易所抽象层（核心）
│   ├── interface.go           # IExchange 统一接口
│   ├── factory.go             # 工厂模式创建交易所实例
│   ├── types.go               # 通用数据结构
│   ├── wrapper_*.go           # 适配器（包装各交易所）
│   ├── binance/               # 币安实现
│   ├── bitget/                # Bitget实现
│   └── gate/                  # Gate.io实现
│
├── logger/                    # 日志系统
│   └── logger.go              # 文件日志 + 控制台日志
│
├── monitor/                   # 价格监控
│   └── price_monitor.go       # 全局唯一价格流
│
├── order/                     # 订单执行层
│   └── executor_adapter.go    # 订单执行器（限流+重试）
│
├── position/                  # 仓位管理（核心）
│   └── super_position_manager.go  # 超级槽位管理器
│
├── safety/                    # 安全与风控
│   ├── safety.go              # 启动前安全检查
│   ├── risk_monitor.go        # 主动风控（K线监控）
│   ├── reconciler.go          # 持仓对账
│   └── order_cleaner.go       # 订单清理
│
└── utils/                     # 工具函数
    └── orderid.go             # 自定义订单ID生成
```

---

## 数据流分析

### 启动流程
```
1. 加载配置（YAML + OPENSQT_* 环境变量）
   ↓
2. 创建交易所实例 (factory.go)
   └── Binance：同步服务器时间、加载合约过滤器、校验交易权限/单向持仓/空头持仓
   ↓
3. 启动价格监控 (PriceMonitor.Start)
   ├── 唯一价格 WebSocket
   └── 等待首个有效价格
   ↓
4. 启动前安全检查
   ├── Binance 查询账户真实 Maker 费率
   ├── 验证保证金币种余额与杠杆
   └── 验证网格间距扣除双边手续费后仍有利润
   ↓
5. 创建执行器与核心组件（新单门禁保持关闭）
   ↓
6. 启动订单流 (exchange.StartOrderStream)
   ├── 所有交易所必须完成私有订阅握手并进入 READY
   ├── 连接代际、订阅确认或消息校验异常会立即转为 DEGRADED
   └── 回调 → SuperPositionManager.OnOrderUpdate
   ↓
7. 初始化仓位管理器 (SuperPositionManager.Initialize)
   ├── 设置价格锚点
   └── 恢复持仓槽位；此时仍禁止下单
   ↓
8. 同步执行首次完整对账
   ↓
9. 同步启动主动风控
   ├── 加载足量已完结历史 K 线
   ├── 启动实时 K 线流
   └── 所有监控交易对收到实时数据后进入 READY
   ↓
10. 启动统一交易门禁 (tradingGateRuntime)
    ├── 订单流、风控、对账、价格新鲜度全部健康才放行
    ├── 首次放行立即执行 AdjustOrders
    └── 任一条件恶化立即停新单、撤买单，恢复前强制对账
    ↓
11. 启动订单清理、只读面板与状态日志
```

### 退出流程

```
SIGINT / SIGTERM
   ↓
永久关闭交易门禁并等待协调器退出
   ↓
cancel_on_exit=true ? 全撤并反复查询直至远端挂单为空 : 明确保留远端挂单
   ↓
Shutdown 订单执行器，取消后台上下文
   ↓
停止价格流、订单流、风控 K 线流和只读面板
```

### 价格流
```
Exchange WebSocket
    ↓
PriceMonitor.updatePrice()
    ↓
latestPriceChange (atomic.Value)
    ↓
periodicPriceSender (定期推送)
    ↓
priceChangeCh (channel)
    ↓
tradingGateRuntime 串行协调器
    ↓
联合健康检查（订单流 / 风控 / 对账 / 价格新鲜度）
    ├── ❌ 任一异常 → 停新单、撤买单、等待恢复对账
    └── ✅ 全部健康 → SuperPositionManager.AdjustOrders()
```

### 订单流
```
Exchange WebSocket (订单更新)
    ↓
各交易所适配器校验连接代际、订阅确认、交易对与订单字段
    ├── ❌ 解码或字段异常 → DEGRADED、重连、门禁关闭
    └── ✅ 有效 OpenSQT 订单
    ↓
main.go 回调函数
    ↓
反射提取字段 (解决匿名结构体问题)
    ↓
position.OrderUpdate
    ↓
SuperPositionManager.OnOrderUpdate()
    ├── 匹配槽位（严格通过 ClientOrderID）
    ├── 更新槽位状态
    ├── FILLED → 释放槽位，下一次 AdjustOrders 创建对向单
    └── CANCELED → 重置槽位
```

### 交易逻辑流
```
价格变化
    ↓
AdjustOrders(newPrice)
    ↓
遍历所有槽位
    ↓
┌─────────────────────────────────┐
│ 槽位类型判断                    │
├─────────────────────────────────┤
│ 1. 空槽位 (无订单，无持仓)      │
│    → 检查是否在买入窗口         │
│    → 下买单                     │
│                                 │
│ 2. 有买单 (等待成交)            │
│    → 检查是否超出窗口           │
│    → 撤单                       │
│                                 │
│ 3. 有持仓 (等待卖出)            │
│    → 检查是否有卖单             │
│    → 无卖单 → 下卖单            │
│    → 有卖单 → 检查价格          │
│                                 │
│ 4. 有卖单 (等待成交)            │
│    → 检查是否需要调价           │
│    → 撤单并重新下单             │
└─────────────────────────────────┘
```

---

## 关键组件详解

### 1. Exchange（交易所抽象层）

#### 设计模式
- **接口**: `IExchange` 统一所有交易所操作
- **工厂**: `NewExchange()` 根据配置创建实例
- **适配器**: `wrapper_*.go` 包装各交易所实现

#### 核心接口
```go
type IExchange interface {
    // 订单操作
    PlaceOrder(ctx, req) (*Order, error)
    BatchPlaceOrders(ctx, orders) ([]*Order, bool)
    CancelOrder(ctx, symbol, orderID) error
    BatchCancelOrders(ctx, symbol, orderIDs) error
    CancelAllOrders(ctx, symbol) error
    
    // 账户查询
    GetAccount(ctx) (*Account, error)
    GetPositions(ctx, symbol) ([]*Position, error)
    GetOpenOrders(ctx, symbol) ([]*Order, error)
    
    // WebSocket
    StartPriceStream(ctx, symbol, callback)
    StartOrderStream(ctx, callback)
    StartKlineStream(ctx, symbols, interval, callback)
    
    // 精度信息
    GetPriceDecimals() int
    GetPriceTickSize() float64
    GetQuantityDecimals() int
    GetBaseAsset() string
    GetQuoteAsset() string
}
```

#### Binance 安全边界

- 初始化时同步服务器时间，并拒绝非 `TRADING`、非 `PERPETUAL` 合约。
- 价格按 `tickSize` 量化（买价向下、卖价向上），数量按 `stepSize` 向下量化，同时校验 `minQty`、`maxQty` 与 `minNotional`。
- 仅允许单向持仓；卖单必须 `reduceOnly`，所有策略订单必须为 `LIMIT + GTX`。
- 503、超时、断连、异常 2xx、重复 ClientOrderID 和创建订单响应超限，都按原 ClientOrderID 查询确认；结果未知时禁止盲重试。
- 用户订单流握手后才进入 READY；listenKey 过期、保活失败、JSON/字段异常会立即 DEGRADED、重连并触发恢复对账。
- REST 响应在进入 SDK 前有大小上限（普通接口 1 MiB，`exchangeInfo` 8 MiB）。
- 全撤使用 Binance 原生接口，并查询确认远端挂单为空。

#### 实现层级
```
IExchange (接口)
    ↓
wrapper_binance.go (适配器)
    ↓
binance/adapter.go (交易所SDK)
    ↓
binance/websocket.go (WebSocket)
```

#### 关键挑战
1. **API差异**:
   - Binance: listenKey 订单流
   - Bitget: 私有订单WebSocket
   - Gate.io: 用户订单 WebSocket

2. **精度处理**:
   - Binance: 使用 `PRICE_FILTER.tickSize`
   - Bitget: 使用 `priceEndStep × 10^-pricePlace`
   - Gate.io: 使用 `order_price_round`
   - Bybit / Backpack: 使用合约元数据中的 `tickSize`
   - 真实 tick 缺失或非法时拒绝启动交易，不用展示小数位猜测

3. **批量操作**:
   - Bitget: 原生支持批量下单/撤单
   - Binance/Gate: 循环调用单个API

---

### 2. SuperPositionManager（仓位管理器）

#### 核心数据结构
```go
type InventorySlot struct {
    Price float64  // 槽位价格（精确到小数点后n位）
    
    // 持仓状态
    PositionStatus string  // EMPTY/FILLED
    PositionQty    float64
    
    // 订单状态
    OrderID        int64
    ClientOID      string
    OrderSide      string  // BUY/SELL
    OrderStatus    string  // NOT_PLACED/PLACED/FILLED/CANCELED
    OrderPrice     float64
    OrderFilledQty float64
    
    // 锁定机制
    SlotStatus string  // FREE/PENDING/LOCKED
    
    // 历史字段：卖单撤销/拒绝计数（仅用于诊断）
    PostOnlyFailCount int
    
    mu sync.RWMutex  // 槽位锁
}
```

#### 槽位生命周期
```
1. 初始化 (FREE, EMPTY, 无订单)
   ↓
2. 下买单 (PENDING → LOCKED, 等待成交)
   ↓
3. 买单成交 (FILLED, 有持仓)
   ↓
4. 下卖单 (LOCKED, 等待卖出)
   ↓
5. 卖单成交 (FREE, EMPTY, 回到初始状态)
```

#### 关键方法
```go
// 初始化（设置锚点价格，创建初始槽位）
Initialize(currentPrice, currentPriceStr) error

// 订单窗口调整（价格变化时调用）
AdjustOrders(newPrice) error

// 订单更新回调（WebSocket推送）
OnOrderUpdate(update OrderUpdate)

// 批量操作
CreateBuyOrders(prices []float64)
CreateSellOrders(prices []float64)
CancelAllBuyOrders()
```

#### 并发控制
1. **全局锁**: `mu sync.RWMutex`（保护 slots Map）
2. **槽位锁**: `slot.mu sync.RWMutex`（保护单个槽位）
3. **槽位状态**: `SlotStatus` 防止重复操作

#### 卖价唯一性

- 卖价下限为 `max(原网格盈利目标, Maker 安全价)`，然后按交易所真实 tick 向上量化。
- 每个持仓槽位仍保持一张独立 `ReduceOnly + PostOnly` 卖单，不合并数量。
- 已活跃、`PENDING/UNKNOWN` 和 `CANCEL_REQUESTED` 的 SELL 都持续占用其价格 tick；新卖单逐 tick 向上寻找未占用价位。
- 候选收集后会再次检查槽位订单身份，防止订单流的异步绑定被新 reservation 覆盖。

**典型操作流程**:
```go
// 下单前：
slot.mu.Lock()
if slot.SlotStatus != "FREE" {
    slot.mu.Unlock()
    return // 槽位已被占用
}
slot.SlotStatus = "PENDING"
slot.mu.Unlock()

// 下单后：
slot.mu.Lock()
slot.OrderID = orderID
slot.SlotStatus = "LOCKED"
slot.mu.Unlock()
```

---

### 3. PriceMonitor（价格监控）

#### 设计原则
- **全局唯一**: 整个系统只有一个实例
- **WebSocket Only**: 不使用 REST API 轮询
- **原子操作**: 使用 `atomic.Value` 存储价格

#### 核心字段
```go
type PriceMonitor struct {
    exchange      exchange.IExchange
    lastPrice     atomic.Value  // float64
    lastPriceStr  atomic.Value  // string（用于检测精度）
    
    priceChangeCh     chan PriceChange
    latestPriceChange atomic.Value  // *PriceChange
    
    isRunning atomic.Bool
    priceSendInterval time.Duration
}
```

#### 工作流程
```
1. StartPriceStream (启动 WebSocket)
   ↓
2. updatePrice (收到价格推送)
   ↓
3. lastPrice / lastPriceTime / latestPriceChange 原子更新
   ↓
4. periodicPriceSender (定期发送到 channel)
   ↓
5. tradingGateRuntime 读取价格事件并执行健康复检
   ↓
6. 门禁仍健康时调用 AdjustOrders
```

#### 价格精度检测
```go
// 通过价格字符串检测小数位数
priceStr := "123.4567"
parts := strings.Split(priceStr, ".")
if len(parts) == 2 {
    decimals := len(parts[1])  // 4位小数
}
```

---

### 4. Safety（安全与风控）

#### 核心安全机制

##### 4.1 启动前安全检查 (safety.go)
```go
CheckAccountSafety(
    ex, symbol, currentPrice,
    orderAmount, priceInterval, feeRate,
    requiredPositions, priceDecimals
)
```

**检查内容**:
1. 保证金币种余额充足性（Binance 严格使用合约 `marginAsset`）
2. 杠杆倍数限制（最高 10 倍）
3. 最大可持仓数计算
4. Binance 账户实际 Maker 费率查询
5. 网格价差 vs 双边手续费
6. Binance `canTrade`、单向持仓模式与非负持仓检查
7. 合约状态、`PERPETUAL` 类型及 PRICE_FILTER / LOT_SIZE / MIN_NOTIONAL 校验

**公式**:
```
最大可用保证金 = 账户余额 × 杠杆倍数
每仓成本 = 订单金额（固定）
最大持仓数 = 最大可用保证金 / 每仓成本
```

##### 4.2 主动风控监控 (risk_monitor.go)
```go
type RiskMonitor struct {
    cfg           *config.Config
    exchange      exchange.IExchange
    symbolDataMap map[string]*SymbolData  // K线缓存
    triggered     bool                    // 是否触发风控
}
```

**监控逻辑**:
1. 启动时为每个监控交易对加载足量已完结历史 K 线
2. 实时监听多个币种的 K 线（如 BTC、ETH），按时间戳去重更新
3. 计算成交量移动平均并检测异常倍数
4. 历史数据不足、实时流未握手或数据陈旧时保持 fail-closed
5. 触发风控 → 统一门禁停新单并撤销所有买单
6. 达到恢复阈值后仍需通过完整对账，才可恢复交易

**配置示例**:
```yaml
risk_control:
  enabled: true
  monitor_symbols: ["BTCUSDT", "ETHUSDT", "BNBUSDT", "SOLUSDT", "ADAUSDT"]
  interval: "1m"
  volume_multiplier: 3.0
  average_window: 20
  recovery_threshold: 3
```

##### 4.3 持仓对账 (reconciler.go)
```go
type Reconciler struct {
    cfg              *config.Config
    exchange         IExchange
    positionManager  *SuperPositionManager
    pauseChecker     func() bool  // 风控暂停检查
}
```

**对账内容**:
1. 交易所持仓 vs 本地持仓
2. 交易所策略挂单 ID 集合 vs 本地活跃订单 ID 集合
3. 下单提交中的 PENDING 窗口识别，避免瞬时误报
4. 任一不一致立即使交易门禁失效；不在对账器内静默改写槽位

**对账周期**:
- 默认每 60 秒（可配置）
- 启动放行前同步执行一次
- 订单流异常恢复后强制执行一次
- 风控期间仍继续对账，仅降低普通日志噪声

##### 4.4 订单清理 (order_cleaner.go)
```go
type OrderCleaner struct {
    cfg       *config.Config
    executor  *order.ExchangeOrderExecutor
    manager   *SuperPositionManager
}
```

**清理策略**:
1. 检查未完成订单数量
2. 超过阈值（默认100）时触发清理
3. 批量撤销最旧的订单（默认10个/批）
4. 重置对应槽位状态

##### 4.5 统一交易门禁 (trading_gate.go)

新单只有在以下条件同时成立时才允许提交：

- 订单流为 READY
- 风控数据已就绪且未触发
- 最近一次完整对账健康
- 唯一价格流已有正价格且更新时间未陈旧
- 保证金占用比例硬限制未锁存触发
- 每次真正进入交易所 `PlaceOrder` 前再次同步复检以上条件

任一条件恶化时，门禁会先取消执行器中的在途下单，再撤销全部买单并使对账失效。订单流恢复后必须重新核对真实持仓与挂单；只有对账通过才重新放行，并立即按最新价格调整订单窗口。

保证金占用超过 `trading.max_margin_usage_percent` 是不可自动恢复的硬门禁：程序立即锁存停止所有新单，并撤销当前交易对的全部挂单（买单和卖单）。首次全撤要求连续 3 次确认远端为空，随后在 15 秒的迟到受理窗口内每 2 秒查询一次，只有发现残单才再次定向全撤。限制锁存后的退出清理也使用固定 15 秒绝对截止窗口复查迟到挂单，不因重复发现残单而无限延长。即使之后占用比例回落，本次运行仍保持关闭；必须由人工确认账户状态后重启。首次账户读数已超限时，确认全撤后以 dormant 停单态继续运行，让只读面板保持可用。该行为与其它健康条件恢复后可经对账重新放行的流程不同。

交易所明确返回“订单未受理”的业务拒绝不属于健康条件恶化。此类失败只收敛对应槽位并进入短冷却；下单结果 UNKNOWN、无法安全归类的错误、订单状态无法收敛或上述健康条件异常时，才进入全局恢复路径。

---

### 5. Order Executor（订单执行器）

#### 核心功能
- **限流**: 25单/秒，突发30（可配置）
- **交易门禁**: 启动、异常恢复和停机期间可原子停止新单
- **有界请求**: 下单、撤单与重试等待均可由上下文取消
- **分类重试**: 仅重试明确可安全重试的失败
- **严格 PostOnly**: 所有网格单只做 Maker；明确会吃单时不做同价重试，等待槽位冷却后按最新价格重算，绝不降级为普通单
- **明确拒绝局部收敛**: 交易所已确认未受理的订单只释放对应槽位并短暂冷却，不关闭全局门禁、不撤销其它买单
- **UNKNOWN 传播**: 结果未知时不释放槽位、不换 ClientOrderID 盲目重下

#### 执行流程
```go
PlaceOrder(req *OrderRequest) (*Order, error) {
    requireNewOrderGateOpen()
    rateLimiter.Wait(ctx)
    req.PostOnly = true
    order, err := exchange.PlaceOrder(requestTimeoutCtx, req)
    if resultIsUnknown(err) {
        return nil, err // 上层关门并对账，禁止换 ID 重下
    }
    if definitelyRejected(err) {
        return nil, OrderRejectedError{Cause: err} // 局部释放，不触发全局撤买
    }
    if safelyRetryable(err) {
        waitWithContext(ctx, orderRetryDelay)
        // 使用同一槽位请求继续受控重试
    }
    return order, err
}
```

Binance 适配器还会在创建订单返回 HTTP 408/409/5xx、超时、断连、异常 2xx 或重复 ClientOrderID 后，以**原始 ClientOrderID**查询订单；明确不存在时才使用同一 ID 重试。其他交易所遇到 408/409 或重复 ClientOrderID 类响应时也必须保留为 UNKNOWN，不得当作“远端无订单”释放槽位。

#### 批量下单错误传播
```go
BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error) {
    // 返回已确认订单、保证金错误标记及保留分类的聚合错误。
    // position 层释放明确失败槽位、保留 UNKNOWN reservation；
    // 统一门禁把 UNKNOWN、无法安全归类和状态不一致视为恢复事件。
}
```

---

## 接口与依赖关系

### 依赖图
```
main.go
  ├── config (配置)
  ├── logger (日志)
  ├── exchange (交易所)
  │     └── binance/bitget/gate (实现)
  ├── monitor (价格监控)
  │     └── exchange.IExchange
  ├── order (订单执行)
  │     └── exchange.IExchange
  ├── position (仓位管理)
  │     ├── order.OrderExecutor (接口适配)
  │     └── IExchange (子集接口)
  └── safety (安全风控)
        ├── exchange.IExchange
        └── position.SuperPositionManager
```

### 循环依赖问题及解决方案

#### 问题1: position ↔ order
**问题**: position 需要调用 order 执行器，order 需要 position 的数据结构

**解决方案**: 在 position 包内定义接口
```go
// position/super_position_manager.go
type OrderExecutorInterface interface {
    PlaceOrder(req *OrderRequest) (*Order, error)
    BatchPlaceOrders(orders []*OrderRequest) ([]*Order, bool, error)
    BatchCancelOrders(orderIDs []int64) error
}

// main.go 中创建适配器
type exchangeExecutorAdapter struct {
    executor *order.ExchangeOrderExecutor
}

func (a *exchangeExecutorAdapter) PlaceOrder(req *position.OrderRequest) (*position.Order, error) {
    // 转换类型并调用
}
```

#### 问题2: position ↔ exchange
**问题**: position 需要查询交易所，但不能依赖 exchange 包（循环）

**解决方案**: 定义子集接口
```go
// position/super_position_manager.go
type IExchange interface {
    GetName() string
    GetPositions(ctx, symbol) (interface{}, error)
    GetOpenOrders(ctx, symbol) (interface{}, error)
    GetOrder(ctx, symbol, orderID) (interface{}, error)
    GetBaseAsset() string
    CancelAllOrders(ctx, symbol) error
}
```

#### 问题3: WebSocket 回调类型
**问题**: exchange 订单流回调需要传递 position.OrderUpdate，但会循环依赖

**解决方案**: 使用 `interface{}` + 反射
```go
// exchange/interface.go
StartOrderStream(ctx, callback func(interface{})) error

// main.go
ex.StartOrderStream(ctx, func(updateInterface interface{}) {
    v := reflect.ValueOf(updateInterface)
    // 反射提取字段
    posUpdate := position.OrderUpdate{
        OrderID:       getInt64Field("OrderID"),
        ClientOrderID: getStringField("ClientOrderID"),
        ...
    }
    superPositionManager.OnOrderUpdate(posUpdate)
})
```

---

## 并发模型

### Goroutine 列表
```
主要后台任务:
1. PriceMonitor                 # 唯一价格 WebSocket + 定期价格事件
2. Exchange OrderStream Manager # 私有订单流、连接代际、保活与重连
3. RiskMonitor                  # 风控 K 线流、陈旧检测与报告
4. tradingGateRuntime           # 健康观察、价格调整、周期/恢复对账
5. OrderCleaner                 # 定期清理旧订单
6. Dashboard                    # 只读监控（启用时）
7. 定期状态日志
```

### Channel 列表
```
1. priceChangeCh (monitor)
   类型: chan PriceChange
   容量: 10
   作用: 价格变化推送

2. tradingGateRuntime.wake
   类型: chan struct{}
   容量: 1
   作用: 合并健康状态变化，唤醒串行协调器

3. 交易所订单流生命周期 channel
   作用: 握手结果、连接完成、凭据失效和流错误通知

4. sigChan (main)
   类型: chan os.Signal
   容量: 1
   作用: 退出信号
```

### 同步原语
```
1. sync.Map (position/slots)
   作用: 槽位存储（支持并发读写）
   
2. sync.RWMutex (position/mu)
   作用: 全局槽位锁（保护 Map 操作）
   
3. sync.RWMutex (InventorySlot/mu)
   作用: 槽位级别锁（细粒度锁）
   
4. atomic.Value (price/lastPrice)
   作用: 无锁原子操作（价格读取）
   
5. atomic.Bool (price/isRunning)
   作用: 运行状态标志
```

### 并发安全性分析

#### 高风险操作
1. **槽位并发修改**
   - 风险: 价格变化协程 vs 订单更新回调
   - 保护: 槽位锁 + SlotStatus 状态机

2. **订单重复下单**
   - 风险: AdjustOrders 快速调用
   - 保护: SlotStatus = PENDING 锁定

3. **价格读取**
   - 风险: 多个协程同时读取
   - 保护: atomic.Value（无锁）

#### 死锁风险
```
❌ 反模式:
全局锁持有时 → 调用交易所API → 网络延迟 → 阻塞其他协程

✅ 正确做法:
释放锁 → 调用API → 重新获取锁 → 更新状态
```

---

## 风险控制机制

### 层次化风控

```
第1层: 启动前检查 (safety.CheckAccountSafety)
  ├── 余额充足性
  ├── 杠杆倍数限制
  ├── 实际 Maker 手续费率验证
  └── Binance 账户/合约规格校验

第2层: 主动风控 (RiskMonitor)
  ├── 历史/实时 K 线完整性与陈旧检测
  ├── K线成交量异常检测
  ├── 多币种联动监控
  └── 自动撤销买单

第3层: 统一交易门禁 (tradingGateRuntime)
  ├── 订单流 / 风控 / 对账 / 价格联合判定
  ├── 异常立即停止新单并撤买单
  └── 恢复前强制对账

第4层: 订单执行安全 (ExchangeOrderExecutor + BinanceAdapter)
  ├── 严格 PostOnly、限流与有界请求
  ├── UNKNOWN 结果按原 ClientOrderID 确认
  └── 不盲重试、不提前释放槽位

第5层: 订单清理 (OrderCleaner)
  ├── 未完成订单数量限制
  └── 定期清理旧订单

第6层: 持仓与挂单对账 (Reconciler)
  ├── 本地 vs 交易所持仓
  └── 本地 vs 交易所策略挂单 ID

第7层: 优雅停机
  ├── 先永久关闭新单门禁
  ├── cancel_on_exit=true 时全撤并确认远端为空
  └── 停执行器后再停止各 WebSocket

第8层: 人工干预
  ├── SIGINT/SIGTERM 优雅退出
  └── cancel_on_exit 配置
```

### 风控触发流程
```
成交量异常检测
    ↓
RiskMonitor.IsTriggered() = true
    ↓
tradingGateRuntime 健康观察器检测
    ↓
ExchangeOrderExecutor.StopNewOrders()
    ↓
superPositionManager.CancelAllBuyOrders()
    ↓
Reconciler.Invalidate()
    ↓
等待恢复条件满足
    ↓
RiskMonitor.IsTriggered() = false
    ↓
同步完整对账通过
    ↓
重新放行并立即 AdjustOrders
```

### 保证金管理

保证金管理包含两类不同机制：交易所返回保证金不足时使用短时冷却；账户保证金占用比例超过配置上限时使用本次运行永久锁存的硬门禁。

#### 保证金不足冷却

```go
// SuperPositionManager
insufficientMargin bool            # 标志位
marginLockUntil    time.Time       # 锁定时间
marginLockDuration time.Duration   # 锁定时长（默认10秒）

// 批量下单失败处理
if marginError {
    manager.insufficientMargin = true
    manager.marginLockUntil = time.Now().Add(marginLockDuration)
}

// 后续下单检查：保证金锁只暂停新增 BUY，已有买单保持，
// ReduceOnly SELL 仍可继续提交。
if manager.insufficientMargin && time.Now().Before(manager.marginLockUntil) {
    skipNewBuyOrders()
    continueReduceOnlySellOrders()
}
```

#### 保证金占用比例硬限制

配置项 `trading.max_margin_usage_percent` 的合法范围为 `(0,100]`，默认 `100`。占用比例使用账户同一保证金币种口径计算：

```text
保证金占用比例 = (保证金余额 - 可用余额) / 保证金余额 × 100%
```

其中已占用部分同时包含持仓占用保证金和未成交挂单冻结保证金。检查结果超过配置上限（`usage > limit`）时执行以下流程：

```text
锁存保证金硬限制
    ↓
停止该交易对所有新单
    ↓
撤销该交易对全部挂单（买单 + 卖单）
    ↓
短时复查迟到受理挂单
    ↓
本次运行保持锁存，不因占用比例回落自动恢复
    ↓
人工确认账户状态后重启
```

---

## 附录

### A. 配置文件示例
```yaml
app:
  current_exchange: "binance"

exchanges:
  binance:
    api_key: "your_api_key"
    secret_key: "your_secret_key"
    fee_rate: 0.0002
    
  bitget:
    api_key: "your_api_key"
    secret_key: "your_secret_key"
    passphrase: "your_passphrase"
    fee_rate: 0.0002

trading:
  symbol: "BTCUSDT"
  price_interval: 1.0
  order_quantity: 30.0
  min_order_value: 6.0
  max_margin_usage_percent: 80.0  # 范围 (0,100]，默认100
  buy_window_size: 100
  sell_window_size: 100
  reconcile_interval: 60
  order_cleanup_threshold: 100
  cleanup_batch_size: 10
  margin_lock_duration_seconds: 10
  position_safety_check: 100

system:
  log_level: "INFO"
  cancel_on_exit: true  # 默认全撤并确认；false 会显式保留远端挂单

risk_control:
  enabled: true
  monitor_symbols: ["BTCUSDT", "ETHUSDT", "BNBUSDT"]
  interval: "1m"
  volume_multiplier: 3.0
  average_window: 20
  recovery_threshold: 3

timing:
  websocket_reconnect_delay: 5
  websocket_write_wait: 10
  websocket_pong_wait: 60
  websocket_ping_interval: 20
  listen_key_keepalive_interval: 30
  price_send_interval: 50
  rate_limit_retry_delay: 1
  order_retry_delay: 500
  price_poll_interval: 500
  status_print_interval: 1
  order_cleanup_interval: 60
```

### B. 关键术语表

| 术语 | 英文 | 说明 |
|------|------|------|
| 槽位 | Slot | 每个价格点的仓位和订单管理单元 |
| 锚点价格 | Anchor Price | 系统初始化时的市场价格，作为网格基准 |
| 固定金额模式 | Fixed Amount Mode | 每笔交易投入固定金额（而非固定数量） |
| 价格精度 | Price Decimals | 价格小数位数（如BTC为2，ETH为2） |
| 价格步长 | Price Tick Size | 交易所允许的最小价格变动单位；可能为 `0.25` 等非 10 的幂 |
| 数量精度 | Quantity Decimals | 数量小数位数（如BTC为3，ETH为3） |
| PostOnly | Post Only Order | 只做Maker的订单（不立即成交） |
| ReduceOnly | Reduce Only Order | 只减仓订单（平仓单） |
| 保证金锁定 | Margin Lock | 批量下单失败后的冷却时间 |
| 对账 | Reconciliation | 本地状态与交易所状态的一致性检查 |

### C. API调用频率限制

#### Binance
```
REST API: 1200次/分钟
WebSocket: 10连接/IP
订单: 10单/秒（单交易对）
```

#### Bitget
```
REST API: 600次/分钟
批量下单: 20单/次
批量撤单: 20单/次
WebSocket: 无特殊限制
```

#### Gate.io
```
REST API: 900次/分钟
订单: 100单/秒
WebSocket: 无特殊限制
```

### D. 典型运行日志示例
```
2025-12-24 10:00:00 [INFO] 🚀 www.OpenSQT.com 做市商系统启动...
2025-12-24 10:00:00 [INFO] 📦 版本号: v3.3.1
2025-12-24 10:00:00 [INFO] ✅ 配置加载成功: 交易对=BTCUSDT, 窗口大小=100
2025-12-24 10:00:01 [INFO] ✅ 使用交易所: Binance
2025-12-24 10:00:02 [INFO] 🔗 启动 WebSocket 价格流...
2025-12-24 10:00:03 [INFO] 📊 当前价格: 42156.78
2025-12-24 10:00:04 [INFO] 🔒 ===== 开始持仓安全性检查 =====
2025-12-24 10:00:04 [INFO] 💰 账户余额: 3000.00 USDT
2025-12-24 10:00:04 [INFO] 📈 当前币价: 42156.78, 每笔金额: 30.00 USDT
2025-12-24 10:00:04 [INFO] ✅ 持仓安全性检查通过：可以安全持有至少 100 仓
2025-12-24 10:00:05 [INFO] ✅ [Binance] 订单流已启动
2025-12-24 10:00:06 [INFO] 📊 [SuperPositionManager] 初始化成功，锚点价格: 42156.78
2025-12-24 10:00:07 [INFO] 🛡️ 启动主动安全风控监控 (周期: 1m, 倍数: 3.0)
2025-12-24 10:00:08 [INFO] ✅ 系统启动完成，开始自动交易
```

---

## 总结

OpenSQT是一个设计合理但有改进空间的做市商系统。核心架构采用：
- **接口抽象** + **工厂模式**（多交易所）
- **WebSocket驱动** + **事件回调**（实时性）
- **细粒度锁** + **原子操作**（并发安全）
- **多层风控** + **状态机**（安全性）

**官网**:
- Website: www.OpenSQT.com
- Version: v3.5.3
- Last Updated: 2026-08-29
