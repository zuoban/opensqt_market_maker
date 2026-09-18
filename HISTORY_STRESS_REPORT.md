# 历史订单内存压力测试（2026-09-18）

**后续优化已完成**：最近 20 条成交记录改用固定容量缓冲区，百万全成交场景累计分配从约 3.46 GiB 降至 0.60 GiB；随后减少历史查询与被过滤日志的临时分配，最新本轮对照进一步降至 0.276 GiB，终态场景由 1.056 GiB 降至 0.508 GiB。各轮独立数据和验证见 [PERFORMANCE.md](PERFORMANCE.md)。历史去重占用仍约 84 MiB，终态历史仍约 158 MiB。

以下保留**缓冲区优化前**的压力测试基线。初次压测仅新增离线测试与报告，没有改动生产交易逻辑、淘汰历史或接入持久化。基于 Go v3.5.23 工作区，Apple M1 / macOS arm64 / Go 1.27.1，GOMAXPROCS=8。

## 结果

历史占用随**唯一订单数**持续增长，即使只有一个活跃价格槽。以下为强制 GC 后的 Go 堆增量，相对于已创建管理器和一个槽位的基线；不是进程 RSS，也不包含整个做市程序的内存。

| 唯一订单数 | 全成交去重场景 | 终态累计进度场景 |
|---:|---:|---:|
| 10,000 | 0.69 MiB | 1.27 MiB |
| 100,000 | 6.37 MiB | 11.04 MiB |
| 1,000,000 | 83.81 MiB | 158.40 MiB |

每列在独立测试进程运行。百万订单时，前者有 1,000,000 个 `filledOrderKeys`，后者有 1,000,000 个 `terminalOrders`；不是同一场景同时产生两张百万记录表。两次独立运行的末端堆增量分别为 83.69–83.81 MiB、158.15–158.40 MiB，上表取第二次。map 扩容呈阶梯变化，不能把单点字节数当作每个订单的固定成本。

活跃槽位始终为 1。全成交场景最近明细始终最多 20 条，实际小时桶为 1；终态场景不产生全成交明细。历史表之外的这些状态没有随百万订单扩大。

另一项 `resolvedAbsentOrders` 测试累计插入 1,000,000 个唯一身份，每批最多 64 个待收敛记录，一半通过迟到事件的删除路径移除，一半通过带显式时间的收敛检查移除。每批完成后记录数归零，末端堆增量约 20.5 KiB。额外回归覆盖下一次插入清理早于宽限期加一分钟的记录。

这只证明**固定未完成窗口**下不积累全部历史。该 map 没有硬容量上限，清理依赖后续插入、查询或订单事件；大规模突发及空闲后残留仍需区分，删除键也不意味着 map 容量立即归还。插入会在锁内扫描当前记录，成本取决于同时待收敛数量。

## 临时分配与 GC

第二次百万订单测量：

| 指标 | 全成交 | 撤销/过期/拒绝终态 |
|---|---:|---:|
| 实际订单回调数 | 3,000,000 | 5,000,000 |
| 工作循环累计分配 | 3.462 GiB | 1.055 GiB |
| 工作循环分配次数 | 31,517,037 | 48,015,886 |
| 工作循环自然 GC 次数 | 221 | 39 |
| 自然 GC 累计暂停 | 11.54 ms | 2.06 ms |
| 工作循环耗时 | 2.98 s | 3.56 s |
| 最后一次人为触发 GC 的调用耗时 | 8.07 ms | 10.24 ms |

这些是加速回放的本机结果，包含生成事件、断言和槽位回收的成本。计时不包含检查点强制 GC、结果打印和 profile 写盘。人为触发 GC 的调用耗时包含 GC 完成等待，**不是全程停止应用的时长**；自然 GC 暂停也不包含全部 GC CPU 和分配辅助成本。没有测量实盘回调 P95/P99，不能由这些总量推断线上延迟。

内存 profile 定位到两个不同问题：

1. `inuse_space` 的主要来源是两类历史 map 及身份字符串，吻合强制 GC 后的持续增长。
2. 全成交场景 `alloc_space` 约 **83%** 来自 `appendFilledOrder` 中保留最近 20 条的切片复制。每次超过上限都分配并复制一个新切片；本机采样约 2.8–2.9 GiB。列表长度有界，但这条路径仍反复产生临时垃圾。采样 profile 的估算总量与 `runtime.MemStats` 的精确累计计数会略有差异。

相关生产位置：`position/super_position_manager.go` 的 `appendFilledOrder`、`storeTerminalOrderProgress`、`filledOrderKey`。测试夹具自身的格式化也产生分配；这里没有把整个回放的分配全部归因于业务。

## 工作负载与正确性

实现见 [position/history_stress_test.go](position/history_stress_test.go)。普通测试执行每类 10,000 个唯一订单，百万规模需要显式开启；没有读取真实配置或连接交易所。

- **全成交**：每轮 BUY / SELL 各一笔，每笔部分成交、全成交、带经纪商前缀的重复全成交，共六次真实 `OnOrderUpdate`。每轮库存归零，买卖累计量与盈亏按精确二进制小数校验。
- **终态**：循环使用 CANCELED / EXPIRED / REJECTED。旧 BUY 先部分成交终结，在新 SELL 已绑定后追加累计数量修正；随后 SELL 终态追加数量和向下的累计盈亏修正，同时注入重复、旧版本事件。每轮十次真实回调，并验证新订单身份、状态及已成交量未被旧修正覆盖。
- **回收与重用**：每 256 轮回收空槽并在同价重建，百万订单共 1,953 次；验证旧指针已退休且新对象不同。
- **很久以前的历史**：所有记录写入后，在新订单已占槽的情况下重放最早、中间及末尾订单，检查库存、盈亏、买卖累计量、全成交通知和补单通知没有重复。终态场景再对最早的一买一卖补充更高累计量和更低盈亏，验证只记差额并保持新订单绑定。
- **有界展示状态**：断言 20 条明细、最多 24 个小时桶，以及固定槽位数量。测试不模拟真实运行 24 小时的时钟推进；时间跨度与实际线上速率不由加速回放代替。

事件直接注入回调，没有执行规划器、订单创建、限流、网络或磁盘提交。日志级别为 ERROR，telemetry 关闭，通知仅用计数器；时间和分配数字不能替代默认日志与完整服务负载下的测试。百万规模按串行订单流回放，普通包测试另使用竞态检查。

## 复测

在仓库根目录运行。为避免 profile 与进程计数混入其它工作负载，每项使用单独进程；profile 路径可自选。

```sh
rtk proxy go test -race ./position

rtk proxy env OPENSQT_HISTORY_STRESS_ORDERS=1000000 OPENSQT_HISTORY_PROFILE_DIR=/tmp/opensqt-history-profiles go test ./position -run '^TestOrderHistoryStress/filled$' -count=1 -v
rtk proxy env OPENSQT_HISTORY_STRESS_ORDERS=1000000 OPENSQT_HISTORY_PROFILE_DIR=/tmp/opensqt-history-profiles go test ./position -run '^TestOrderHistoryStress/terminal$' -count=1 -v
rtk proxy env OPENSQT_HISTORY_STRESS_ORDERS=1000000 go test ./position -run '^TestResolvedAbsentHistoryStress$' -count=1 -v

rtk proxy go tool pprof -top -inuse_space /tmp/opensqt-history-profiles/filled.heap.pprof
rtk proxy go tool pprof -list appendFilledOrder -alloc_space /tmp/opensqt-history-profiles/filled.heap.pprof
rtk proxy go tool pprof -top -inuse_space /tmp/opensqt-history-profiles/terminal.heap.pprof
```

`OPENSQT_HISTORY_STRESS_ORDERS` 支持 10,000–10,000,000 范围的偶数。`history_measurement` 行输出 JSON，包含记录数、槽位数、堆字节数/对象数、累计分配、GC 与耗时；标准测试默认跳过大规模测量。不把内存和速度阈值写成断言，避免 Go 版本及平台差异造成错误失败。

初次压测已通过：三个百万规模场景、全部 `position` 包竞态测试、差异格式检查。该压测阶段没有修改 UI 或生产代码；后续缓冲区优化的验证记录见上方链接。

## 后续顺序

1. **已完成最近 20 条列表的缓冲区复用**：原锁内复用容量，对外快照独立、顺序与通知保持不变，已复用本压力测试验证分配与 GC 下降；历史 map 的持续增长仍保留。
   后续也已减少历史查询及被过滤诊断日志的临时分配，最新数据见本页开头链接。
2. **再评估持久化接入**：复用 [FILL_LEDGER_DESIGN.md](FILL_LEDGER_DESIGN.md) 的现有原型，另测同步写入 P95/P99、历史冷查询、崩溃恢复、存储故障停单与实际磁盘增长。本轮没有重新测量或接入原型，既有约 8–10 ms 平均同步提交成本不能当作尾延迟。
3. **按实际订单速率评估部署容量**：用唯一全成交和终态订单增长量估计历史规模，并观测实际堆及 GC。不能按固定条数/TTL 删除去重证据，也不能把重启当成长期修复；当前完整去重仍只保存在本次进程内。
