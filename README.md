<div align="center">
  <img src="https://r2.opensqt.com/opensqt_logo.png" alt="OpenSQT Logo" width="600"/>
  
  # OpenSQT Market Maker
  
  **毫秒级高频加密货币做市商系统 | High-Frequency Crypto Market Maker**

  [![Go Version](https://img.shields.io/badge/Go-1.25%2B-blue.svg)](https://golang.org/dl/)
  [![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
</div>

---

## 📖 项目简介 (Introduction)

OpenSQT Market Maker 是一个高性能、低延迟的 Binance 永续合约做市商系统，专注于单向做多无限独立网格交易策略。系统采用 Go 语言开发，基于 WebSocket 实时数据流驱动。

经过数个版本迭代，我们已经使用此系统交易超过1亿美元的虚拟货币，例如，交易币安ETHUSDC，0手续，价格间隔1美元，每笔购买300美元，每天的交易量将达到300万美元以上，一个月可以交易5000万美元以上，只要市场是震荡或向上将持续产生盈利，如果市场单边下跌，3万美元保证金可以保证下跌1000个点不爆仓，通过不断交易拉低成本，只要回涨50%即可保本，涨回开仓原价可以赚到丰厚利润，如果出现单边极速下跌，主动风控系统将会自动识别立刻停止交易，当市场恢复后才允许继续下单，不担心插针爆仓。

举例： eth 3000点开始交易，价格下跌到2700点，亏损约3000美元，价格涨回2850点以上已经保本，涨回3000点，盈利在1000-3000美元。

OpenSQT is a high-performance, low-latency Binance perpetual futures market maker focused on long grid trading. It is developed in Go and driven by WebSocket real-time data streams.

## 📺 实时演示 (Live Demo)

<video src="https://r2.opensqt.com/product_review.mp4" controls="controls" width="100%"></video>

[点击观看演示视频 / Watch Demo Video](https://r2.opensqt.com/product_review.mp4)

## ✨ 核心特性 (Key Features)

- **Binance 专用**: 聚焦 Binance U 本位永续合约，减少无关适配层的复杂度。
- **毫秒级响应**: 全 WebSocket 驱动（行情与订单流），拒绝轮询延迟。
- **智能网格策略**: 
  - **固定金额模式**: 资金利用率更可控。
  - **超级槽位系统 (Super Slot)**: 智能管理挂单与持仓状态，防止并发冲突。
  - **严格 Maker 执行**: 最优盘口提交前复检、被动补格和版本化退避，全路径保持 PostOnly。
- **强大的风控系统**:
  - **主动风控**: 实时监控 K 线成交量异常，自动暂停交易。
  - **资金安全**: 启动前自动检查余额、杠杆倍数与最大持仓风险。
  - **自动对账**: 定期同步本地与交易所状态，确保数据一致性。
- **高并发架构**: 基于 Goroutine + Channel + Sync.Map 的高效并发模型。

## 🏦 支持的交易所 (Supported Exchange)

| 交易所 (Exchange) | 状态 (Status) 
|-------------------|---------------
| **Binance**       | ✅ Stable      

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
│   ├── binance_exchange.go    # 创建 Binance 实例
│   ├── types.go               # 通用数据结构
│   ├── wrapper_binance*.go    # Binance 接口包装
│   └── binance/               # Binance 实现
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

## 最佳实践
1.用来刷交易所vip，本系统是刷量神器，如果上涨下跌幅度不大，3000美元保证金两三天即可刷出1000万美元交易量。

2.赚钱的最佳实践，在市场经过一轮下跌后介入，先买一笔持仓，然后再启动软件，会自动向上一格格卖出，当你的持仓卖光以后停止系统，或不确定当前市场是否是低点，可以不买底仓启动，如果下跌在低点再补一笔持仓重新启动持续给你卖出，利润将最大化，如此循环往复持续赚钱，下跌也不怕，程序持续拉低成本，只要涨回一半即可保本。

## 🚀 快速开始 (Getting Started)

### 环境要求 (Prerequisites)
- Go 1.25 或更高版本
- 网络环境需能访问 Binance API

### 安装 (Installation)

1. **克隆仓库**
   ```bash
   git clone https://github.com/dennisyang1986/opensqt_market_maker.git
   cd opensqt_market_maker
   ```

2. **安装依赖**
   ```bash
   go mod download
   ```

### 配置 (Configuration)

1. 复制示例配置文件：
   ```bash
   cp config.example.yaml config.yaml
   cp .env.example .env          # 可选：用环境变量覆盖密钥
   ```

2. 编辑 `config.yaml`，填入你的 API Key 和策略参数：

   ```yaml
   exchanges:
     binance:
       api_key: "YOUR_API_KEY"
       secret_key: "YOUR_SECRET_KEY"
       fee_rate: 0.0002

   trading:
     symbol: "ETHUSDT"       # 交易对
     price_interval: 2       # 网格间距 (价格)
     order_quantity: 30      # 每格投入金额 (USDT)
     max_margin_usage_percent: 80 # 最大保证金占用比例，范围 (0,100]，默认100
     buy_window_size: 10     # 买单挂单数量
     sell_window_size: 10    # 卖单挂单数量
   ```

旧配置中的 `app.current_exchange` 已不再使用，可以直接删除；`exchanges.binance` 与原有 Binance 环境变量名称保持兼容。

保证金占用比例按 `(保证金余额 - 可用余额) / 保证金余额 × 100%` 计算，既包含持仓占用，也包含未成交挂单冻结。超过 `max_margin_usage_percent` 后，程序会锁存停止新单，并撤销该交易对的全部挂单；即使占用比例随后下降，本次运行也不会自动恢复，需人工确认账户状态后重启。首次账户读数已经超限时，确认全撤后程序会以停单态继续运行，方便从只读面板观察；首次全撤后的 15 秒内还会每 2 秒复查一次迟到挂单。限制已经锁存时，即使配置了 `system.cancel_on_exit: false`，退出过程仍会强制全撤，并在固定 15 秒窗口内复查迟到挂单。

也可以只使用 `.env`（例如 `OPENSQT_EXCHANGES_BINANCE_API_KEY`），不必提供 `config.yaml`。若两者都有，非空环境变量优先。保证金限制对应环境变量为 `OPENSQT_TRADING_MAX_MARGIN_USAGE_PERCENT`；完整变量列表见 [.env.example](.env.example)。

#### Telegram 成交通知

可选开启买单、卖单的全成交通知，默认关闭。在 Telegram 中通过 [@BotFather](https://t.me/BotFather) 创建 Bot 并取得 Bot Token，然后向自己的 Bot 发送 `/start`。向群组发送时，先把 Bot 加入群组并确保它有发送消息的权限；向频道发送时需授予 Bot 发布消息的权限。

在 `.env` 中填写：

```dotenv
OPENSQT_TELEGRAM_ENABLED=true
OPENSQT_TELEGRAM_BOT_TOKEN=你的BotToken
OPENSQT_TELEGRAM_CHAT_ID=你的ChatID
```

Chat ID 可以是私聊数字 ID、群组负数 ID（超级群通常以 `-100` 开头）或公开频道的 `@频道名`。可在向 Bot 发送消息后，通过 Telegram Bot API 的 `getUpdates` 响应读取 `message.chat.id`；若 Bot 已配置 webhook，需使用现有 webhook 接收到的聊天 ID。不要公开包含 Token 的 API 地址。

也支持 YAML 配置（非空环境变量优先）：

```yaml
telegram:
  enabled: true
  bot_token: "你的BotToken"
  chat_id: "你的ChatID"
```

保存后重启程序生效。通知包含交易对、买卖方向、成交均价、数量、成交金额、订单 ID 和带时区的成交时间；卖单还包含按交易所口径记录的已实现盈亏（未扣手续费）。只在系统新确认订单全部成交（`FILLED`）后通知，部分成交、撤销、拒绝和重复事件不会发送；对账确认的全成交也经过同一去重入口。

通知使用独立后台协程，队列最多 128 条，同一聊天发送间隔至少 1 秒；Telegram 明确返回限流时按 `retry_after` 等待，最多尝试 3 次，单次等待上限 60 秒。单次请求超时 10 秒，网络错误和其他发送失败只记日志，不影响交易；为避免重复送达，不重试结果不确定的网络请求。队列满时丢弃新通知并记录日志；队列和去重状态只在本次运行内有效，退出会取消发送，重启不补发历史通知。运行环境需能访问 `api.telegram.org`，支持标准 `HTTPS_PROXY` 环境变量。

#### 严格 Maker 执行

Binance 的 PostOnly 订单在到达撮合系统时如果会立即成交，会被明确拒绝。程序不会降级成 Taker，而是使用同一条市场数据 WebSocket 同时接收逐笔成交和 `bookTicker`，并在真正提交前按最新买一/卖一再次复检。逻辑网格价格保持不变；当 BUY 网格已经穿过 Maker 安全边界时，可把实际委托价向下移动到安全价，成交后仍按原逻辑槽位计算目标卖价。若 SELL 目标在下单前已被行情穿过，则挂到买一价上方最近的合法 Maker tick；多个卖单冲突时逐 tick 避让，不再额外抬高一整个网格。

```yaml
execution:
  maker_guard_ticks: 2
  quote_stale_ms: 1500
  post_only_retry_min_ms: 50
  post_only_retry_max_ms: 500
  post_only_retry_burst: 5
  catch_up_mode: "passive"          # passive: 被动补格；exact_wait: 原价等待
  max_active_catch_up_slots: 1
  max_catch_up_slots_per_adjust: 1
  max_catch_up_distance_ratio: 0.5
  near_touch_single_order_ratio: 0.15
  max_gap_stack_slots: 1            # 跳格时当前买单合并几格金额
```

- 同一盘口版本只尝试一次；明确 `-5022` 后默认按 50/100/200/400/500ms 退避，超过 5 次后每次至少等待 1 秒。
- 盘口超过 1500ms 未更新、连接重建或快照不完整时暂停新提交；不会使用旧盘口盲挂。
- 距盘口过近的订单拆成单笔请求，降低原生批量请求中共享陈旧盘口的概率。
- 下单结果为 UNKNOWN 时保持原槽位 `PENDING` 和原 ClientOrderID，优先等待订单流；预留超过 5 秒后，对账会按同一 ClientOrderID 只读确认。只有 3 次、间隔至少 500ms 的查询都明确返回“订单不存在”才释放槽位；超时或异常响应仍保持关闭，避免重复下单。
- `order_quantity` 始终表示每格报价货币金额；被动补格改变实际买价时会重新计算数量。
- 价格向下跳过尚未成交的空格时，当前格会按跳过的格数合并金额下一单；成交后拆到被跳过的格，分别在各自上一格卖出。

### 运行 (Usage)

```bash
go run main.go
```

或者编译后运行：

```bash
go build -o opensqt
./opensqt
```

启动后默认打开本机监控面板（只读，不改单、不下单）：

```text
http://127.0.0.1:8787
```

可在 `config.yaml` 里调整：

```yaml
dashboard:
  enabled: true
  listen: "127.0.0.1:8787"
  token: ""                  # 非空则 /api 与 /ws 需要 ?token=
  push_interval_ms: 400
  account_refresh_sec: 10
```

面板展示价格、K 线网格执行图、Maker 接受率与拒单/补格指标、程序启动后的成交订单、持仓、主动风控和最近日志。保证金的人民币估值使用 Frankfurter 的 USD/CNY 日汇率：服务端每 6 小时异步刷新内存缓存，失败时每 10 分钟重试并保留最后一次成功值，不阻塞面板或交易流程。网格执行图会区分待确认、撤单中、重试冷却、状态异常与真正空网格，并在策略参数中显示订单容量已用/上限/剩余。默认只绑定本机；若改成 `0.0.0.0` 会对外暴露账户与仓位，请同时设置 `token`。

## 🐳 Docker (GitHub Packages)

推送到 `main`、打 `v*` 版本标签，或手动触发 Actions 后，GitHub Actions 会自动构建 Docker 镜像并发布到 GitHub Container Registry（GitHub Packages）：

```text
ghcr.io/zuoban/opensqt_market_maker:<tag>
```

版本标签示例：`v3.4.7`、`3.4.7`、`latest`（仅版本 tag 会更新 `latest`）。`main` 分支会发布 `main` 和 `sha-*` 标签。镜像同时支持 `linux/amd64` 与 `linux/arm64`。

首次发布的 Package 默认是私有的。可在仓库的 **Packages** 页面把可见性改为 Public，或拉取前先登录：

```bash
echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin
docker pull ghcr.io/zuoban/opensqt_market_maker:latest
```

镜像不包含配置和密钥。`config.yaml` 不是必须的：把 `.env` 填齐即可启动。若同时挂载 YAML，非空环境变量仍会覆盖它。

```bash
cp .env.example .env
# 编辑 .env：填写 API Key、策略参数，并设置 OPENSQT_DASHBOARD_TOKEN

docker compose up -d --build
```

`.env.example` 里已把 `OPENSQT_DASHBOARD_LISTEN=0.0.0.0:8787`，容器外可访问监控面板。

或直接运行已发布镜像：

```bash
docker run -d --name opensqt_market_maker --restart unless-stopped \
  --stop-timeout 30 \
  -p 8787:8787 \
  --env-file .env \
  -v "$PWD/log:/app/log" \
  ghcr.io/zuoban/opensqt_market_maker:latest
```

监控面板：

```text
http://127.0.0.1:8787
```

## 📦 版本发布 (Release)

项目现在使用统一版本规范：以 [main.go](main.go#L21) 中的 `Version` 为准，Git Tag 与 GitHub Release 必须和它保持一致。

标准发版文档见 [RELEASE.md](RELEASE.md)。

生成可分发安装包：

```bash
./scripts/package_release.sh
```

也可以指定目标平台：

```bash
TARGET_OS=windows TARGET_ARCH=amd64 ./scripts/package_release.sh
TARGET_OS=MacOS TARGET_ARCH=arm64 ./scripts/package_release.sh
```

推送版本 tag 后，GitHub Actions 会自动构建并发布 Linux、Windows、MacOS 三个平台附件到 GitHub Release，其中 MacOS 使用 Apple Silicon 对应的 arm64 架构；同时会构建 Docker 镜像并推送到 GitHub Packages（`ghcr.io`）。

生成的发行包默认包含：

- 编译后的可执行文件
- `live_server/` 中由 Git 明确跟踪的演示文件
- `config.example.yaml`
- `config.yaml`（由示例配置生成，避免泄露本机密钥）
- `.env.example`
- `README.md`
- `ARCHITECTURE.md`

本地的 `部署教程.pdf` 不会进入版本 tag、自动源码包或公开发行附件。

## 🏗️ 系统架构 (Architecture)

系统采用模块化设计，核心组件包括：

- **Exchange Layer**: Binance 适配器与交易核心之间的接口边界。
- **Price Monitor**: 全局唯一的 WebSocket 价格源，确保决策一致性。
- **Super Position Manager**: 核心仓位管理器，基于槽位 (Slot) 机制管理订单生命周期。
- **Safety & Risk Control**: 多层级风控，包含启动检查、运行时监控和异常熔断。

更多详细架构说明请参阅 [ARCHITECTURE.md](ARCHITECTURE.md)。

## ⚠️ 免责声明 (Disclaimer)

本软件仅供学习和研究使用。加密货币交易具有极高风险，可能导致资金损失。
- 使用本软件产生的任何盈亏由用户自行承担。
- 当前发行版尚无 Testnet 切换配置，使用 Binance 生产环境；仅更换为测试网 Key 不会切换连接地址。离线测试可按仓库说明运行。
- 开发者不对因软件错误、网络延迟或交易所故障导致的损失负责。

This software is for educational and research purposes only. Cryptocurrency trading involves high risk.
- Users are solely responsible for any profits or losses.
- This release uses Binance production endpoints and does not provide a Testnet switch. Replacing API keys does not change the environment; use the repository test commands for offline validation.
- The developers are not liable for losses due to software bugs, network latency, or exchange failures.

## 🤝 贡献 (Contributing)

欢迎提交 Issue 和 Pull Request！

---
Copyright © 2025 OpenSQT Team. All Rights Reserved.
