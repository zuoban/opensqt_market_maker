# AGENTS.md - OpenSQT Market Maker 开发指南

## 构建和测试命令

### 构建命令
```bash
go run main.go [config.yaml]
go build -o opensqt
go mod download && go mod tidy
```

### 测试命令
```bash
go test ./...                          # 运行所有测试
go test ./exchange                      # 运行单个包的测试
go test -run TestFunctionName ./path   # 运行单个测试函数
go test -v ./...                       # 详细测试输出
go test -cover ./...                   # 带覆盖率测试
```

### 代码质量检查
```bash
go fmt ./...                           # 格式化代码
go vet ./...                           # 静态检查
```

## 代码风格指南

### 导入规范
标准库 → 内部包 → 第三方包，空行分隔，按字母顺序排列。

```go
import (
	"context"
	"fmt"
	"time"

	"opensqt/config"
	"opensqt/logger"

	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
)
```

### 命名约定
- 包名：小写单词，无下划线（如 `exchange`, `position`, `safety`）
- 常量：大写驼峰（如 `SlotStatusFree`, `OrderStatusFilled`）
- 变量/函数：小写驼峰（如 `currentPrice`, `PlaceOrder`）
- 私有字段：小写驼峰（如 `anchorPrice`, `slots`）
- 公开字段：大写驼峰（如 `OrderID`, `ClientOID`）
- 接口名：以 `I` 开头（如 `IExchange`）

### 类型定义
使用结构体标签配置 YAML/JSON 映射，使用常量定义枚举值。

```go
// ExchangeConfig 交易所配置
type ExchangeConfig struct {
	APIKey    string  `yaml:"api_key"`
	SecretKey string  `yaml:"secret_key"`
	FeeRate   float64 `yaml:"fee_rate"`
}

const (
	SlotStatusFree    = "FREE"
	SlotStatusPending = "PENDING"
	SlotStatusLocked  = "LOCKED"
)
```

### 并发控制
使用 `sync.Map` 存储槽位等并发数据，`sync.RWMutex` 保护复杂结构体，`atomic.Value` 存储简单值（如价格），避免在持锁时调用外部 API。

```go
type SuperPositionManager struct {
	slots              sync.Map
	mu                 sync.RWMutex
	insufficientMargin atomic.Bool
	lastMarketPrice    atomic.Value
}
```

### 错误处理
函数返回 `(result, error)`，错误信息使用 `fmt.Errorf` 并包含上下文，关键操作失败时记录日志。

```go
if err != config.LoadConfig(configPath); err != nil {
	logger.Fatalf("❌ 加载配置失败: %v", err)
}
```

### 注释和日志
公开函数必须添加注释（格式：`// Name 描述`）。使用自定义 `logger` 包（不使用 `log` 包），级别：DEBUG < INFO < WARN < ERROR < FATAL。

```go
logger.Info("✅ 配置加载成功: 交易对=%s", cfg.Trading.Symbol)
logger.Errorf("❌ 读取配置文件失败: %v", err)
```

### WebSocket 使用
WebSocket 是唯一数据源（不使用 REST 轮询），必须在交易前启动 WebSocket 流，订单流必须先于下单启动，使用回调函数处理实时更新，实现断线重连机制。

### 交易所抽象
所有交易所实现 `exchange.IExchange` 接口，使用工厂模式创建实例：`exchange.NewExchange(cfg)`，接口方法签名必须一致，统一错误处理和重试逻辑。

### 交易策略
固定金额模式：每次投入固定金额（非固定数量），槽位机制：每个价格点独立管理，价格窗口：动态调整买卖单窗口，风控优先：启动前检查，运行时监控。

### 安全注意事项
永远不要提交 API Key 或 Secret，使用 `.gitignore` 过滤配置文件，敏感信息通过环境变量传递，测试代码使用 Testnet，不使用 Mainnet。

### 核心架构原则
1. **单一价格源**：全局唯一 PriceMonitor，所有组件通过 `GetLastPrice()` 获取价格
2. **订单流优先**：先启动订单流，再下单，确保不遗漏成交推送
3. **槽位锁定**：使用 SlotStatus 防止并发重复操作
4. **接口隔离**：定义最小接口避免循环依赖
5. **原子操作**：价格和标志位使用 atomic 保证并发安全

### 避免循环依赖
`position` 包定义 `OrderRequest` 等类型避免依赖 `exchange`，`exchange` 回调使用 `interface{}` + 反射避免依赖 `position`，子包接口：只定义需要的最小接口。

### 交易核心流程
1. 启动价格监控（Wait for first price）
2. 持仓安全检查
3. 启动订单流（WebSocket）
4. 初始化仓位管理器（Create slots）
5. 启动风控监控
6. 价格驱动交易循环（AdjustOrders）

### Git 工作流
功能分支：`feature/功能名`，修复分支：`fix/问题描述`，提交信息：简短明确，中英文均可，提交前运行 `go fmt` 和 `go vet`。

### 代码示例模板
```go
package example

import (
	"context"
	"fmt"
	"opensqt/config"
	"opensqt/logger"
)

// ExampleFunction 函数描述
func ExampleFunction(ctx context.Context, cfg *config.Config) error {
	if cfg == nil {
		return fmt.Errorf("配置不能为空")
	}
	logger.Info("开始执行...")
	defer logger.Info("执行完成")
	result, err := doSomething()
	if err != nil {
		logger.Errorf("执行失败: %v", err)
		return err
	}
	return nil
}
```
