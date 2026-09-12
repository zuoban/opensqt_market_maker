package config

import (
	"fmt"
	"math"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// TradingConfig 交易参数配置。
type TradingConfig struct {
	Symbol                   string  `yaml:"symbol"`
	PriceInterval            float64 `yaml:"price_interval"`
	OrderQuantity            float64 `yaml:"order_quantity"`           // 每单购买金额（USDT/USDC）
	MinOrderValue            float64 `yaml:"min_order_value"`          // 最小订单价值（USDT），默认6U，小于此值不挂单
	MaxMarginUsagePercent    float64 `yaml:"max_margin_usage_percent"` // 最大允许占用保证金百分比，默认100
	BuyWindowSize            int     `yaml:"buy_window_size"`
	SellWindowSize           int     `yaml:"sell_window_size"` // 卖单窗口大小
	ReconcileInterval        int     `yaml:"reconcile_interval"`
	OrderCleanupThreshold    int     `yaml:"order_cleanup_threshold"`      // 订单清理上限（默认100）
	CleanupBatchSize         int     `yaml:"cleanup_batch_size"`           // 清理批次大小（默认10）
	MarginLockDurationSec    int     `yaml:"margin_lock_duration_seconds"` // 保证金锁定时间（秒，默认10）
	PositionSafetyCheck      int     `yaml:"position_safety_check"`        // 持仓安全性检查（默认100，最少能向下持有多少仓）
	maxMarginUsagePercentSet bool
	// 注意：price_decimals 和 quantity_decimals 已废弃，现在从交易所自动获取
}

// ExecutionConfig 控制严格 Maker 的盘口保护、拒单退避、被动补格和跳格合并。
// 所有模式最终仍由订单执行边界强制 PostOnly，不提供 Taker 降级。
type ExecutionConfig struct {
	MakerGuardTicks           int     `yaml:"maker_guard_ticks"`
	QuoteStaleMS              int     `yaml:"quote_stale_ms"`
	PostOnlyRetryMinMS        int     `yaml:"post_only_retry_min_ms"`
	PostOnlyRetryMaxMS        int     `yaml:"post_only_retry_max_ms"`
	PostOnlyRetryBurst        int     `yaml:"post_only_retry_burst"`
	CatchUpMode               string  `yaml:"catch_up_mode"`
	MaxActiveCatchUpSlots     int     `yaml:"max_active_catch_up_slots"`
	MaxCatchUpSlotsPerAdjust  int     `yaml:"max_catch_up_slots_per_adjust"`
	MaxCatchUpDistanceRatio   float64 `yaml:"max_catch_up_distance_ratio"`
	NearTouchSingleOrderRatio float64 `yaml:"near_touch_single_order_ratio"`
	MaxGapStackSlots          int     `yaml:"max_gap_stack_slots"` // 额外合并的已跨过漏买格数，0 关闭（默认）
}

// UnmarshalYAML 记录 max_margin_usage_percent 是否由用户显式配置，
// 从而区分“未配置时使用默认值”和“显式配置 0（非法）”。
func (c *TradingConfig) UnmarshalYAML(value *yaml.Node) error {
	type plain TradingConfig
	var decoded plain
	if err := value.Decode(&decoded); err != nil {
		return err
	}
	*c = TradingConfig(decoded)
	// 用指针再次解码存在性，兼容 YAML merge/anchor 中显式配置的 0。
	// 直接遍历当前 mapping 的 key 会漏掉通过 `<<` 合并进来的字段。
	var presence struct {
		MaxMarginUsagePercent *float64 `yaml:"max_margin_usage_percent"`
	}
	if err := value.Decode(&presence); err != nil {
		return err
	}
	c.maxMarginUsagePercentSet = presence.MaxMarginUsagePercent != nil
	return nil
}

// Config 做市商系统配置
type Config struct {
	// Binance 配置。继续使用 exchanges.binance 路径，兼容现有配置文件。
	Exchanges struct {
		Binance BinanceConfig `yaml:"binance"`
	} `yaml:"exchanges"`

	Trading TradingConfig `yaml:"trading"`

	Execution ExecutionConfig `yaml:"execution"`

	System struct {
		LogLevel     string `yaml:"log_level"`
		CancelOnExit bool   `yaml:"cancel_on_exit"`
	} `yaml:"system"`

	// 主动安全风控配置
	RiskControl struct {
		Enabled           bool     `yaml:"enabled"`            // 是否启用风控，默认true
		MonitorSymbols    []string `yaml:"monitor_symbols"`    // 监控币种，如 ["BTCUSDT", "ETHUSDT"]
		Interval          string   `yaml:"interval"`           // K线周期，如 "1m", "3m", "5m"
		VolumeMultiplier  float64  `yaml:"volume_multiplier"`  // 成交量倍数阈值，默认3.0
		AverageWindow     int      `yaml:"average_window"`     // 移动平均窗口大小，默认20
		RecoveryThreshold int      `yaml:"recovery_threshold"` // 恢复交易所需的正常币种数量，默认3
	} `yaml:"risk_control"`

	// 时间间隔配置（单位：秒，除非特别说明）
	Timing struct {
		// WebSocket相关
		WebSocketReconnectDelay    int `yaml:"websocket_reconnect_delay"`     // WebSocket断线重连等待时间（秒，默认5）
		WebSocketWriteWait         int `yaml:"websocket_write_wait"`          // WebSocket写入等待时间（秒，默认10）
		WebSocketPongWait          int `yaml:"websocket_pong_wait"`           // WebSocket PONG等待时间（秒，默认60）
		WebSocketPingInterval      int `yaml:"websocket_ping_interval"`       // WebSocket PING间隔（秒，默认20）
		ListenKeyKeepAliveInterval int `yaml:"listen_key_keepalive_interval"` // listenKey保活间隔（分钟，默认30）

		// 价格监控相关
		PriceSendInterval int `yaml:"price_send_interval"` // 定期发送价格的间隔（毫秒，默认50）

		// 订单执行相关
		RateLimitRetryDelay  int `yaml:"rate_limit_retry_delay"` // 速率限制重试等待时间（秒，默认1）
		OrderRetryDelay      int `yaml:"order_retry_delay"`      // 已弃用，仅为兼容旧配置保留；未分类错误按 UNKNOWN 处理
		PricePollInterval    int `yaml:"price_poll_interval"`    // 等待获取价格的轮询间隔（毫秒，默认500）
		StatusPrintInterval  int `yaml:"status_print_interval"`  // 定期打印状态的间隔（分钟，默认1）
		OrderCleanupInterval int `yaml:"order_cleanup_interval"` // 订单清理检查间隔（秒，默认60）
	} `yaml:"timing"`

	// 监控面板（只读，嵌在主进程）
	Dashboard DashboardConfig `yaml:"dashboard"`

	Telegram TelegramConfig `yaml:"telegram"`
}

// TelegramConfig 可选的订单全成交通知，默认关闭。
type TelegramConfig struct {
	Enabled  bool   `yaml:"enabled"`
	BotToken string `yaml:"bot_token" json:"-"`
	ChatID   string `yaml:"chat_id" json:"-"`
}

// DashboardConfig 本地监控面板配置
type DashboardConfig struct {
	// Enabled 缺省视为开启。显式写 false 可关闭。
	Enabled           *bool  `yaml:"enabled"`
	Listen            string `yaml:"listen"`              // 默认 127.0.0.1:8787
	Token             string `yaml:"token"`               // 非空则 /api 与 /ws 需要 token
	PushIntervalMS    int    `yaml:"push_interval_ms"`    // WebSocket 推送间隔，默认400，最小200
	AccountRefreshSec int    `yaml:"account_refresh_sec"` // 账户缓存刷新秒数，默认10
}

// BinanceConfig Binance API 与手续费配置。
type BinanceConfig struct {
	APIKey    string  `yaml:"api_key"`
	SecretKey string  `yaml:"secret_key"`
	FeeRate   float64 `yaml:"fee_rate"` // 手续费率（例如 0.0002 表示 0.02%）
}

// LoadConfig 加载 YAML（文件可不存在），再用非空 OPENSQT_* 环境变量覆盖。
func LoadConfig(configPath string) (*Config, error) {
	if err := LoadDotEnv(); err != nil {
		return nil, err
	}

	var cfg Config
	if configPath != "" {
		data, err := os.ReadFile(configPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("读取配置文件失败: %v", err)
			}
		} else if err := yaml.Unmarshal(data, &cfg); err != nil {
			return nil, fmt.Errorf("解析配置文件失败: %v", err)
		}
	}

	if err := applyEnvOverrides(&cfg); err != nil {
		return nil, fmt.Errorf("环境变量覆盖失败: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置验证失败: %v", err)
	}

	return &cfg, nil
}

// Validate 验证配置
func (c *Config) Validate() error {
	// 项目仅支持 Binance，配置路径固定为 exchanges.binance。
	exchangeCfg := c.Exchanges.Binance
	if exchangeCfg.APIKey == "" || exchangeCfg.SecretKey == "" {
		return fmt.Errorf("Binance API 配置不完整 (exchanges.binance)")
	}

	// 验证手续费率配置
	if exchangeCfg.FeeRate < 0 {
		return fmt.Errorf("Binance 手续费率不能为负数")
	}

	if c.Trading.Symbol == "" {
		return fmt.Errorf("交易对不能为空")
	}
	if c.Trading.OrderQuantity <= 0 {
		return fmt.Errorf("订单金额必须大于0")
	}
	if c.Trading.BuyWindowSize <= 0 {
		return fmt.Errorf("买单窗口大小必须大于0")
	}
	if c.Trading.SellWindowSize <= 0 {
		c.Trading.SellWindowSize = c.Trading.BuyWindowSize // 默认与买单窗口相同
	}
	if c.Trading.CleanupBatchSize <= 0 {
		c.Trading.CleanupBatchSize = 10 // 默认10
	}
	if c.Trading.OrderCleanupThreshold <= 0 {
		c.Trading.OrderCleanupThreshold = 100
	}
	if c.Trading.BuyWindowSize+c.Trading.SellWindowSize >= c.Trading.OrderCleanupThreshold {
		return fmt.Errorf("买单窗口(%d)+卖单窗口(%d) 必须小于 order_cleanup_threshold(%d)，否则会反复补单再清理。密网格请先加大阈值（未配置时默认 100），不要把窗口开到阈值以上；旧配置若仍是 50，请同步加大该值",
			c.Trading.BuyWindowSize, c.Trading.SellWindowSize, c.Trading.OrderCleanupThreshold)
	}
	// 注意：price_decimals 和 quantity_decimals 已从配置中移除，现在从交易所自动获取
	if c.Trading.MinOrderValue <= 0 {
		c.Trading.MinOrderValue = 20.0 // 默认6U (币安通常最小5U)
	}
	if !c.Trading.maxMarginUsagePercentSet && c.Trading.MaxMarginUsagePercent == 0 {
		c.Trading.MaxMarginUsagePercent = 100
	}
	if math.IsNaN(c.Trading.MaxMarginUsagePercent) || math.IsInf(c.Trading.MaxMarginUsagePercent, 0) ||
		c.Trading.MaxMarginUsagePercent <= 0 || c.Trading.MaxMarginUsagePercent > 100 {
		return fmt.Errorf("最大保证金占用比例必须大于0且不超过100 (trading.max_margin_usage_percent)")
	}

	// 设置默认时间间隔
	if c.Timing.WebSocketReconnectDelay <= 0 {
		c.Timing.WebSocketReconnectDelay = 5 // 默认5秒
	}
	if c.Timing.WebSocketWriteWait <= 0 {
		c.Timing.WebSocketWriteWait = 10 // 默认10秒
	}
	if c.Timing.WebSocketPongWait <= 0 {
		c.Timing.WebSocketPongWait = 60 // 默认60秒
	}
	if c.Timing.WebSocketPingInterval <= 0 {
		c.Timing.WebSocketPingInterval = 20 // 默认20秒
	}
	if c.Timing.ListenKeyKeepAliveInterval <= 0 {
		c.Timing.ListenKeyKeepAliveInterval = 30 // 默认30分钟
	}
	if c.Timing.PriceSendInterval <= 0 {
		c.Timing.PriceSendInterval = 50 // 默认50毫秒
	}
	if c.Timing.RateLimitRetryDelay <= 0 {
		c.Timing.RateLimitRetryDelay = 1 // 默认1秒
	}
	if c.Timing.OrderRetryDelay <= 0 {
		c.Timing.OrderRetryDelay = 500 // 兼容旧配置；执行器不再盲重试未分类错误
	}
	if c.Timing.PricePollInterval <= 0 {
		c.Timing.PricePollInterval = 500 // 默认500毫秒
	}
	if c.Timing.StatusPrintInterval <= 0 {
		c.Timing.StatusPrintInterval = 1 // 默认1分钟
	}
	if c.Timing.OrderCleanupInterval <= 0 {
		c.Timing.OrderCleanupInterval = 60 // 默认60秒
	}

	if c.Execution.MakerGuardTicks < 0 {
		return fmt.Errorf("execution.maker_guard_ticks 不能为负数")
	}
	if c.Execution.MakerGuardTicks == 0 {
		c.Execution.MakerGuardTicks = 2
	}
	if c.Execution.QuoteStaleMS < 0 {
		return fmt.Errorf("execution.quote_stale_ms 不能为负数")
	}
	if c.Execution.QuoteStaleMS == 0 {
		c.Execution.QuoteStaleMS = 30000
	}
	if c.Execution.PostOnlyRetryMinMS < 0 {
		return fmt.Errorf("execution.post_only_retry_min_ms 不能为负数")
	}
	if c.Execution.PostOnlyRetryMinMS == 0 {
		c.Execution.PostOnlyRetryMinMS = 50
	}
	if c.Execution.PostOnlyRetryMaxMS < 0 {
		return fmt.Errorf("execution.post_only_retry_max_ms 不能为负数")
	}
	if c.Execution.PostOnlyRetryMaxMS == 0 {
		c.Execution.PostOnlyRetryMaxMS = 500
	}
	if c.Execution.PostOnlyRetryMaxMS < c.Execution.PostOnlyRetryMinMS {
		return fmt.Errorf("execution.post_only_retry_max_ms 不能小于 post_only_retry_min_ms")
	}
	if c.Execution.PostOnlyRetryBurst < 0 {
		return fmt.Errorf("execution.post_only_retry_burst 不能为负数")
	}
	if c.Execution.PostOnlyRetryBurst == 0 {
		c.Execution.PostOnlyRetryBurst = 5
	}
	c.Execution.CatchUpMode = strings.ToLower(strings.TrimSpace(c.Execution.CatchUpMode))
	if c.Execution.CatchUpMode == "" {
		c.Execution.CatchUpMode = "passive"
	}
	if c.Execution.CatchUpMode != "passive" && c.Execution.CatchUpMode != "exact_wait" {
		return fmt.Errorf("execution.catch_up_mode 仅支持 passive 或 exact_wait")
	}
	if c.Execution.MaxActiveCatchUpSlots < 0 {
		return fmt.Errorf("execution.max_active_catch_up_slots 不能为负数")
	}
	if c.Execution.MaxActiveCatchUpSlots == 0 {
		c.Execution.MaxActiveCatchUpSlots = 1
	}
	if c.Execution.MaxCatchUpSlotsPerAdjust < 0 {
		return fmt.Errorf("execution.max_catch_up_slots_per_adjust 不能为负数")
	}
	if c.Execution.MaxCatchUpSlotsPerAdjust == 0 {
		c.Execution.MaxCatchUpSlotsPerAdjust = 1
	}
	if math.IsNaN(c.Execution.MaxCatchUpDistanceRatio) || math.IsInf(c.Execution.MaxCatchUpDistanceRatio, 0) ||
		c.Execution.MaxCatchUpDistanceRatio < 0 {
		return fmt.Errorf("execution.max_catch_up_distance_ratio 必须在 (0, 1] 范围内")
	}
	if c.Execution.MaxCatchUpDistanceRatio == 0 {
		c.Execution.MaxCatchUpDistanceRatio = 0.5
	}
	if c.Execution.MaxCatchUpDistanceRatio > 1 {
		return fmt.Errorf("execution.max_catch_up_distance_ratio 必须在 (0, 1] 范围内")
	}
	if math.IsNaN(c.Execution.NearTouchSingleOrderRatio) || math.IsInf(c.Execution.NearTouchSingleOrderRatio, 0) ||
		c.Execution.NearTouchSingleOrderRatio < 0 {
		return fmt.Errorf("execution.near_touch_single_order_ratio 必须在 (0, 1] 范围内")
	}
	if c.Execution.NearTouchSingleOrderRatio == 0 {
		c.Execution.NearTouchSingleOrderRatio = 0.15
	}
	if c.Execution.NearTouchSingleOrderRatio > 1 {
		return fmt.Errorf("execution.near_touch_single_order_ratio 必须在 (0, 1] 范围内")
	}
	if c.Execution.MaxGapStackSlots < 0 {
		return fmt.Errorf("execution.max_gap_stack_slots 不能为负数")
	}
	if c.Execution.MaxGapStackSlots > 10 {
		return fmt.Errorf("execution.max_gap_stack_slots 不能大于 10")
	}

	// 验证风控配置并设置默认值
	if c.RiskControl.Interval == "" {
		c.RiskControl.Interval = "1m" // 默认1分钟
	}
	if c.RiskControl.VolumeMultiplier <= 0 {
		c.RiskControl.VolumeMultiplier = 3.0 // 默认3倍
	}
	if c.RiskControl.AverageWindow <= 0 {
		c.RiskControl.AverageWindow = 20 // 默认20根K线
	}
	if len(c.RiskControl.MonitorSymbols) == 0 {
		c.RiskControl.MonitorSymbols = []string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "XRPUSDT", "DOGEUSDT"}
	}

	// 验证恢复阈值配置
	monitorCount := len(c.RiskControl.MonitorSymbols)
	if c.RiskControl.RecoveryThreshold <= 0 {
		c.RiskControl.RecoveryThreshold = 3 // 默认3个币种
	} else if c.RiskControl.RecoveryThreshold < 1 {
		c.RiskControl.RecoveryThreshold = 1 // 最小1个
	} else if c.RiskControl.RecoveryThreshold > monitorCount {
		c.RiskControl.RecoveryThreshold = monitorCount // 最大为监控币种数量
	}

	if err := c.applyDashboardDefaults(); err != nil {
		return err
	}
	c.Telegram.BotToken = strings.TrimSpace(c.Telegram.BotToken)
	c.Telegram.ChatID = strings.TrimSpace(c.Telegram.ChatID)
	if c.Telegram.Enabled {
		if c.Telegram.BotToken == "" {
			return fmt.Errorf("启用 Telegram 通知时必须设置 telegram.bot_token")
		}
		if c.Telegram.ChatID == "" {
			return fmt.Errorf("启用 Telegram 通知时必须设置 telegram.chat_id")
		}
	}

	return nil
}

// DashboardEnabled 面板是否启用。配置缺省时默认开启。
func (c *Config) DashboardEnabled() bool {
	if c.Dashboard.Enabled == nil {
		return true
	}
	return *c.Dashboard.Enabled
}

func (c *Config) applyDashboardDefaults() error {
	if c.Dashboard.Listen == "" {
		c.Dashboard.Listen = "127.0.0.1:8787"
	}
	if _, _, err := net.SplitHostPort(c.Dashboard.Listen); err != nil {
		return fmt.Errorf("dashboard.listen 格式无效，应为 host:port: %v", err)
	}
	if c.Dashboard.PushIntervalMS <= 0 {
		c.Dashboard.PushIntervalMS = 400
	} else if c.Dashboard.PushIntervalMS < 200 {
		c.Dashboard.PushIntervalMS = 200
	}
	if c.Dashboard.AccountRefreshSec <= 0 {
		c.Dashboard.AccountRefreshSec = 10
	}
	return nil
}
