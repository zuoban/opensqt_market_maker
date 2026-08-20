# AGENTS.md

给编码代理用的仓库说明。详细架构见 [ARCHITECTURE.md](ARCHITECTURE.md)，发版见 [RELEASE.md](RELEASE.md)。

## 项目

OpenSQT 是 Go 写的加密货币永续合约**单向做多网格做市**程序。行情和订单都走 WebSocket，不轮询价格。当前版本以 `main.go` 里的 `Version` 为准。

模块路径：`opensqt`（Go 1.25）。

## 常用命令

```bash
go build ./...
go test ./...
go test ./position ./web ./monitor ./config ./safety
node --test web/app_frontend_test.js
go run main.go                  # 读取 ./config.yaml
go run main.go /path/to.yaml
```

本地打包：

```bash
./scripts/package_release.sh
TARGET_OS=windows TARGET_ARCH=amd64 ./scripts/package_release.sh
TARGET_OS=MacOS TARGET_ARCH=arm64 ./scripts/package_release.sh
```

Docker：

```bash
docker compose up -d --build    # 需挂载 config.yaml；dashboard.listen 用 0.0.0.0:8787
```

## 硬约束（改交易逻辑前必读）

1. **单一价格源**：全局只有 `monitor.PriceMonitor` 一条 WebSocket 价格流。其它模块用 `GetLastPrice()` / `GetLastPriceString()`，禁止再开价格流或用 REST 轮询价格。
2. **先订交流、后下单**：`StartOrderStream` 必须在任何 `PlaceOrder` 之前，否则会丢成交推送。
3. **固定金额网格**：`trading.order_quantity` 是每格投入的报价货币金额（USDT/USDC），不是币数量。
4. **槽位锁定**：买卖与持仓都挂在价格槽上（FREE / PENDING / LOCKED）。不要绕过槽位直接改订单。
5. **面板只读**：`web/` 是本地监控，不能改单、不能下单。失败不得拖垮做市主循环。
6. **密钥不出包**：`config.yaml` 已 gitignore。发版脚本和 Docker 镜像只用 `config.example.yaml`，禁止把真实 Key 打进镜像、Release 或日志。

更完整的原则与数据流见 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 目录

| 路径 | 职责 |
|------|------|
| `main.go` | 启动编排、版本号 |
| `config/` | YAML 加载与校验 |
| `exchange/` | `IExchange` + 各所 adapter/wrapper |
| `monitor/` | 唯一价格流；面板用的 1m K 线缓存 |
| `order/` | 下单执行（限流、PostOnly 降级、重试） |
| `position/` | 超级槽位、成交记录、小时汇总 |
| `safety/` | 启动检查、主动风控、对账、订单清理 |
| `web/` | 只读监控（Go embed `web/static/`） |
| `logger/` | 控制台 + DEBUG 文件日志 |
| `live_server/` | 独立演示页，**不是**主程序依赖 |
| `scripts/package_release.sh` | 跨平台发行包 |

交易所实现放在 `exchange/<name>/`，经 `wrapper_*.go` 接到 `IExchange`。新交易所走工厂 `exchange.NewExchange`，不要在 `main` 里写死所名分支。

## 配置

- 模板：`config.example.yaml`
- 运行时：`config.yaml`（不要提交）
- 默认监控面板：`http://127.0.0.1:8787`。绑 `0.0.0.0` 时必须设 `dashboard.token`

## 监控面板

- 静态资源：`web/static/{index.html,app.css,app.js}`，由 `web/embed.go` embed。
- 前端单测：`node --test web/app_frontend_test.js`（`app.js` 在 Node 下只导出 `buildKlineGridModel` / `buildHourlyFillModel`）。
- 成交**列表**最多 20 条（`position.maxRecentFilledOrders`）。
- 成交**汇总图**用快照里的 `filledHourly`（后端按本地整点累计，固定 24 小时，含空小时）。不要改回用这 20 条明细在前端加总。
- K 线缓存上限仍是 `monitor.maxVisibleCandles`（当前 500 根 1m），与汇总图无关。
- 可视化夹具：`WEB_VISUAL=1 go test ./web -run TestVisualFixture -count=1`，监听 `127.0.0.1:18789`。

## 发版

1. 改 `main.go` 的 `Version`（`v主.次.补丁`），建议同步 `ARCHITECTURE.md` 文首版本。
2. `go mod verify && go build ./...`，必要时跑测试。
3. 提交并推送 `main`，再打**同名** annotated tag。不要重打已有 tag。
4. GitHub Actions：
   - `.github/workflows/release.yml`：Linux amd64 / Windows amd64 / MacOS arm64 附件
   - `.github/workflows/docker.yml`：`ghcr.io/zuoban/opensqt_market_maker`（tag 出 `vX.Y.Z` 与 `latest`；`main` 出 `main` / `sha-*`）

镜像不含 `config.yaml`。首次 GHCR Package 默认私有。

## 代码习惯

- 提交信息：`type(scope): 中文说明`，与现有 `feat(web):` / `feat:` 风格一致。
- 交易路径改动要有测试：`position/`、`safety/`、`web/`、`monitor/` 已有包内测试。
- 不要把 API Key、Passphrase、真实 `config.yaml` 写进代码、fixture 或文档示例以外的地方。
- `live_server/` 和 `部署教程.pdf` 不要塞进 Docker 构建上下文（已在 `.dockerignore`）。
- 改 UI 时同步 `web/static/` 三件套，并跑前端单测；不要为了图方便去改 K 线逻辑，除非任务明确要求。
