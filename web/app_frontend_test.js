const test = require("node:test");
const assert = require("node:assert/strict");

const { buildKlineGridModel } = require("./static/app.js");

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
