const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");

const {
    buildKlineGridModel,
    buildHourlyFillModel,
    buildMarginUsageModel,
    buildSnapshotHealthModel,
    buildSnapshotAcceptanceModel,
    buildFreshnessModel,
	buildMakerExecutionModel,
    centeredScrollLeft,
    formatRelativeTime,
    candleChangePct,
    formatCandleChange,
    candleDetail
} = require("./static/app.js");

test("maker execution model reports quote health and strict-maker outcomes", () => {
	const healthy = buildMakerExecutionModel(
		{ bestBid: 100, bestAsk: 100.1, quoteAgeMs: 400 },
		{ makerExecution: {
			attempts: 20,
			accepted: 19,
			guardSkips: 1,
			postOnlyRejects: 0,
			catchUpOrders: 3,
			catchUpAbandoned: 1,
			activeCatchUp: 1
		} },
		{ quoteStaleMs: 1500, makerGuardTicks: 2, catchUpMode: "passive" }
	);
	assert.equal(healthy.state, "healthy");
	assert.equal(healthy.acceptanceRate, 95);
	assert.equal(healthy.quoteReady, true);
	assert.equal(healthy.activeCatchUp, 1);
	assert.equal(healthy.makerGuardTicks, 2);

	const stale = buildMakerExecutionModel(
		{ bestBid: 100, bestAsk: 100.1, quoteAgeMs: 1501 },
		{ makerExecution: { attempts: 4, accepted: 4 } },
		{ quoteStaleMs: 1500 }
	);
	assert.equal(stale.state, "warning");
	assert.equal(stale.statusText, "盘口陈旧 · 暂停新单");

	const lowAcceptance = buildMakerExecutionModel(
		{ bestBid: 100, bestAsk: 100.1, quoteAgeMs: 20 },
		{ makerExecution: { attempts: 10, accepted: 8, postOnlyRejects: 2 } },
		{ quoteStaleMs: 1500 }
	);
	assert.equal(lowAcceptance.state, "warning");
	assert.equal(lowAcceptance.acceptanceRate, 80);
	assert.equal(lowAcceptance.postOnlyRejects, 2);

	const waiting = buildMakerExecutionModel({}, {}, {});
	assert.equal(waiting.state, "waiting");
	assert.equal(waiting.acceptanceRate, null);
});

function fixture() {
    return {
        kline: {
            interval: "1m",
            historyReady: true,
            candles: [
                { time: 3000, open: 101, high: 104, low: 100, close: 103, isClosed: false },
                { time: 1000, open: 99, high: 102, low: 98, close: 101, isClosed: true },
                { time: 2000, open: 101, high: 103, low: 100, close: 102, isClosed: true }
            ]
        },
        position: {
            gridPrice: 100,
            orderQuantity: 30,
            slots: [
                {
                    price: 103,
                    priceText: "103.00",
                    positionStatus: "FILLED",
                    positionQty: 0.5,
                    orderSide: "SELL",
                    orderStatus: "PLACED",
                    inSellWindow: true
                },
                { price: 100, priceText: "100.00" },
                {
                    price: 99,
                    priceText: "99.00",
                    orderSide: "BUY",
                    orderStatus: "CONFIRMED",
                    inBuyWindow: true
                },
                { price: 97, priceText: "97.00" }
            ]
        }
    };
}

function cssBlock(source, marker) {
    const markerAt = source.indexOf(marker);
    assert.ok(markerAt >= 0, `missing CSS block: ${marker}`);
    const openAt = source.indexOf("{", markerAt + marker.length);
    assert.ok(openAt >= 0, `missing opening brace: ${marker}`);

    let depth = 0;
    for (let i = openAt; i < source.length; i++) {
        if (source[i] === "{") depth++;
        if (source[i] === "}" && --depth === 0) {
            return source.slice(openAt + 1, i);
        }
    }

    assert.fail(`unclosed CSS block: ${marker}`);
}

test("fill timestamps are formatted relative to now", () => {
    const now = new Date("2026-08-20T12:00:00.000Z");
    assert.equal(formatRelativeTime(new Date(now.getTime() - 45_000), now), "刚刚");
    assert.equal(formatRelativeTime(new Date(now.getTime() - 90_000), now), "1 分钟前");
    assert.equal(formatRelativeTime(new Date(now.getTime() - 3 * 3_600_000), now), "3 小时前");
    assert.equal(formatRelativeTime(new Date(now.getTime() + 2 * 86_400_000), now), "2 天后");
    assert.equal(formatRelativeTime("not-a-date", now), "—");
});

test("snapshot health detects an open but silent websocket at the dynamic threshold", () => {
    const healthy = buildSnapshotHealthModel({
        nowMs: 3_999,
        lastReceivedAtMs: 1_500,
        lastWebSocketReceivedAtMs: 1_500,
        socketOpenedAtMs: 1_000,
        socketState: "open",
        pushIntervalMs: 400
    });
    assert.equal(healthy.silenceThresholdMs, 2_500);
    assert.equal(healthy.socketSilent, false);
    assert.equal(healthy.connectionState, "live");
    assert.equal(healthy.shouldPoll, false);
    assert.equal(healthy.shouldReconnect, false);

    const silent = buildSnapshotHealthModel({
        nowMs: 4_000,
        lastReceivedAtMs: 1_500,
        lastWebSocketReceivedAtMs: 1_500,
        socketOpenedAtMs: 1_000,
        socketState: "open",
        pushIntervalMs: 400
    });
    assert.equal(silent.socketSilenceAgeMs, 2_500);
    assert.equal(silent.socketSilent, true);
    assert.equal(silent.connectionState, "fallback");
    assert.equal(silent.shouldPoll, true);
    assert.equal(silent.shouldReconnect, true);
});

test("snapshot health scales the watchdog and classifies fallback states", () => {
    const scaled = buildSnapshotHealthModel({
        nowMs: 5_999,
        lastReceivedAtMs: 2_000,
        lastWebSocketReceivedAtMs: 2_000,
        socketOpenedAtMs: 1_000,
        socketState: "open",
        pushIntervalMs: 1_000
    });
    assert.equal(scaled.silenceThresholdMs, 4_000);
    assert.equal(scaled.socketSilent, false);
    assert.equal(scaled.connectionState, "live");

    const awaitingFirstSnapshot = buildSnapshotHealthModel({
        nowMs: 3_499,
        lastReceivedAtMs: 0,
        lastWebSocketReceivedAtMs: 0,
        socketOpenedAtMs: 1_000,
        socketState: "open"
    });
    assert.equal(awaitingFirstSnapshot.socketSilent, false);
    assert.equal(awaitingFirstSnapshot.connectionState, "connecting");
    assert.equal(awaitingFirstSnapshot.shouldPoll, false);

    const noFirstSnapshot = buildSnapshotHealthModel({
        nowMs: 3_500,
        lastReceivedAtMs: 0,
        lastWebSocketReceivedAtMs: 0,
        socketOpenedAtMs: 1_000,
        socketState: "open"
    });
    assert.equal(noFirstSnapshot.silenceThresholdMs, 2_500);
    assert.equal(noFirstSnapshot.socketSilent, true);
    assert.equal(noFirstSnapshot.connectionState, "fallback");

    const restCannotMasqueradeAsWebSocket = buildSnapshotHealthModel({
        nowMs: 3_500,
        lastReceivedAtMs: 3_400,
        lastWebSocketReceivedAtMs: 0,
        socketOpenedAtMs: 1_000,
        socketState: "open"
    });
    assert.equal(restCannotMasqueradeAsWebSocket.snapshotAgeMs, 100);
    assert.equal(restCannotMasqueradeAsWebSocket.socketSilent, true);
    assert.equal(restCannotMasqueradeAsWebSocket.connectionState, "fallback");

    const connectingTimedOut = buildSnapshotHealthModel({
        nowMs: 9_000,
        lastReceivedAtMs: 8_900,
        lastWebSocketReceivedAtMs: 0,
        socketOpenedAtMs: 1_000,
        socketState: "connecting"
    });
    assert.equal(connectingTimedOut.socketConnectTimedOut, true);
    assert.equal(connectingTimedOut.shouldReconnect, true);
    assert.equal(connectingTimedOut.connectionState, "fallback");

    const closedWithRecentData = buildSnapshotHealthModel({
        nowMs: 5_000,
        lastReceivedAtMs: 4_500,
        socketState: "closed"
    });
    assert.equal(closedWithRecentData.connectionState, "fallback");
    assert.equal(closedWithRecentData.shouldPoll, true);

    const interrupted = buildSnapshotHealthModel({
        nowMs: 12_001,
        lastReceivedAtMs: 4_000,
        socketState: "closed"
    });
    assert.equal(interrupted.connectionState, "down");
});

test("freshness copy reports market data and only surfaces abnormal position freshness", () => {
    const normal = buildFreshnessModel({
        snapshotAgeMs: 600,
        tradeAgeMs: 800,
        positionReady: true,
        positionAgeMs: 2_500,
        positionThresholdMs: 2_500
    });
    assert.equal(normal.text, "行情 800ms 前 · 数据 600ms 前");
    assert.equal(normal.stale, false);
    assert.equal(normal.positionStale, false);

    const waiting = buildFreshnessModel({
        snapshotAgeMs: 100,
        tradeAgeMs: 200,
        positionReady: false,
        positionAgeMs: 100,
        positionThresholdMs: 2_500
    });
    assert.equal(waiting.text, "行情 200ms 前 · 数据 100ms 前 · 仓位等待");
    assert.equal(waiting.stale, true);
    assert.equal(waiting.positionStale, true);

    const stalePosition = buildFreshnessModel({
        snapshotAgeMs: 100,
        tradeAgeMs: 200,
        positionReady: true,
        positionAgeMs: 2_501,
        positionThresholdMs: 2_500
    });
    assert.equal(stalePosition.text, "行情 200ms 前 · 数据 100ms 前 · 仓位 3s 前");
    assert.equal(stalePosition.stale, true);

    const backwardCompatible = buildFreshnessModel({
        snapshotAgeMs: 100,
        tradeAgeMs: null
    });
    assert.equal(backwardCompatible.text, "等待行情 · 数据 100ms 前");
    assert.equal(backwardCompatible.positionStale, false);
});

test("startup keeps a newer websocket snapshot when the initial REST request finishes late", () => {
    const websocket = buildSnapshotAcceptanceModel(
        { time: "2026-09-02T08:00:00.400900Z" },
        null
    );
    assert.equal(websocket.valid, true);
    assert.equal(websocket.accepted, true);

    const delayedRest = buildSnapshotAcceptanceModel(
        { time: "2026-09-02T08:00:00.400100Z" },
        websocket.sourceOrder
    );
    assert.equal(Date.parse("2026-09-02T08:00:00.400900Z"), Date.parse("2026-09-02T08:00:00.400100Z"));
    assert.equal(delayedRest.valid, true);
    assert.equal(delayedRest.accepted, false);

    const duplicate = buildSnapshotAcceptanceModel(
        { time: "2026-09-02T08:00:00.400900Z" },
        websocket.sourceOrder
    );
    assert.equal(duplicate.accepted, false);
});

test("fallback and visibility recovery accept snapshots monotonically across transports", () => {
    const instance = "2026-09-02T08:00:00Z";
    const initialWebSocket = buildSnapshotAcceptanceModel({
        startedAt: instance,
        sequence: 1,
        time: "2026-09-02T08:01:00.000Z"
    }, null);
    const fallbackRest = buildSnapshotAcceptanceModel(
        { startedAt: instance, sequence: 2, time: "2026-09-02T08:01:00.200Z" },
        initialWebSocket.sourceOrder
    );
    assert.equal(fallbackRest.accepted, true);

    const resumedWebSocket = buildSnapshotAcceptanceModel(
        { startedAt: instance, sequence: 3, time: "2026-09-02T08:01:00.600Z" },
        fallbackRest.sourceOrder
    );
    assert.equal(resumedWebSocket.accepted, true);

    // This response can have the latest client receipt time while carrying an older server snapshot.
    const delayedVisibilityRest = buildSnapshotAcceptanceModel(
        { startedAt: instance, sequence: 2, time: "2026-09-02T08:01:00.300Z" },
        resumedWebSocket.sourceOrder
    );
    assert.equal(delayedVisibilityRest.accepted, false);

    const healthySocket = buildSnapshotHealthModel({
        nowMs: 10_500,
        lastReceivedAtMs: 10_400,
        lastWebSocketReceivedAtMs: 10_400,
        socketOpenedAtMs: 10_000,
        socketState: "open",
        pushIntervalMs: 400
    });
    assert.equal(healthySocket.connectionState, "live");
    assert.equal(healthySocket.shouldPoll, false);
});

test("snapshot sequence survives clock rollback and changes server generations safely", () => {
    const firstInstance = "2026-09-02T08:00:00Z";
    const secondInstance = "2026-09-02T07:00:00Z";
    const first = buildSnapshotAcceptanceModel({
        startedAt: firstInstance,
        sequence: 41,
        time: "2026-09-02T10:00:00Z"
    }, null);
    assert.equal(first.accepted, true);

    const clockRolledBack = buildSnapshotAcceptanceModel({
        startedAt: firstInstance,
        sequence: 42,
        time: "2026-09-02T09:00:00Z"
    }, first.sourceOrder);
    assert.equal(clockRolledBack.accepted, true);

    const duplicate = buildSnapshotAcceptanceModel({
        startedAt: firstInstance,
        sequence: 42,
        time: "2026-09-02T09:00:01Z"
    }, clockRolledBack.sourceOrder);
    assert.equal(duplicate.accepted, false);

    const restarted = buildSnapshotAcceptanceModel({
        startedAt: secondInstance,
        sequence: 1,
        time: "2026-09-02T08:00:00Z"
    }, clockRolledBack.sourceOrder, new Set(), true);
    assert.equal(restarted.accepted, true);
    assert.equal(restarted.instanceChanged, true);

    const delayedOldInstance = buildSnapshotAcceptanceModel({
        startedAt: firstInstance,
        sequence: 43,
        time: "2026-09-02T11:00:00Z"
    }, restarted.sourceOrder, new Set([firstInstance]));
    assert.equal(delayedOldInstance.valid, true);
    assert.equal(delayedOldInstance.accepted, false);
});

test("a delayed REST instance cannot retire the current websocket generation", () => {
    const instanceA = "instance-a";
    const instanceB = "instance-b";
    const instanceC = "instance-c";
    const first = buildSnapshotAcceptanceModel({
        startedAt: instanceA,
        sequence: 10,
        time: "2026-09-02T10:00:00Z"
    }, null, new Set(), true);

    const currentWebSocket = buildSnapshotAcceptanceModel({
        startedAt: instanceC,
        sequence: 1,
        time: "2026-09-02T10:00:02Z"
    }, first.sourceOrder, new Set(), true);
    assert.equal(currentWebSocket.accepted, true);
    assert.equal(currentWebSocket.instanceChanged, true);

    const retired = new Set([instanceA]);
    const delayedMiddleRest = buildSnapshotAcceptanceModel({
        startedAt: instanceB,
        sequence: 5,
        time: "2026-09-02T10:00:01Z"
    }, currentWebSocket.sourceOrder, retired, false);
    assert.equal(delayedMiddleRest.valid, true);
    assert.equal(delayedMiddleRest.accepted, false);

    const nextCurrentWebSocket = buildSnapshotAcceptanceModel({
        startedAt: instanceC,
        sequence: 2,
        time: "2026-09-02T10:00:03Z"
    }, currentWebSocket.sourceOrder, retired, true);
    assert.equal(nextCurrentWebSocket.accepted, true);
});

test("event stream follows execution tape at the end of the dashboard", () => {
    const html = fs.readFileSync(__dirname + "/static/index.html", "utf8");
	const maker = html.indexOf('id="makerPanel"');
    const fills = html.indexOf('id="section-fills"');
    const logs = html.indexOf('id="section-logs"');
    const mainEnd = html.indexOf("</main>");
	assert.ok(maker >= 0);
	assert.ok(fills > maker);
    assert.ok(logs > fills);
    assert.ok(mainEnd > logs);
	assert.ok(html.indexOf("03 / MAKER EXECUTION") < html.indexOf("04 / EXECUTION TAPE"));
	assert.ok(html.indexOf("04 / EXECUTION TAPE") < html.indexOf("05 / EVENT STREAM"));
});

test("margin capacity surface exposes status, exact amounts and an accessible gauge", () => {
    const html = fs.readFileSync(__dirname + "/static/index.html", "utf8");
    const css = fs.readFileSync(__dirname + "/static/app.css", "utf8");

    assert.match(html, /id="marginCapacity"/);
    assert.match(html, /持仓 \+ 挂单冻结/);
    assert.match(html, /id="marginCapacityStatus"[^>]*role="status"[^>]*aria-live="polite"/);
    assert.match(html, /id="marginTrack"[^>]*role="progressbar"/);
    assert.match(html, /id="marginUsedAmount"/);
    assert.match(html, /id="marginBalanceAmount"/);
    assert.match(html, /id="marginAvailableAmount"/);
    assert.match(cssBlock(css, ".margin-fill"), /transform:\s*scaleX\(var\(--margin-fill-scale\)\)/);
    assert.match(cssBlock(css, ".margin-facts"), /grid-template-columns:\s*repeat\(3,/);
});

test("margin usage model keeps backend trigger state authoritative", () => {
    const model = buildMarginUsageModel({
        ready: true,
        triggered: false,
        usagePercent: 72.5,
        limitPercent: 60,
        usedMargin: 1450,
        marginBalance: 2000,
        availableBalance: 550,
        quoteAsset: "USDT",
        updatedAt: "2026-08-27T10:00:00Z"
    });

    assert.equal(model.ready, true);
    assert.equal(model.state, "normal");
    assert.equal(model.triggered, false);
    assert.equal(model.statusText, "容量正常");
    assert.equal(model.fillPercent, 72.5);
    assert.equal(model.fillScale, 0.725);
    assert.equal(model.limitPosition, 60);
    assert.equal(model.usedMargin, 1450);
    assert.equal(model.quoteAsset, "USDT");
});

test("margin usage model reports a latched stop-and-cancel-request state", () => {
    const model = buildMarginUsageModel({
        ready: true,
        triggered: true,
        stale: true,
        usagePercent: 64.25,
        limitPercent: 60,
        usedMargin: 1285,
        marginBalance: 2000,
        availableBalance: 715,
        quoteAsset: "USDC"
    });

    assert.equal(model.state, "triggered");
    assert.equal(model.statusText, "限制已锁存 · 已停单");
    assert.match(model.noteText, /已请求全撤/);
    assert.match(model.noteText, /本次运行中保持锁存/);
    assert.equal(model.showReading, true);
    assert.equal(model.showAmounts, true);
    assert.equal(model.quoteAsset, "USDC");
});

test("margin usage model distinguishes waiting and stale readings from valid zero", () => {
    const uninitialized = buildMarginUsageModel({
        ready: false,
        stale: true,
        usagePercent: 0,
        limitPercent: 50,
        usedMargin: 0,
        marginBalance: 0,
        availableBalance: 0
    }, "USDT");
    assert.equal(uninitialized.state, "waiting");
    assert.equal(uninitialized.stale, false);
    assert.equal(uninitialized.showReading, false);

    const waiting = buildMarginUsageModel({
        ready: false,
        triggered: false,
        usagePercent: 0,
        limitPercent: 50,
        usedMargin: 0,
        marginBalance: 0,
        availableBalance: 0,
        error: "account unavailable"
    }, "USDT");
    assert.equal(waiting.state, "waiting");
    assert.equal(waiting.ready, false);
    assert.equal(waiting.showReading, false);
    assert.equal(waiting.showAmounts, false);
    assert.equal(waiting.statusText, "保证金读取异常");
    assert.match(waiting.noteText, /account unavailable/);

    const stale = buildMarginUsageModel({
        ready: false,
        stale: true,
        usagePercent: 18.75,
        limitPercent: 50,
        usedMargin: 375,
        marginBalance: 2000,
        availableBalance: 1625
    }, "USDT");
    assert.equal(stale.state, "stale");
    assert.equal(stale.showReading, true);
    assert.equal(stale.showAmounts, true);
    assert.equal(stale.statusText, "账户读数待更新");
});

test("margin gauge clamps visual geometry without changing exact readings", () => {
    const model = buildMarginUsageModel({
        ready: true,
        usagePercent: 132.4,
        limitPercent: 12,
        usedMargin: 1324,
        marginBalance: 1000,
        availableBalance: 0
    });

    assert.equal(model.usagePercent, 132.4);
    assert.equal(model.fillPercent, 100);
    assert.equal(model.fillScale, 1);
    assert.equal(model.limitPosition, 12);
    assert.equal(model.limitAtStart, true);
});

test("mobile order and log windows keep native vertical scrolling", () => {
    const css = fs.readFileSync(__dirname + "/static/app.css", "utf8");
    const html = fs.readFileSync(__dirname + "/static/index.html", "utf8");
    const mobile = cssBlock(
        css,
        "@media (max-width: 680px), (max-width: 900px) and (max-height: 500px)"
    );
    const orders = cssBlock(mobile, ".filled-orders-panel .table-wrap");
    const logs = cssBlock(mobile, ".activity .logs");

    for (const rule of [orders, logs]) {
        assert.match(rule, /max-height:\s*min\([^;]*dvh/);
        assert.doesNotMatch(rule, /max-height:\s*none/);
        assert.match(rule, /overflow-y:\s*auto/);
        assert.match(rule, /overflow-x:\s*hidden/);
        assert.match(rule, /overscroll-behavior-y:\s*auto/);
        assert.match(rule, /touch-action:\s*pan-y/);
        assert.match(rule, /-webkit-overflow-scrolling:\s*touch/);
    }

    const orderRegion = html.match(/<div class="table-wrap"[^>]*>/)?.[0];
    const logRegion = html.match(/<div class="logs"[^>]*>/)?.[0];
    for (const tag of [orderRegion, logRegion]) {
        assert.ok(tag);
        assert.match(tag, /tabindex="0"/);
        assert.match(tag, /aria-label="[^"]*可上下滑动"/);
    }
    assert.match(orderRegion, /role="region"/);
    assert.match(logRegion, /role="log"/);
});

test("mobile horizontal chart passes vertical gestures back to the page", () => {
    const css = fs.readFileSync(__dirname + "/static/app.css", "utf8");
    const chart = cssBlock(css, ".fill-hourly-chart");

    assert.match(chart, /overflow-x:\s*auto/);
    assert.match(chart, /overscroll-behavior-x:\s*contain/);
    assert.match(chart, /overscroll-behavior-y:\s*auto/);
    assert.match(chart, /touch-action:\s*pan-x pan-y/);
});

test("hour selection centers within scroll bounds", () => {
    assert.equal(centeredScrollLeft(300, 900, 44, 1100), 772);
    assert.equal(centeredScrollLeft(300, 20, 44, 1100), 0);
    assert.equal(centeredScrollLeft(300, 1090, 44, 1100), 800);
    assert.equal(centeredScrollLeft(400, 100, 44, 300), 0);
});

test("candles are normalized, ordered and summarized", () => {
    const data = fixture();
    const model = buildKlineGridModel(data.kline, data.position, false);

    assert.deepEqual(model.candles.map((item) => item.time), [1000000, 2000000, 3000000]);
    assert.equal(model.latest.close, 103);
    assert.equal(model.change, 4);
    assert.ok(Math.abs(model.changePct - 4 / 99 * 100) < 1e-12);
    assert.equal(model.candleLow, 98);
    assert.equal(model.candleHigh, 104);
    assert.ok(model.priceMin < 98);
    assert.ok(model.priceMax > 104);
});

test("single candle change percent is close versus open", () => {
    assert.equal(candleChangePct({ open: 100, close: 101 }), 1);
    assert.equal(candleChangePct({ open: 100, close: 99 }), -1);
    assert.equal(candleChangePct({ open: 100, close: 100 }), 0);
    assert.equal(candleChangePct({ open: 0, close: 1 }), null);
    assert.equal(candleChangePct({ open: 103.15, close: 103.32 }), (103.32 - 103.15) / 103.15 * 100);

    const up = formatCandleChange({ open: 103.15, close: 103.32 });
    assert.equal(up.text, "涨 +0.16%");
    assert.equal(up.tone, "pos");
    assert.equal(up.direction, "涨");

    const down = formatCandleChange({ open: 103.32, close: 103.15 });
    assert.equal(down.text, "跌 -0.16%");
    assert.equal(down.tone, "neg");
    assert.equal(down.direction, "跌");

    const flat = formatCandleChange({ open: 100, close: 100 });
    assert.equal(flat.text, "平 0.00%");
    assert.equal(flat.tone, "");
    assert.equal(flat.direction, "平");

    const tiny = formatCandleChange({ open: 100, close: 100.001 });
    assert.equal(tiny.text, "平 0.00%");
    assert.equal(tiny.tone, "");
});

test("kline inspect and tooltip include the candle change percent", () => {
    const js = fs.readFileSync(__dirname + "/static/app.js", "utf8");
    const css = fs.readFileSync(__dirname + "/static/app.css", "utf8");
    const detail = candleDetail({
        time: Date.UTC(2026, 8, 1, 8, 15, 0),
        open: 103.15,
        high: 103.38,
        low: 103.14,
        close: 103.32,
        isClosed: true
    }, 4);

    assert.match(detail, /涨 \+0\.16%/);
    assert.match(detail, /已完结/);
    assert.match(js, /function renderKlineDetail/);
    assert.match(js, /function positionChartTooltip[\s\S]*formatCandleChange\(candle\)/);
    assert.match(cssBlock(css, ".chart-tooltip-change.pos"), /var\(--positive\)/);
    assert.match(cssBlock(css, ".chart-tooltip-change.neg"), /var\(--negative\)/);
    assert.match(cssBlock(css, ".ladder-detail .kline-change.pos"), /var\(--positive\)/);
    assert.match(cssBlock(css, ".ladder-detail .kline-change.neg"), /var\(--negative\)/);
});

test("grid orders and positions become distinct K-line overlays", () => {
    const data = fixture();
    const model = buildKlineGridModel(data.kline, data.position, false);

    assert.equal(model.levels.length, 3);
    const sell = model.levels.find((item) => item.price === 103);
    const grid = model.levels.find((item) => item.price === 100);
    const buy = model.levels.find((item) => item.price === 99);
    assert.equal(sell.kind, "sell");
    assert.deepEqual(sell.markers, ["S", "P"]);
    assert.equal(grid.kind, "grid");
    assert.deepEqual(grid.markers, ["G"]);
    assert.equal(buy.kind, "buy");
    assert.deepEqual(buy.markers, ["B"]);
    assert.ok(Math.abs(buy.orderQuantity - 30 / 99) < 1e-12);
    assert.deepEqual(model.execution, { buy: 1, sell: 1, position: 1 });
});

test("outside grid levels are opt-in", () => {
    const data = fixture();
    const hidden = buildKlineGridModel(data.kline, data.position, false);
    const visible = buildKlineGridModel(data.kline, data.position, true);

    assert.equal(hidden.levels.some((item) => item.price === 97), false);
    const outside = visible.levels.find((item) => item.price === 97);
    assert.ok(outside);
    assert.equal(outside.kind, "outside");
    assert.deepEqual(outside.markers, []);
});

test("price scale follows candles instead of far grid slots", () => {
    const data = fixture();
    data.position.priceInterval = 1;
    data.position.slots.push(
        {
            price: 70,
            priceText: "70.00",
            orderSide: "BUY",
            orderStatus: "PLACED",
            inBuyWindow: true
        },
        {
            price: 130,
            priceText: "130.00",
            orderSide: "SELL",
            orderStatus: "PLACED",
            inSellWindow: true
        }
    );
    const model = buildKlineGridModel(data.kline, data.position, false);

    assert.ok(model.levels.some((item) => item.price === 70));
    assert.ok(model.levels.some((item) => item.price === 130));
    assert.ok(model.priceMin > 85, "far buy slot should not stretch the Y axis");
    assert.ok(model.priceMax < 120, "far sell slot should not stretch the Y axis");
    assert.ok(model.priceMin < 98);
    assert.ok(model.priceMax > 104);
    assert.equal(model.overflowBelow.some((item) => item.price === 70), true);
    assert.equal(model.overflowAbove.some((item) => item.price === 130), true);
    assert.equal(model.visibleLevels.some((item) => item.price === 99), true);
    assert.ok(model.railLabels.some((item) => item.isGrid));
    assert.ok(model.railLabels.some((item) => item.hasBuy));
    assert.ok(model.railLabels.some((item) => item.hasSell));
});

test("edge grid slots fold into overflow while nearest orders stay labeled", () => {
    const data = fixture();
    data.position.priceInterval = 1;
    data.position.slots.push(
        {
            price: 96.5,
            priceText: "96.50",
            orderSide: "BUY",
            orderStatus: "PLACED",
            inBuyWindow: true
        },
        {
            price: 105.6,
            priceText: "105.60",
            orderSide: "SELL",
            orderStatus: "PLACED",
            inSellWindow: true
        }
    );
    const model = buildKlineGridModel(data.kline, data.position, false);
    const last = model.latest.close;

    assert.ok(model.overflowBelow.some((item) => item.price === 96.5));
    assert.ok(model.overflowAbove.some((item) => item.price === 105.6));
    assert.equal(model.railLabels.some((item) => item.price === 96.5), false);
    assert.equal(model.railLabels.some((item) => item.price === 105.6), false);
    assert.ok(model.railLabels.some((item) => item.hasBuy && Math.abs(item.price - last) <= Math.abs(99 - last)));
    assert.ok(model.railLabels.some((item) => item.hasSell));
});

test("visible candle count is capped for real-time canvas performance", () => {
    const candles = Array.from({ length: 520 }, (_, index) => ({
        time: 1_700_000_000_000 + index * 60_000,
        open: 100,
        high: 102,
        low: 99,
        close: 101
    }));
    const model = buildKlineGridModel({ interval: "1m", candles }, { slots: [] }, false);

    assert.equal(model.candles.length, 500);
    assert.equal(model.candles[0].time, candles[20].time);
});

function hourBucket(model, date) {
    return model.buckets.find((item) => item.hour.getTime() === date.getTime());
}

test("hourly fill model groups buy and sell counts and stats", () => {
    const now = new Date(2026, 7, 19, 15, 30, 0);
    const model = buildHourlyFillModel([
        { side: "BUY", filledAt: new Date(2026, 7, 19, 13, 10, 0), quantity: 0.01, realizedPnl: 0 },
        { side: "BUY", filledAt: new Date(2026, 7, 19, 13, 40, 0), quantity: 0.02, realizedPnl: 0 },
        { side: "SELL", filledAt: new Date(2026, 7, 19, 14, 5, 0), quantity: 0.01, realizedPnl: 0.12 },
        { side: "SKIP", filledAt: new Date(2026, 7, 19, 14, 20, 0), quantity: 1, realizedPnl: 9 },
        { side: "BUY", filledAt: "not-a-date", quantity: 1, realizedPnl: 0 }
    ], now);

    assert.equal(model.buckets.length, 24);
    const thirteen = hourBucket(model, new Date(2026, 7, 19, 13, 0, 0));
    const fourteen = hourBucket(model, new Date(2026, 7, 19, 14, 0, 0));
    const fifteen = hourBucket(model, new Date(2026, 7, 19, 15, 0, 0));
    assert.equal(thirteen.buy, 2);
    assert.equal(thirteen.sell, 0);
    assert.equal(fourteen.buy, 0);
    assert.equal(fourteen.sell, 1);
    assert.equal(fifteen.buy, 0);
    assert.equal(model.stats.buyCount, 2);
    assert.equal(model.stats.sellCount, 1);
    assert.equal(model.stats.windowTotal, 3);
    assert.equal(model.stats.peakCount, 2);
    assert.equal(model.stats.peakHour, "13:00");
    assert.ok(Math.abs(model.stats.buyQty - 0.03) < 1e-12);
    assert.ok(Math.abs(model.stats.realizedPnl - 0.12) < 1e-12);
    assert.equal(model.maxCount, 2);
    assert.equal(thirteen.label, "13:00");
});

test("hourly fill model always renders 24 empty hours without valid fills", () => {
    const now = new Date(2026, 7, 19, 12, 0, 0);
    const model = buildHourlyFillModel([], now);
    assert.equal(model.buckets.length, 24);
    assert.equal(model.stats.total, 0);
    assert.equal(model.stats.windowTotal, 0);
    assert.equal(model.maxCount, 0);
    assert.equal(model.buckets[0].hour.getTime(), new Date(2026, 7, 18, 13, 0, 0).getTime());
    assert.equal(model.buckets[23].hour.getTime(), now.getTime());
});

test("hourly fill model keeps the most recent 24 hours", () => {
    const now = new Date(2026, 7, 20, 10, 15, 0);
    const model = buildHourlyFillModel([
        { side: "BUY", filledAt: new Date(2026, 7, 19, 8, 0, 0), quantity: 1 },
        { side: "SELL", filledAt: new Date(2026, 7, 20, 9, 20, 0), quantity: 1, realizedPnl: 0.4 }
    ], now, { maxHours: 24 });

    assert.equal(model.buckets.length, 24);
    assert.equal(model.stats.buyCount, 0);
    assert.equal(model.stats.sellCount, 1);
    assert.equal(model.stats.windowTotal, 1);
    assert.equal(model.stats.peakHour, "09:00");
    assert.equal(model.buckets[0].hour.getTime(), new Date(2026, 7, 19, 11, 0, 0).getTime());
    assert.equal(model.buckets[23].hour.getTime(), new Date(2026, 7, 20, 10, 0, 0).getTime());
});

test("hourly fill model prefers snapshot 24h buckets over recent order list", () => {
    const now = new Date(2026, 7, 20, 10, 15, 0);
    const model = buildHourlyFillModel([
        { side: "BUY", filledAt: new Date(2026, 7, 20, 10, 5, 0), quantity: 0.01 }
    ], now, {
        buckets: [
            { hour: new Date(2026, 7, 19, 12, 0, 0), buy: 4, sell: 1, buyQty: 0.04, sellQty: 0.01, pnl: 0.2 },
            { hour: new Date(2026, 7, 20, 9, 0, 0), buy: 0, sell: 3, buyQty: 0, sellQty: 0.03, pnl: 0.5 }
        ]
    });

    assert.equal(model.buckets.length, 24);
    assert.equal(model.stats.buyCount, 4);
    assert.equal(model.stats.sellCount, 4);
    assert.equal(model.stats.windowTotal, 8);
    assert.equal(hourBucket(model, new Date(2026, 7, 19, 12, 0, 0)).buy, 4);
    assert.equal(hourBucket(model, new Date(2026, 7, 20, 9, 0, 0)).sell, 3);
    assert.equal(hourBucket(model, new Date(2026, 7, 20, 10, 0, 0)).buy, 0);
});
