package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// LoadDotEnv 读取 .env 到进程环境。已存在的环境变量不覆盖（容器注入优先）。
// 路径：OPENSQT_ENV_FILE，缺省为工作目录下的 .env。文件不存在则忽略。
func LoadDotEnv() error {
	path := strings.TrimSpace(os.Getenv("OPENSQT_ENV_FILE"))
	if path == "" {
		path = ".env"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			if strings.TrimSpace(os.Getenv("OPENSQT_ENV_FILE")) == "" {
				return nil
			}
			return fmt.Errorf("读取环境变量文件失败: %v", err)
		}
		return fmt.Errorf("读取环境变量文件失败: %v", err)
	}
	kv, err := parseDotEnv(string(data))
	if err != nil {
		return fmt.Errorf("解析 %s 失败: %v", path, err)
	}
	for key, value := range kv {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("设置环境变量 %s 失败: %v", key, err)
		}
	}
	return nil
}

func applyEnvOverrides(cfg *Config) error {
	if err := applyBinanceEnv(&cfg.Exchanges.Binance); err != nil {
		return err
	}

	t := &cfg.Trading
	if err := setString(&t.Symbol, "OPENSQT_TRADING_SYMBOL"); err != nil {
		return err
	}
	if err := setFloat(&t.PriceInterval, "OPENSQT_TRADING_PRICE_INTERVAL"); err != nil {
		return err
	}
	if err := setFloat(&t.OrderQuantity, "OPENSQT_TRADING_ORDER_QUANTITY"); err != nil {
		return err
	}
	if err := setFloat(&t.MinOrderValue, "OPENSQT_TRADING_MIN_ORDER_VALUE"); err != nil {
		return err
	}
	if _, ok := lookupEnv("OPENSQT_TRADING_MAX_MARGIN_USAGE_PERCENT"); ok {
		if err := setFloat(&t.MaxMarginUsagePercent, "OPENSQT_TRADING_MAX_MARGIN_USAGE_PERCENT"); err != nil {
			return err
		}
		t.maxMarginUsagePercentSet = true
	}
	if err := setInt(&t.BuyWindowSize, "OPENSQT_TRADING_BUY_WINDOW_SIZE"); err != nil {
		return err
	}
	if err := setInt(&t.SellWindowSize, "OPENSQT_TRADING_SELL_WINDOW_SIZE"); err != nil {
		return err
	}
	if err := setInt(&t.ReconcileInterval, "OPENSQT_TRADING_RECONCILE_INTERVAL"); err != nil {
		return err
	}
	if err := setInt(&t.OrderCleanupThreshold, "OPENSQT_TRADING_ORDER_CLEANUP_THRESHOLD"); err != nil {
		return err
	}
	if err := setInt(&t.CleanupBatchSize, "OPENSQT_TRADING_CLEANUP_BATCH_SIZE"); err != nil {
		return err
	}
	if err := setInt(&t.MarginLockDurationSec, "OPENSQT_TRADING_MARGIN_LOCK_DURATION_SECONDS"); err != nil {
		return err
	}
	if err := setInt(&t.PositionSafetyCheck, "OPENSQT_TRADING_POSITION_SAFETY_CHECK"); err != nil {
		return err
	}

	execCfg := &cfg.Execution
	if err := setInt(&execCfg.MakerGuardTicks, "OPENSQT_EXECUTION_MAKER_GUARD_TICKS"); err != nil {
		return err
	}
	if err := setInt(&execCfg.QuoteStaleMS, "OPENSQT_EXECUTION_QUOTE_STALE_MS"); err != nil {
		return err
	}
	if err := setInt(&execCfg.PostOnlyRetryMinMS, "OPENSQT_EXECUTION_POST_ONLY_RETRY_MIN_MS"); err != nil {
		return err
	}
	if err := setInt(&execCfg.PostOnlyRetryMaxMS, "OPENSQT_EXECUTION_POST_ONLY_RETRY_MAX_MS"); err != nil {
		return err
	}
	if err := setInt(&execCfg.PostOnlyRetryBurst, "OPENSQT_EXECUTION_POST_ONLY_RETRY_BURST"); err != nil {
		return err
	}
	if err := setString(&execCfg.CatchUpMode, "OPENSQT_EXECUTION_CATCH_UP_MODE"); err != nil {
		return err
	}
	if err := setInt(&execCfg.MaxActiveCatchUpSlots, "OPENSQT_EXECUTION_MAX_ACTIVE_CATCH_UP_SLOTS"); err != nil {
		return err
	}
	if err := setInt(&execCfg.MaxCatchUpSlotsPerAdjust, "OPENSQT_EXECUTION_MAX_CATCH_UP_SLOTS_PER_ADJUST"); err != nil {
		return err
	}
	if err := setFloat(&execCfg.MaxCatchUpDistanceRatio, "OPENSQT_EXECUTION_MAX_CATCH_UP_DISTANCE_RATIO"); err != nil {
		return err
	}
	if err := setFloat(&execCfg.NearTouchSingleOrderRatio, "OPENSQT_EXECUTION_NEAR_TOUCH_SINGLE_ORDER_RATIO"); err != nil {
		return err
	}

	if err := setString(&cfg.System.LogLevel, "OPENSQT_SYSTEM_LOG_LEVEL"); err != nil {
		return err
	}
	if err := setBool(&cfg.System.CancelOnExit, "OPENSQT_SYSTEM_CANCEL_ON_EXIT"); err != nil {
		return err
	}

	r := &cfg.RiskControl
	if err := setBool(&r.Enabled, "OPENSQT_RISK_CONTROL_ENABLED"); err != nil {
		return err
	}
	if err := setStringSlice(&r.MonitorSymbols, "OPENSQT_RISK_CONTROL_MONITOR_SYMBOLS"); err != nil {
		return err
	}
	if err := setString(&r.Interval, "OPENSQT_RISK_CONTROL_INTERVAL"); err != nil {
		return err
	}
	if err := setFloat(&r.VolumeMultiplier, "OPENSQT_RISK_CONTROL_VOLUME_MULTIPLIER"); err != nil {
		return err
	}
	if err := setInt(&r.AverageWindow, "OPENSQT_RISK_CONTROL_AVERAGE_WINDOW"); err != nil {
		return err
	}
	if err := setInt(&r.RecoveryThreshold, "OPENSQT_RISK_CONTROL_RECOVERY_THRESHOLD"); err != nil {
		return err
	}

	tm := &cfg.Timing
	if err := setInt(&tm.WebSocketReconnectDelay, "OPENSQT_TIMING_WEBSOCKET_RECONNECT_DELAY"); err != nil {
		return err
	}
	if err := setInt(&tm.WebSocketWriteWait, "OPENSQT_TIMING_WEBSOCKET_WRITE_WAIT"); err != nil {
		return err
	}
	if err := setInt(&tm.WebSocketPongWait, "OPENSQT_TIMING_WEBSOCKET_PONG_WAIT"); err != nil {
		return err
	}
	if err := setInt(&tm.WebSocketPingInterval, "OPENSQT_TIMING_WEBSOCKET_PING_INTERVAL"); err != nil {
		return err
	}
	if err := setInt(&tm.ListenKeyKeepAliveInterval, "OPENSQT_TIMING_LISTEN_KEY_KEEPALIVE_INTERVAL"); err != nil {
		return err
	}
	if err := setInt(&tm.PriceSendInterval, "OPENSQT_TIMING_PRICE_SEND_INTERVAL"); err != nil {
		return err
	}
	if err := setInt(&tm.RateLimitRetryDelay, "OPENSQT_TIMING_RATE_LIMIT_RETRY_DELAY"); err != nil {
		return err
	}
	if err := setInt(&tm.OrderRetryDelay, "OPENSQT_TIMING_ORDER_RETRY_DELAY"); err != nil {
		return err
	}
	if err := setInt(&tm.PricePollInterval, "OPENSQT_TIMING_PRICE_POLL_INTERVAL"); err != nil {
		return err
	}
	if err := setInt(&tm.StatusPrintInterval, "OPENSQT_TIMING_STATUS_PRINT_INTERVAL"); err != nil {
		return err
	}
	if err := setInt(&tm.OrderCleanupInterval, "OPENSQT_TIMING_ORDER_CLEANUP_INTERVAL"); err != nil {
		return err
	}

	d := &cfg.Dashboard
	if v, ok := lookupEnv("OPENSQT_DASHBOARD_ENABLED"); ok {
		b, err := parseBool(v)
		if err != nil {
			return fmt.Errorf("OPENSQT_DASHBOARD_ENABLED 不是有效布尔值: %q", v)
		}
		d.Enabled = &b
	}
	if err := setString(&d.Listen, "OPENSQT_DASHBOARD_LISTEN"); err != nil {
		return err
	}
	if err := setString(&d.Token, "OPENSQT_DASHBOARD_TOKEN"); err != nil {
		return err
	}
	if err := setInt(&d.PushIntervalMS, "OPENSQT_DASHBOARD_PUSH_INTERVAL_MS"); err != nil {
		return err
	}
	if err := setInt(&d.AccountRefreshSec, "OPENSQT_DASHBOARD_ACCOUNT_REFRESH_SEC"); err != nil {
		return err
	}
	return nil
}

func applyBinanceEnv(ex *BinanceConfig) error {
	const prefix = "OPENSQT_EXCHANGES_BINANCE_"
	if v, ok := lookupEnv(prefix + "API_KEY"); ok {
		ex.APIKey = v
	}
	if v, ok := lookupEnv(prefix + "SECRET_KEY"); ok {
		ex.SecretKey = v
	}
	if v, ok := lookupEnv(prefix + "FEE_RATE"); ok {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return fmt.Errorf("%sFEE_RATE 不是有效数字: %q", prefix, v)
		}
		ex.FeeRate = n
	}
	return nil
}

func lookupEnv(key string) (string, bool) {
	v, ok := os.LookupEnv(key)
	if !ok {
		return "", false
	}
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	return v, true
}

func setString(dst *string, key string) error {
	if v, ok := lookupEnv(key); ok {
		*dst = v
	}
	return nil
}

func setInt(dst *int, key string) error {
	v, ok := lookupEnv(key)
	if !ok {
		return nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s 不是有效整数: %q", key, v)
	}
	*dst = n
	return nil
}

func setFloat(dst *float64, key string) error {
	v, ok := lookupEnv(key)
	if !ok {
		return nil
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return fmt.Errorf("%s 不是有效数字: %q", key, v)
	}
	*dst = n
	return nil
}

func setBool(dst *bool, key string) error {
	v, ok := lookupEnv(key)
	if !ok {
		return nil
	}
	b, err := parseBool(v)
	if err != nil {
		return fmt.Errorf("%s 不是有效布尔值: %q", key, v)
	}
	*dst = b
	return nil
}

func setStringSlice(dst *[]string, key string) error {
	v, ok := lookupEnv(key)
	if !ok {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	*dst = out
	return nil
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true, nil
	case "0", "false", "no", "n", "off":
		return false, nil
	default:
		return false, fmt.Errorf("invalid bool")
	}
}

func parseDotEnv(content string) (map[string]string, error) {
	out := make(map[string]string)
	for i, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "export ") {
			trimmed = strings.TrimSpace(trimmed[len("export "):])
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			return nil, fmt.Errorf("第 %d 行缺少 '='", i+1)
		}
		key = strings.TrimSpace(key)
		if key == "" {
			return nil, fmt.Errorf("第 %d 行变量名为空", i+1)
		}
		value = strings.TrimSpace(value)
		if unquoted, err := unquoteEnv(value); err == nil {
			value = unquoted
		} else if i := strings.Index(value, " #"); i >= 0 {
			value = strings.TrimSpace(value[:i])
		}
		out[key] = value
	}
	return out, nil
}

func unquoteEnv(v string) (string, error) {
	if len(v) >= 2 {
		if v[0] == '"' && v[len(v)-1] == '"' {
			return strconv.Unquote(v)
		}
		if v[0] == '\'' && v[len(v)-1] == '\'' {
			return v[1 : len(v)-1], nil
		}
	}
	return "", fmt.Errorf("not quoted")
}
