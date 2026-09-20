#!/usr/bin/env python3
"""Offline ledger acceptance on a selected filesystem; never starts the trader."""

import argparse
from collections import deque
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import platform
import shutil
import subprocess
import sys
import tempfile


def positive_float(value):
    number = float(value)
    if not math.isfinite(number) or number <= 0:
        raise argparse.ArgumentTypeError("must be a positive finite number")
    return number


def queue_projection(samples, interval_ms, capacity):
    """Single-writer estimate from chronological service times, not live callbacks.

    An unlimited FIFO is modeled to expose required capacity. Overflow is reported,
    not simulated by silently dropping events or changing their arrival times.
    """
    completions = deque()
    finish = 0.0
    waits = []
    maximum = 0
    first_overflow = None
    for i, duration in enumerate(samples):
        arrival = i * interval_ms
        while completions and completions[0] <= arrival:
            completions.popleft()
        # If a prior operation is active, this arrival waits behind it. After
        # adding this event, queued (excluding active) equals prior outstanding.
        queued = len(completions)
        maximum = max(maximum, queued)
        if queued > capacity and first_overflow is None:
            first_overflow = i + 1
        started = max(arrival, finish)
        waits.append(started - arrival)
        finish = started + duration
        completions.append(finish)
    waits.sort()
    return {
        "arrival_interval_ms": interval_ms,
        "max_waiting_events": maximum,
        "queue_capacity": capacity,
        "first_overflow_event": first_overflow,
        "wait_p99_ms": waits[math.ceil(len(waits) * .99) - 1],
        "max_wait_ms": waits[-1],
        "drain_after_last_arrival_ms": finish - (len(samples) - 1) * interval_ms,
    }


def run_logged(command, cwd, env, path):
    with path.open("w", encoding="utf-8") as output:
        subprocess.run(command, cwd=cwd, env=env, stdout=output,
                       stderr=subprocess.STDOUT, check=True)


def load_scale_results(path, samples):
    rows = []
    for line in path.read_text(encoding="utf-8").splitlines():
        marker = "ledger_scale {"
        if marker in line:
            row = json.loads("{" + line.split(marker, 1)[1])
            values = row.get("sync_commit_samples_ms", [])
            if len(values) != samples or any(not math.isfinite(x) or x < 0 for x in values):
                raise ValueError("missing/invalid chronological commit samples")
            if not row["recovery"]["old_corrections_and_replays_ok"]:
                raise ValueError("recovery verification did not pass")
            rows.append(row)
    return rows


def write_report(directory, report):
    (directory / "results.json").write_text(
        json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    status = {"measured": "测量完成", "budget_exceeded": "提交 P99 超出指定预算",
              "failed": "执行失败", "interrupted": "已中断"}.get(report["status"], report["status"])
    race = "开启" if report["race"] else "已显式关闭"
    lines = ["# 账本部署磁盘离线验收", "", f"- 状态：{status}",
             f"- 平台：{report['platform']}", f"- Go：{report.get('go_version', '未完成检查')}",
             f"- 目标目录：{report['target_directory']}",
             f"- 正确性测试 race：{race}",
             "- 性能采样不启用 race；数据库保持默认同步写入。", ""]
    if "error" in report:
        lines += [f"失败详情：{report['error']}", ""]
    lines += ["| 历史数 | 提交 P95 / P99 / 最大（ms） | 重试 P99（ms） | 启动（s） | 启动分配（MiB） | 文件（MiB） |",
              "|---:|---:|---:|---:|---:|---:|"]
    for row in report.get("scale_results", []):
        c, r = row["sync_commit"], row["recovery"]
        lines.append(f"| {row['seed_orders']} | {c['p95_ms']:.3f} / {c['p99_ms']:.3f} / {c['max_ms']:.3f} | "
                     f"{row['exact_duplicate_commit']['p99_ms']:.4f} | {r['open_ms']/1000:.3f} | "
                     f"{r['startup_allocated_bytes']/2**20:.2f} | {row['file_final_bytes']/2**20:.2f} |")
    if report.get("queue_projections"):
        lines += ["", "## 最大历史规模的排队估算", "",
                  "以下是按实测逐笔提交耗时计算的单写者 FIFO 模型，不是真实回调负载测试。",
                  "假设固定到达间隔、无限排队；超过所填容量时报告首个超限事件，未丢弃样本。",
                  "不含回调读取、槽位锁、网络、风控和生产缓冲开销，可能低估实际积压。", "",
                  "| 到达间隔（ms） | 最大等待事件数 | 等待 P99（ms） | 最大等待（ms） | 首个容量超限事件 |",
                  "|---:|---:|---:|---:|---:|"]
        for q in report["queue_projections"]:
            lines.append(f"| {q['arrival_interval_ms']:g} | {q['max_waiting_events']} | "
                         f"{q['wait_p99_ms']:.3f} | {q['max_wait_ms']:.3f} | {q['first_overflow_event'] or '无'} |")
    lines += ["", "新进程及查询均未清空系统缓存；Go 堆不是 RSS。日志与原始样本保存在本目录，临时数据库已清理。",
              "正确性检查是故障注入与进程退出测试，不代表掉电验证。完成测量不等于允许生产接入；",
              "仍需完整启动恢复、历史补齐、迁移、并发负载及部署平台验收。", ""]
    (directory / "REPORT.md").write_text("\n".join(lines), encoding="utf-8")


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", type=Path, required=True, help="existing directory on the target persistent filesystem")
    parser.add_argument("--orders", type=int, choices=[1000, 10000, 100000, 1000000], default=1000000)
    parser.add_argument("--samples", type=int, default=1000, help="100–5000 sync commits per scale")
    parser.add_argument("--arrival-ms", type=positive_float, nargs="+", default=[10, 20, 50, 100], help="illustrative queue-model arrival intervals")
    parser.add_argument("--queue-capacity", type=int, default=256, help="waiting-event capacity to compare with the model")
    parser.add_argument("--p99-budget-ms", type=positive_float, help="optional maximum commit P99; exceeding returns exit code 2")
    parser.add_argument("--no-race", action="store_true", help="explicitly disable correctness race checks if platform/toolchain cannot support them")
    args = parser.parse_args(argv)
    if not 100 <= args.samples <= 5000 or args.queue_capacity < 0:
        parser.error("samples must be 100–5000; queue capacity must be nonnegative")
    target = args.directory.expanduser().resolve()
    if not target.is_dir():
        parser.error("directory must already exist on the intended filesystem")
    required = args.orders * 3000 + (256 << 20)
    if shutil.disk_usage(target).free < required:
        parser.error(f"insufficient free space; reserve at least {required/2**30:.2f} GiB for this fixture")
    root = Path(__file__).resolve().parents[1]
    output = Path(tempfile.mkdtemp(prefix="opensqt-ledger-acceptance-", dir=target))
    report = {"status": "running", "timestamp_utc": datetime.now(timezone.utc).isoformat(),
              "platform": platform.platform(), "target_directory": str(target),
              "race": not args.no_race, "orders": args.orders, "samples": args.samples,
              "commit_p99_budget_ms": args.p99_budget_ms, "scale_results": []}
    # Never inherit private subprocess-helper paths or opt-in fixture flags.
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(("OPENSQT_TEST_", "OPENSQT_LEDGER_"))}
    print(f"结果目录：{output}", flush=True)
    code = 1
    try:
        report["go_version"] = subprocess.check_output(["go", "version"], cwd=root, env=env, text=True).strip()
        # Executables stay in local temporary storage (target may be noexec).
        # Only the runtime tests use the target filesystem for their databases.
        with tempfile.TemporaryDirectory(prefix="opensqt-ledger-tools-") as tools_dir, \
             tempfile.TemporaryDirectory(prefix="data-", dir=output) as data_dir:
            runtime_env = dict(env, TMPDIR=data_dir, TMP=data_dir, TEMP=data_dir)
            packages = [("internal/fillledger", "^Test"),
                        ("order", "^Test(LedgerStorage|SubmissionBoundary)"),
                        ("position", "^Test(Ledger|RealTransition|RealTerminalCorrection)")]
            suffix = ".exe" if os.name == "nt" else ""
            for package, pattern in packages:
                name = package.split("/")[-1]
                binary = str(Path(tools_dir) / (name + suffix))
                print(f"检查 {package} 的离线故障与恢复场景…", flush=True)
                build = ["go", "test", "-c", "-o", binary]
                if not args.no_race:
                    build.append("-race")
                run_logged(build + ["./" + package], root, env, output / (name + "-build.log"))
                run_logged([binary, "-test.run=" + pattern, "-test.count=1", "-test.v", "-test.timeout=10m"],
                           root, runtime_env, output / (name + "-correctness.log"))
            binary = str(Path(tools_dir) / ("scale" + suffix))
            run_logged(["go", "test", "-c", "-o", binary, "./internal/fillledger"], root, env, output / "scale-build.log")
            runtime_env.update(OPENSQT_LEDGER_SCALE_ORDERS=str(args.orders),
                               OPENSQT_LEDGER_SCALE_SAMPLES=str(args.samples), OPENSQT_LEDGER_EXPORT_SAMPLES="1")
            print("测量同步提交、历史查询和新进程恢复…", flush=True)
            log = output / "scale.log"
            run_logged([binary, "-test.run=^TestSlotLedgerScale$", "-test.count=1", "-test.v", "-test.timeout=20m"],
                       root, runtime_env, log)
            rows = load_scale_results(log, args.samples)
            expected = [n for n in (1000, 10000, 100000, 1000000) if n <= args.orders]
            if [r["seed_orders"] for r in rows] != expected:
                raise ValueError("scale run did not produce every expected result")
            report["scale_results"] = rows
            report["queue_projections"] = [queue_projection(rows[-1]["sync_commit_samples_ms"], x, args.queue_capacity)
                                           for x in args.arrival_ms]
            exceeded = args.p99_budget_ms is not None and any(r["sync_commit"]["p99_ms"] > args.p99_budget_ms for r in rows)
            report["status"] = "budget_exceeded" if exceeded else "measured"
            code = 2 if exceeded else 0
    except (OSError, subprocess.SubprocessError, ValueError) as error:
        report.update(status="failed", error=str(error))
    except KeyboardInterrupt:
        report.update(status="interrupted", error="用户中断")
        code = 130
    write_report(output, report)
    print(f"{report['status']}：{output / 'REPORT.md'}", flush=True)
    return code


if __name__ == "__main__":
    sys.exit(main())
