const test = require("node:test");
const assert = require("node:assert/strict");

const { buildKlineGridModel, buildHourlyFillModel } = require("./static/app.js");

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

test("hourly fill model groups buy and sell counts and stats", () => {
    const now = new Date(2026, 7, 19, 15, 30, 0);
    const model = buildHourlyFillModel([
        { side: "BUY", filledAt: new Date(2026, 7, 19, 13, 10, 0), quantity: 0.01, realizedPnl: 0 },
        { side: "BUY", filledAt: new Date(2026, 7, 19, 13, 40, 0), quantity: 0.02, realizedPnl: 0 },
        { side: "SELL", filledAt: new Date(2026, 7, 19, 14, 5, 0), quantity: 0.01, realizedPnl: 0.12 },
        { side: "SKIP", filledAt: new Date(2026, 7, 19, 14, 20, 0), quantity: 1, realizedPnl: 9 },
        { side: "BUY", filledAt: "not-a-date", quantity: 1, realizedPnl: 0 }
    ], now);

    assert.equal(model.buckets.length, 3);
    assert.equal(model.buckets[0].buy, 2);
    assert.equal(model.buckets[0].sell, 0);
    assert.equal(model.buckets[1].buy, 0);
    assert.equal(model.buckets[1].sell, 1);
    assert.equal(model.buckets[2].buy, 0);
    assert.equal(model.stats.buyCount, 2);
    assert.equal(model.stats.sellCount, 1);
    assert.equal(model.stats.windowTotal, 3);
    assert.equal(model.stats.peakCount, 2);
    assert.equal(model.stats.peakHour, "13:00");
    assert.ok(Math.abs(model.stats.buyQty - 0.03) < 1e-12);
    assert.ok(Math.abs(model.stats.realizedPnl - 0.12) < 1e-12);
    assert.equal(model.maxCount, 2);
    assert.equal(model.buckets[0].label, "13:00");
});

test("hourly fill model is empty without valid fills", () => {
    const model = buildHourlyFillModel([], new Date(2026, 7, 19, 12, 0, 0));
    assert.deepEqual(model.buckets, []);
    assert.equal(model.stats.total, 0);
    assert.equal(model.maxCount, 0);
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
});
