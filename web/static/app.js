(function () {
    if (typeof module !== "undefined" && module.exports) {
        module.exports = { buildKlineGridModel, buildHourlyFillModel };
        return;
    }

    const params = new URLSearchParams(location.search);
    const queryToken = params.get("token") || "";
    let token = queryToken || readSessionToken() || readLegacyToken() || "";
    let ws = null;
    let reconnectTimer = null;
    let restTimer = null;
    let freshnessTimer = null;
    let renderFrame = null;
    let pendingSnapshot = null;
    let latestSnapshot = null;
    let showOutside = false;
    let authBlocked = false;
    let lastSnapshotAt = 0;
    let lastPriceAgeBase = null;
    let lastPriceAgeAt = 0;
    let lastRenderedPrice = null;
    let chartModel = null;
    let chartGeometry = null;
    let chartSelection = null;
    let chartTooltipActive = false;
    let chartPinned = false;
    let chartPointer = null;
    let chartResizeObserver = null;
    let selectedHourKey = null;
    let toastTimer = null;
    const renderKeys = Object.create(null);

    const $ = (id) => document.getElementById(id);
    const connectionClasses = ["live", "fallback", "connecting", "down"];

    if (queryToken) {
        params.delete("token");
        const query = params.toString();
        history.replaceState(null, "", location.pathname + (query ? "?" + query : "") + location.hash);
    }
    removeLegacyToken();

    $("showOutside").addEventListener("change", (event) => {
        showOutside = event.target.checked;
        renderKeys["market-chart"] = "";
        if (latestSnapshot) {
            renderKlineChart(latestSnapshot.kline || {}, latestSnapshot.position || {});
        }
    });

    $("tokenForm").addEventListener("submit", async (event) => {
        event.preventDefault();
        const candidate = $("tokenInput").value.trim();
        if (!candidate) {
            $("tokenErr").textContent = "请输入访问令牌";
            $("tokenInput").focus();
            return;
        }

        token = candidate;
        authBlocked = false;
        setTokenLoading(true);
        $("tokenErr").textContent = "";
        const ok = await pullRest();
        setTokenLoading(false);
        if (!ok) return;

        saveSessionToken(token);
        hideTokenModal();
        connect();
    });

    function readSessionToken() {
        try {
            return sessionStorage.getItem("opensqt_dash_token") || "";
        } catch (_error) {
            return "";
        }
    }

    function readLegacyToken() {
        try {
            return localStorage.getItem("opensqt_dash_token") || "";
        } catch (_error) {
            return "";
        }
    }

    function saveSessionToken(value) {
        try {
            sessionStorage.setItem("opensqt_dash_token", value);
        } catch (_error) {
            // The dashboard still works when session storage is unavailable.
        }
    }

    function removeLegacyToken() {
        try {
            localStorage.removeItem("opensqt_dash_token");
        } catch (_error) {
            // Ignore storage restrictions.
        }
    }

    function setTokenLoading(loading) {
        const button = $("tokenSubmit");
        button.disabled = loading;
        button.textContent = loading ? "验证中…" : "验证并进入";
        $("tokenInput").disabled = loading;
    }

    function showTokenModal(message) {
        authBlocked = true;
        clearTimeout(reconnectTimer);
        if (ws) {
            ws.close();
            ws = null;
        }
        const mask = $("tokenMask");
        mask.classList.remove("hidden");
        mask.setAttribute("aria-hidden", "false");
        $("tokenErr").textContent = message || "";
        requestAnimationFrame(() => $("tokenInput").focus());
    }

    function hideTokenModal() {
        const mask = $("tokenMask");
        mask.classList.add("hidden");
        mask.setAttribute("aria-hidden", "true");
        $("tokenErr").textContent = "";
    }

    function snapshotHeaders() {
        return token ? { "X-Dashboard-Token": token } : {};
    }

    function websocketURL() {
        const proto = location.protocol === "https:" ? "wss:" : "ws:";
        const url = new URL(proto + "//" + location.host + "/ws");
        if (token) url.searchParams.set("token", token);
        return url.toString();
    }

    function fmt(value, digits) {
        if (value === undefined || value === null || Number.isNaN(Number(value))) return "—";
        return Number(value).toLocaleString("en-US", {
            minimumFractionDigits: digits ?? 2,
            maximumFractionDigits: digits ?? 2
        });
    }

    function ago(milliseconds) {
        if (milliseconds === undefined || milliseconds === null) return "";
        const ms = Math.max(0, Math.round(milliseconds));
        if (ms < 1000) return ms + "ms 前";
        if (ms < 60000) return Math.round(ms / 1000) + "s 前";
        return Math.round(ms / 60000) + "m 前";
    }

    function isActive(status) {
        return status === "PLACED" || status === "CONFIRMED" || status === "PARTIALLY_FILLED";
    }

    function setConnectionState(state, label) {
        const pill = $("wsPill");
        connectionClasses.forEach((name) => pill.classList.remove(name));
        pill.classList.add(state);
        setText(pill.querySelector("span"), label);
    }

    function setFreshness(snapshot) {
        lastSnapshotAt = Date.now();
        const price = snapshot.price || {};
        lastPriceAgeBase = price.ageMs == null ? null : Number(price.ageMs);
        lastPriceAgeAt = Date.now();
        updateFreshness();
    }

    function updateFreshness() {
        if (!latestSnapshot) return;
        const now = Date.now();
        const snapshotAge = now - lastSnapshotAt;
        const quoteAge = lastPriceAgeBase == null ? null : lastPriceAgeBase + (now - lastPriceAgeAt);
        const meta = $("priceMeta");
        meta.textContent = quoteAge == null
            ? "等待价格 · 数据 " + ago(snapshotAge)
            : "报价 " + ago(quoteAge) + " · 数据 " + ago(snapshotAge);
        meta.classList.toggle("stale", snapshotAge > 5000 || quoteAge > 5000);

        if (!ws || ws.readyState !== WebSocket.OPEN) {
            if (snapshotAge > 8000) setConnectionState("down", "数据已中断");
            else if (!authBlocked) setConnectionState("fallback", "轮询回退");
        }
    }

    function element(tag, className, text) {
        const node = document.createElement(tag);
        if (className) node.className = className;
        if (text !== undefined && text !== null) node.textContent = String(text);
        return node;
    }

    function setText(node, value) {
        const text = value === undefined || value === null ? "" : String(value);
        if (node.textContent !== text) node.textContent = text;
    }

    function replaceChildren(container, nodes) {
        const fragment = document.createDocumentFragment();
        nodes.forEach((node) => fragment.appendChild(node));
        container.replaceChildren(fragment);
    }

    function updateSection(key, value, callback) {
        const signature = JSON.stringify(value);
        if (renderKeys[key] === signature) return;
        renderKeys[key] = signature;
        callback();
    }

    function scheduleRender(snapshot) {
        if (!snapshot) return;
        pendingSnapshot = snapshot;
        if (renderFrame !== null) return;
        renderFrame = requestAnimationFrame(() => {
            renderFrame = null;
            const next = pendingSnapshot;
            pendingSnapshot = null;
            render(next);
        });
    }

    function render(snapshot) {
        if (!snapshot) return;
        latestSnapshot = snapshot;
        setFreshness(snapshot);

        const app = snapshot.app || {};
        const pos = snapshot.position || {};
        const risk = snapshot.risk || {};
        const acc = snapshot.account || {};
        const price = snapshot.price || {};
        const dec = pos.priceDecimals ?? 2;
        const quote = acc.quoteAsset || "USDT";

        setText($("pair"), (app.exchange || "—") + " · " + (app.symbol || pos.symbol || "—"));
        const priceNode = $("lastPrice");
        const priceText = price.lastText || fmt(price.last, dec);
        const currentPrice = Number(price.last);
        setText(priceNode, priceText);
        priceNode.setAttribute("aria-label", "最新价格 " + priceText);
        if (Number.isFinite(currentPrice)) {
            if (lastRenderedPrice !== null && currentPrice !== lastRenderedPrice) {
                priceNode.classList.toggle("price-up", currentPrice > lastRenderedPrice);
                priceNode.classList.toggle("price-down", currentPrice < lastRenderedPrice);
            }
            lastRenderedPrice = currentPrice;
        }
        const uptimeText = formatDuration(snapshot.uptimeSec);
        setText(
            $("version"),
            (snapshot.version || "") + (uptimeText === "—" ? "" : " · 已运行 " + uptimeText)
        );

        const triggered = Boolean(risk.triggered);
        const riskPill = $("riskPill");
        setText(riskPill.querySelector("span") || riskPill, !risk.enabled
            ? "风控关闭"
            : (triggered ? "风控已触发 · 暂停买单" : "风控正常"));
        riskPill.classList.toggle("hot", triggered);
        riskPill.classList.toggle("ok", Boolean(risk.enabled) && !triggered);

        const realized = pos.realizedPnl != null ? pos.realizedPnl : 0;
        const mark = Number(price.last || pos.lastPrice);
        const positionValue = Number(pos.positionValue) > 0
            ? Number(pos.positionValue)
            : (Number(pos.positionQty) > 0 && Number.isFinite(mark) && mark > 0
                ? Number(pos.positionQty) * mark
                : 0);
        const positionValueText = positionValue > 0 ? fmt(positionValue) + " " + quote : "";
        const initialMarginReady = Boolean(acc.initialMarginReady);
        const marginChange = Number(acc.marginChange || 0);
        const marginChangeClass = acc.stale
            ? "warn"
            : (marginChange > 0 ? "pos" : (marginChange < 0 ? "neg" : ""));
        const marginChangeHint = initialMarginReady
            ? "启动 " + fmt(acc.initialMargin) + " · 变化 " + fmtSigned(marginChange) +
                (Number(acc.initialMargin) !== 0 ? " (" + fmtSigned(acc.marginChangePct) + "%)" : "")
            : "等待启动余额";
        const primaryItems = [
            metric("可用余额", fmt(acc.available) + " " + quote, acc.stale ? "warn" : ""),
            metric("当前保证金余额", fmt(acc.margin) + " " + quote, acc.stale ? "warn" : "", marginChangeHint, marginChangeClass),
            metric("未实现盈亏", fmt(acc.unrealizedPnl) + " " + quote, (acc.unrealizedPnl || 0) >= 0 ? "pos" : "neg"),
            metric("已实现盈亏", fmt(realized) + " " + quote, realized >= 0 ? "pos" : "neg"),
            metric(
                "持仓",
                (pos.filledSlotCount || 0) + " 槽 · " + fmt(pos.positionQty, pos.quantityDecimals || 4) +
                    (pos.baseAsset ? " " + pos.baseAsset : ""),
                "",
                positionValueText
            ),
            metric("活动买 / 卖", (pos.activeBuyOrders || 0) + " / " + (pos.activeSellOrders || 0))
        ];
        const estimated = pos.estimatedProfit || 0;
        const strategyItems = [
            metric("程序启动时间", formatDateTime(snapshot.startedAt)),
            metric("运行时长", uptimeText),
            metric("启动保证金余额", initialMarginReady ? fmt(acc.initialMargin) + " " + quote : "等待首次读取", initialMarginReady ? "" : "warn"),
            metric("预计盈利", fmt(estimated) + " " + quote, estimated >= 0 ? "pos" : "neg"),
            metric("累计买 / 卖", fmt(pos.totalBuyQty, 4) + " / " + fmt(pos.totalSellQty, 4)),
            metric("最近对账", formatTime(pos.lastReconcileTime)),
            metric("保证金锁", pos.marginLocked ? fmt(pos.marginLockRemainingSec, 0) + "s" : "未锁定", pos.marginLocked ? "warn" : ""),
            metric("网格 / 间距", fmt(pos.gridPrice, dec) + " / " + fmt(pos.priceInterval, dec)),
            metric("每单金额", fmt(pos.orderQuantity || app.orderQuantity) + " " + quote),
            metric("窗口 买 / 卖", (pos.buyWindowSize || 0) + " / " + (pos.sellWindowSize || 0))
        ];

        updateSection("primary-kpis", primaryItems, () => renderMetrics($("kpis"), primaryItems));
        updateSection("strategy-kpis", strategyItems, () => renderMetrics($("strategyKpis"), strategyItems));
        $("strategySummary").textContent =
            "已运行 " + uptimeText +
            " · 网格 " + fmt(pos.gridPrice, dec) +
            " · 间距 " + fmt(pos.priceInterval, dec) +
            " · 每单 " + fmt(pos.orderQuantity || app.orderQuantity) + " " + quote;

        updateSection("market-chart", {
            kline: snapshot.kline || {},
            slots: pos.slots || [],
            gridPrice: pos.gridPrice,
            priceDecimals: pos.priceDecimals,
            quantityDecimals: pos.quantityDecimals,
            orderQuantity: pos.orderQuantity,
            showOutside
        }, () => renderKlineChart(snapshot.kline || {}, pos));
        updateSection("risk", risk, () => renderRisk(risk));
        updateSection("logs", snapshot.logs || [], () => renderLogs(snapshot.logs || []));
        updateSection("tables", {
            filledOrders: pos.filledOrders || [],
            filledHourly: pos.filledHourly || [],
            filledOrderCount: pos.filledOrderCount,
            priceDecimals: pos.priceDecimals,
            quantityDecimals: pos.quantityDecimals,
            quote
        }, () => renderTables(pos, quote));
    }

    function metric(label, value, className, hint, hintClassName) {
        return {
            label,
            value,
            className: className || "",
            hint: hint || "",
            hintClassName: hintClassName || ""
        };
    }

    function renderMetrics(container, items) {
        const cards = items.map((item) => {
            const card = element("div", "kpi");
            card.setAttribute("aria-label", item.label + "：" + item.value + (item.hint ? "；" + item.hint : ""));
            const label = element("div", "lab", item.label);
            const value = element("div", "val" + (item.className ? " " + item.className : ""), item.value);
            card.append(label, value);
            if (item.hint) {
                card.appendChild(element(
                    "div",
                    "hint" + (item.hintClassName ? " " + item.hintClassName : ""),
                    item.hint
                ));
            }
            return card;
        });
        replaceChildren(container, cards);
        container.classList.remove("is-loading");
        container.setAttribute("aria-busy", "false");
    }

    function buildKlineGridModel(kline, pos, includeOutside) {
        const candleMap = new Map();
        (kline.candles || []).forEach((item) => {
            let time = Number(item.time);
            const open = Number(item.open);
            const high = Number(item.high);
            const low = Number(item.low);
            const close = Number(item.close);
            if (time > 0 && time < 100000000000) time *= 1000;
            if (![time, open, high, low, close].every(Number.isFinite) ||
                time <= 0 || open <= 0 || high <= 0 || low <= 0 || close <= 0) return;
            candleMap.set(time, {
                time,
                open,
                high: Math.max(high, open, close),
                low: Math.min(low, open, close),
                close,
                volume: Number(item.volume || 0),
                isClosed: Boolean(item.isClosed)
            });
        });
        const candles = Array.from(candleMap.values())
            .sort((a, b) => a.time - b.time)
            .slice(-500);

        const grid = Number(pos.gridPrice);
        const orderValue = Number(pos.orderQuantity || 0);
        const levels = [];
        (pos.slots || []).forEach((slot) => {
            const price = Number(slot.price);
            if (!Number.isFinite(price) || price <= 0) return;
            const isGrid = Number.isFinite(grid) && Math.abs(price - grid) <= 1e-12;
            const inWindow = Boolean(slot.inBuyWindow || slot.inSellWindow);
            if (!includeOutside && !inWindow && !isGrid) return;

            const hasPosition = slot.positionStatus === "FILLED" && Number(slot.positionQty || 0) > 0;
            const hasBuy = slot.orderSide === "BUY" && isActive(slot.orderStatus);
            const hasSell = slot.orderSide === "SELL" && isActive(slot.orderStatus);
            const markers = [];
            if (isGrid) markers.push("G");
            if (hasBuy) markers.push("B");
            if (hasSell) markers.push("S");
            if (hasPosition) markers.push("P");

            let kind = "outside";
            if (isGrid) kind = "grid";
            else if (hasSell) kind = "sell";
            else if (hasPosition) kind = "position";
            else if (hasBuy) kind = "buy";
            else if (inWindow) kind = "empty";

            levels.push({
                price,
                priceText: slot.priceText || "",
                kind,
                markers,
                isGrid,
                hasBuy,
                hasSell,
                hasPosition,
                inWindow,
                orderQuantity: (hasBuy || hasSell) && orderValue > 0 ? orderValue / price : 0,
                positionQuantity: Number(slot.positionQty || 0),
                slot
            });
        });
        levels.sort((a, b) => b.price - a.price);

        const first = candles[0];
        const latest = candles[candles.length - 1];
        const change = first && latest ? latest.close - first.open : 0;
        const changePct = first && first.open ? change / first.open * 100 : 0;
        const candleLow = candles.length ? Math.min(...candles.map((candle) => candle.low)) : 0;
        const candleHigh = candles.length ? Math.max(...candles.map((candle) => candle.high)) : 0;
        const focused = focusKlinePriceRange(candleLow, candleHigh, levels, Number(pos.priceInterval));
        const uniqueLevels = mergeLevelsByPrice(levels);
        const split = splitLevelsByRange(uniqueLevels, focused.priceMin, focused.priceMax);
        const lastPrice = latest ? latest.close : 0;
        const keyLevels = selectKeyLevels(split.visible, lastPrice);
        const folded = foldEdgeLevels(split.visible, keyLevels, focused.priceMin, focused.priceMax);
        const execution = levels.reduce((result, level) => {
            if (level.hasBuy) result.buy += 1;
            if (level.hasSell) result.sell += 1;
            if (level.hasPosition) result.position += 1;
            return result;
        }, { buy: 0, sell: 0, position: 0 });
        return {
            interval: kline.interval || "5m",
            historyReady: Boolean(kline.historyReady),
            degraded: Boolean(kline.degraded),
            candles,
            levels,
            visibleLevels: folded.keep,
            railLabels: keyLevels.filter((level) => folded.keep.indexOf(level) >= 0),
            overflowAbove: split.above.concat(folded.above),
            overflowBelow: split.below.concat(folded.below),
            grid,
            priceMin: focused.priceMin,
            priceMax: focused.priceMax,
            latest,
            candleLow,
            candleHigh,
            change,
            changePct,
            execution
        };
    }

    function focusKlinePriceRange(candleLow, candleHigh, levels, interval) {
        const prices = [];
        if (candleLow > 0) prices.push(candleLow);
        if (candleHigh > 0) prices.push(candleHigh);
        if (!prices.length) {
            (levels || []).forEach((level) => prices.push(level.price));
            if (!prices.length) return { priceMin: 0, priceMax: 0 };
            const min = Math.min(...prices);
            const max = Math.max(...prices);
            const span = Math.max(max - min, max * 0.002);
            return { priceMin: min - span * 0.08, priceMax: max + span * 0.08 };
        }
        const lo = Math.min(candleLow, candleHigh);
        const hi = Math.max(candleLow, candleHigh);
        const step = Number.isFinite(interval) && interval > 0 ? interval : 0;
        const span = Math.max(hi - lo, hi * 0.002, step);
        const pad = Math.max(span * 0.18, step * 0.55, hi * 0.0006);
        let priceMin = lo - pad;
        let priceMax = hi + pad;
        if (priceMax <= priceMin) priceMax = priceMin + span;
        return { priceMin, priceMax };
    }

    function selectKeyLevels(levels, lastPrice) {
        const keys = [];
        const add = (level) => {
            if (level && keys.indexOf(level) < 0) keys.push(level);
        };
        add((levels || []).find((level) => level.isGrid));
        let nearestBuy = null;
        let nearestSell = null;
        (levels || []).forEach((level) => {
            if (level.hasBuy && (!nearestBuy || Math.abs(level.price - lastPrice) < Math.abs(nearestBuy.price - lastPrice))) {
                nearestBuy = level;
            }
            if (level.hasSell && (!nearestSell || Math.abs(level.price - lastPrice) < Math.abs(nearestSell.price - lastPrice))) {
                nearestSell = level;
            }
        });
        add(nearestBuy);
        add(nearestSell);
        return keys;
    }

    function foldEdgeLevels(visible, keyLevels, priceMin, priceMax) {
        const span = Math.max(priceMax - priceMin, 1e-9);
        const edge = span * 0.12;
        const keySet = new Set(keyLevels || []);
        const keep = [];
        const above = [];
        const below = [];
        (visible || []).forEach((level) => {
            if (keySet.has(level)) {
                keep.push(level);
                return;
            }
            if (priceMax - level.price <= edge) {
                above.push(level);
                return;
            }
            if (level.price - priceMin <= edge) {
                below.push(level);
                return;
            }
            keep.push(level);
        });
        return { keep, above, below };
    }

    function mergeLevelsByPrice(levels) {
        const merged = new Map();
        (levels || []).forEach((level) => {
            const key = Number(level.price).toFixed(8);
            const current = merged.get(key);
            if (!current) {
                merged.set(key, {
                    ...level,
                    markers: (level.markers || []).slice()
                });
                return;
            }
            current.markers = Array.from(new Set(current.markers.concat(level.markers || [])));
            current.hasBuy = current.hasBuy || level.hasBuy;
            current.hasSell = current.hasSell || level.hasSell;
            current.hasPosition = current.hasPosition || level.hasPosition;
            current.isGrid = current.isGrid || level.isGrid;
            current.inWindow = current.inWindow || level.inWindow;
            if (current.isGrid) current.kind = "grid";
            else if (current.hasSell) current.kind = "sell";
            else if (current.hasPosition) current.kind = "position";
            else if (current.hasBuy) current.kind = "buy";
        });
        return Array.from(merged.values());
    }

    function splitLevelsByRange(levels, priceMin, priceMax) {
        const visible = [];
        const above = [];
        const below = [];
        (levels || []).forEach((level) => {
            if (level.price > priceMax) above.push(level);
            else if (level.price < priceMin) below.push(level);
            else visible.push(level);
        });
        return { visible, above, below };
    }

    function renderKlineChart(kline, pos) {
        chartModel = buildKlineGridModel(kline, pos, showOutside);
        if (chartSelection !== null && chartSelection >= chartModel.candles.length) {
            chartSelection = Math.max(0, chartModel.candles.length - 1);
        }
        const empty = $("klineEmpty");
        const interval = chartModel.interval;
        const intervalNode = $("klineInterval");
        setText(intervalNode, interval + " · " + chartModel.candles.length + " 根");
        intervalNode.classList.toggle("warn", chartModel.degraded);

        if (!chartModel.candles.length) {
            empty.hidden = false;
            setText(empty, chartModel.degraded
                ? "历史 K 线暂不可用，正在从实时行情积累"
                : "正在加载真实 K 线数据");
            setText($("klineSummary"), chartModel.degraded
                ? "历史行情读取失败；当前价格流仍在记录新的真实蜡烛。"
                : "正在加载真实 OHLC 行情…");
            renderKlineStats(null, pos.priceDecimals ?? 2);
            setText($("klineDetail"), "获得首根 K 线后即可查看开、高、低、收明细");
            $("klineCanvas").setAttribute("aria-label", "K 线数据尚未就绪");
            syncKlineStepper();
            drawKlineChart();
            return;
        }

        empty.hidden = true;
        const decimals = pos.priceDecimals ?? 2;
        const latest = chartModel.latest;
        const direction = chartModel.change > 0 ? "上涨" : (chartModel.change < 0 ? "下跌" : "持平");
        const degradedText = chartModel.degraded ? " · 历史读取降级" : "";
        const summary = interval + " K 线 · " + chartModel.candles.length + " 根" +
            " · 最新 " + fmt(latest.close, decimals) +
            " · 显示区间 " + fmt(chartModel.priceMin, decimals) + "–" + fmt(chartModel.priceMax, decimals) +
            " · " + direction + " " + fmtSigned(chartModel.changePct, 2) + "%" +
            " · 网格层 " + chartModel.levels.length + degradedText;
        setText($("klineSummary"), summary);
        $("klineCanvas").setAttribute("aria-label", summary + "。可使用左右方向键或两侧按钮逐根查看。");
        renderKlineStats(chartModel, decimals);
        if ((chartTooltipActive || chartPinned) && chartSelection !== null) updateSelectedCandle();
        else setText($("klineDetail"), candleDetail(latest, decimals));
        syncKlineStepper();
        drawKlineChart();
    }

    function renderKlineStats(model, decimals) {
        const changeNode = $("klineChange");
        changeNode.classList.remove("pos", "neg");
        if (!model || !model.latest) {
            setText($("klineLast"), "—");
            setText(changeNode, "—");
            setText($("klineRange"), "—");
            setText($("klineExecution"), "等待行情");
            return;
        }
        setText($("klineLast"), fmt(model.latest.close, decimals));
        setText(changeNode, fmtSigned(model.changePct, 2) + "%");
        changeNode.classList.add(model.changePct >= 0 ? "pos" : "neg");
        setText($("klineRange"), fmt(model.candleLow, decimals) + " – " + fmt(model.candleHigh, decimals));
        setText(
            $("klineExecution"),
            "B " + model.execution.buy + " · S " + model.execution.sell + " · P " + model.execution.position
        );
    }

    function isCoarsePointer(event) {
        if (event && event.pointerType) return event.pointerType === "touch" || event.pointerType === "pen";
        return Boolean(window.matchMedia && window.matchMedia("(pointer: coarse)").matches);
    }

    function prefersReducedMotion() {
        return Boolean(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
    }

    function inspectKlineAt(clientX, options) {
        if (!chartModel || !chartModel.candles.length || !chartGeometry) return;
        const canvas = $("klineCanvas");
        const rect = canvas.getBoundingClientRect();
        const slot = chartGeometry.plotWidth / chartModel.candles.length;
        const index = Math.max(0, Math.min(
            chartModel.candles.length - 1,
            Math.floor((clientX - rect.left - chartGeometry.left) / slot)
        ));
        chartSelection = index;
        chartPinned = Boolean(options && options.pin);
        chartTooltipActive = !isCoarsePointer();
        updateSelectedCandle();
        syncKlineStepper();
        drawKlineChart();
    }

    function stepKline(delta) {
        if (!chartModel || !chartModel.candles.length) return;
        if (chartSelection === null) chartSelection = chartModel.candles.length - 1;
        chartSelection = Math.max(0, Math.min(chartModel.candles.length - 1, chartSelection + delta));
        chartPinned = true;
        chartTooltipActive = !isCoarsePointer();
        updateSelectedCandle();
        syncKlineStepper();
        drawKlineChart();
    }

    function syncKlineStepper() {
        const prev = $("klinePrev");
        const next = $("klineNext");
        if (!prev || !next) return;
        const count = chartModel && chartModel.candles ? chartModel.candles.length : 0;
        const index = chartSelection === null ? count - 1 : chartSelection;
        prev.disabled = count < 2 || index <= 0;
        next.disabled = count < 2 || index >= count - 1;
    }

    function initKlineChart() {
        const canvas = $("klineCanvas");
        const stage = $("klineStage");
        if (!canvas || !stage) return;

        canvas.addEventListener("pointerdown", (event) => {
            chartPointer = {
                id: event.pointerId,
                x: event.clientX,
                y: event.clientY,
                panning: false
            };
            if (!isCoarsePointer(event)) {
                inspectKlineAt(event.clientX, { pin: false });
                canvas.focus({ preventScroll: true });
            }
        });
        canvas.addEventListener("pointermove", (event) => {
            if (!isCoarsePointer(event)) {
                inspectKlineAt(event.clientX, { pin: chartPinned });
                return;
            }
            if (!chartPointer || event.pointerId !== chartPointer.id) return;
            const dx = event.clientX - chartPointer.x;
            const dy = event.clientY - chartPointer.y;
            if (Math.hypot(dx, dy) > 10) chartPointer.panning = true;
        });
        canvas.addEventListener("pointerup", (event) => {
            if (chartPointer && event.pointerId === chartPointer.id && !chartPointer.panning && isCoarsePointer(event)) {
                inspectKlineAt(event.clientX, { pin: true });
                canvas.focus({ preventScroll: true });
            }
            if (chartPointer && event.pointerId === chartPointer.id) chartPointer = null;
        });
        canvas.addEventListener("pointercancel", (event) => {
            if (chartPointer && event.pointerId === chartPointer.id) chartPointer = null;
        });
        canvas.addEventListener("pointerleave", (event) => {
            if (isCoarsePointer(event) || chartPinned) return;
            if (document.activeElement !== canvas) {
                chartTooltipActive = false;
                hideChartTooltip();
                drawKlineChart();
            }
        });
        canvas.addEventListener("focus", () => {
            if (chartModel && chartModel.candles.length) {
                if (chartSelection === null) chartSelection = chartModel.candles.length - 1;
                chartTooltipActive = !isCoarsePointer();
                updateSelectedCandle();
                syncKlineStepper();
                drawKlineChart();
            }
        });
        canvas.addEventListener("blur", () => {
            if (chartPinned) return;
            chartTooltipActive = false;
            hideChartTooltip();
            drawKlineChart();
        });
        canvas.addEventListener("keydown", (event) => {
            if (!chartModel || !chartModel.candles.length || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
            event.preventDefault();
            if (event.key === "ArrowLeft") stepKline(-1);
            if (event.key === "ArrowRight") stepKline(1);
            if (event.key === "Home") {
                chartSelection = 0;
                stepKline(0);
            }
            if (event.key === "End") {
                chartSelection = chartModel.candles.length - 1;
                stepKline(0);
            }
        });

        const prev = $("klinePrev");
        const next = $("klineNext");
        if (prev) prev.addEventListener("click", () => stepKline(-1));
        if (next) next.addEventListener("click", () => stepKline(1));

        document.addEventListener("pointerdown", (event) => {
            if (!chartPinned) return;
            if (event.target.closest("#klineStage, .chart-inspect")) return;
            chartPinned = false;
            chartTooltipActive = false;
            hideChartTooltip();
            drawKlineChart();
        });

        if (typeof ResizeObserver !== "undefined") {
            chartResizeObserver = new ResizeObserver(() => drawKlineChart());
            chartResizeObserver.observe(stage);
        } else {
            window.addEventListener("resize", drawKlineChart);
        }
    }

    function drawKlineChart() {
        if (typeof document === "undefined") return;
        const canvas = $("klineCanvas");
        if (!canvas) return;
        const rect = canvas.getBoundingClientRect();
        const width = Math.max(1, Math.floor(rect.width));
        const height = Math.max(1, Math.floor(rect.height));
        const pixelRatio = Math.min(window.devicePixelRatio || 1, 2);
        const pixelWidth = Math.floor(width * pixelRatio);
        const pixelHeight = Math.floor(height * pixelRatio);
        if (canvas.width !== pixelWidth || canvas.height !== pixelHeight) {
            canvas.width = pixelWidth;
            canvas.height = pixelHeight;
        }
        const ctx = canvas.getContext("2d");
        if (!ctx) return;
        ctx.setTransform(pixelRatio, 0, 0, pixelRatio, 0, 0);
        ctx.clearRect(0, 0, width, height);
        if (!chartModel || !chartModel.candles.length || chartModel.priceMax <= chartModel.priceMin) {
            hideChartTooltip();
            return;
        }

        const style = getComputedStyle(document.documentElement);
        const color = (name, fallback) => style.getPropertyValue(name).trim() || fallback;
        const colors = {
            ink: color("--ink", "#eef4f1"),
            muted: color("--muted", "#9aaba4"),
            mutedSoft: color("--muted-soft", "#7e9089"),
            line: color("--line", "rgba(154,186,176,.14)"),
            lineStrong: color("--line-strong", "rgba(154,186,176,.28)"),
            up: color("--positive", "#3ee0a4"),
            down: color("--negative", "#ff7a73"),
            buy: color("--buy", "#ff7a73"),
            sell: color("--sell", "#3ee0a4"),
            position: color("--cyan", "#5ec6ff"),
            grid: color("--warn", "#f0c15a"),
            surface: color("--surface", "#0d1214"),
            rail: color("--plot-rail", "rgba(8,12,14,.55)"),
            labelBg: color("--plot-label", "rgba(8,12,14,.9)")
        };
        const compact = width < 520;
        const narrow = width < 400;
        const left = narrow ? 6 : (compact ? 9 : 14);
        const axisWidth = narrow ? 46 : (compact ? 58 : 72);
        const railWidth = narrow ? 62 : (compact ? 78 : 98);
        const right = axisWidth + railWidth;
        const top = 22;
        const bottom = 30;
        const plotWidth = Math.max(20, width - left - right);
        const plotHeight = Math.max(20, height - top - bottom);
        const plotRight = left + plotWidth;
        const railLeft = plotRight + 6;
        const railRight = width - axisWidth - 4;
        const yForPrice = (price) => top + (chartModel.priceMax - price) /
            (chartModel.priceMax - chartModel.priceMin) * plotHeight;
        chartGeometry = { left, right, top, bottom, plotWidth, plotHeight, plotRight, railLeft, railRight, yForPrice };

        ctx.font = (compact ? "9px " : "10px ") + style.getPropertyValue("--font-mono");
        ctx.textBaseline = "middle";
        ctx.lineWidth = 1;
        const yTicks = compact ? 4 : 5;
        for (let tick = 0; tick <= yTicks; tick++) {
            const ratio = tick / yTicks;
            const y = top + ratio * plotHeight;
            const price = chartModel.priceMax - ratio * (chartModel.priceMax - chartModel.priceMin);
            ctx.strokeStyle = colors.line;
            ctx.setLineDash([]);
            ctx.beginPath();
            ctx.moveTo(left, Math.round(y) + 0.5);
            ctx.lineTo(plotRight, Math.round(y) + 0.5);
            ctx.stroke();
            ctx.fillStyle = colors.muted;
            ctx.textAlign = "left";
            ctx.fillText(compactPrice(price), railRight + 7, y);
        }
        ctx.fillStyle = colors.rail;
        ctx.fillRect(plotRight + 1, top, Math.max(0, railRight - plotRight), plotHeight);
        ctx.strokeStyle = colors.line;
        ctx.beginPath();
        ctx.moveTo(Math.round(plotRight) + 0.5, top);
        ctx.lineTo(Math.round(plotRight) + 0.5, top + plotHeight);
        ctx.stroke();
        ctx.fillStyle = colors.mutedSoft;
        ctx.textAlign = "right";
        ctx.fillText("PRICE", width - 7, 7);
        ctx.textAlign = "left";
        ctx.fillText("TIME · " + chartModel.interval, left, 7);
        ctx.fillText(compact ? "ORD" : "EXECUTION", railLeft, 7);

        drawExecutionBands(ctx, chartModel.levels, chartGeometry, colors);
        drawGridLevels(ctx, chartModel.visibleLevels || chartModel.levels, chartGeometry, colors, {
            above: chartModel.overflowAbove || [],
            below: chartModel.overflowBelow || [],
            labelLevels: chartModel.railLabels
        });
        drawOverflowChips(ctx, chartModel.overflowAbove || [], chartModel.overflowBelow || [], chartGeometry, colors, compact);

        const candles = chartModel.candles;
        const slotWidth = plotWidth / candles.length;
        const bodyWidth = Math.max(2.5, Math.min(14, slotWidth * 0.76));
        const timeTickCount = compact ? 3 : 5;
        const timeIndices = Array.from(new Set(Array.from({ length: timeTickCount }, (_, index) =>
            Math.round(index * (candles.length - 1) / Math.max(1, timeTickCount - 1))
        )));
        ctx.strokeStyle = colors.line;
        ctx.globalAlpha = 0.62;
        ctx.setLineDash([2, 5]);
        timeIndices.forEach((index) => {
            const x = left + slotWidth * (index + 0.5);
            ctx.beginPath();
            ctx.moveTo(Math.round(x) + 0.5, top);
            ctx.lineTo(Math.round(x) + 0.5, top + plotHeight);
            ctx.stroke();
        });
        ctx.globalAlpha = 1;
        ctx.save();
        ctx.beginPath();
        ctx.rect(left, top, plotWidth, plotHeight);
        ctx.clip();
        candles.forEach((candle, index) => {
            const x = left + slotWidth * (index + 0.5);
            const openY = yForPrice(candle.open);
            const closeY = yForPrice(candle.close);
            const highY = yForPrice(candle.high);
            const lowY = yForPrice(candle.low);
            const bullish = candle.close >= candle.open;
            const candleColor = bullish ? colors.up : colors.down;
            const live = !candle.isClosed;
            ctx.setLineDash([]);
            ctx.strokeStyle = candleColor;
            ctx.lineWidth = live ? 1.6 : 1.15;
            ctx.beginPath();
            ctx.moveTo(x, highY);
            ctx.lineTo(x, lowY);
            ctx.stroke();
            const bodyTop = Math.min(openY, closeY);
            const bodyHeight = Math.max(2, Math.abs(openY - closeY));
            const bodyX = x - bodyWidth / 2;
            if (bullish) {
                ctx.fillStyle = candleColor;
                ctx.fillRect(bodyX, bodyTop, bodyWidth, bodyHeight);
            } else {
                ctx.fillStyle = colors.surface;
                ctx.fillRect(bodyX, bodyTop, bodyWidth, bodyHeight);
                ctx.strokeRect(bodyX, bodyTop, bodyWidth, bodyHeight);
            }
            if (live) {
                ctx.strokeStyle = colors.position;
                ctx.lineWidth = 1.2;
                ctx.strokeRect(bodyX - 1, bodyTop - 1, bodyWidth + 2, bodyHeight + 2);
            }
        });
        ctx.restore();

        ctx.fillStyle = colors.muted;
        timeIndices.forEach((index, order) => {
            const x = left + slotWidth * (index + 0.5);
            ctx.textAlign = order === 0 ? "left" : (order === timeIndices.length - 1 ? "right" : "center");
            ctx.fillText(formatChartTime(candles[index].time, candles), x, top + plotHeight + 18);
        });

        const latestY = yForPrice(chartModel.latest.close);
        ctx.strokeStyle = colors.position;
        ctx.setLineDash([3, 4]);
        ctx.beginPath();
        ctx.moveTo(left, latestY);
        ctx.lineTo(plotRight, latestY);
        ctx.stroke();
        drawAxisPriceTag(ctx, compactPrice(chartModel.latest.close), latestY, railRight + 4, colors.position, colors.surface, width - railRight - 5);

        if (chartSelection !== null && candles[chartSelection] && (chartTooltipActive || chartPinned)) {
            const selected = candles[chartSelection];
            const x = left + slotWidth * (chartSelection + 0.5);
            const y = yForPrice(selected.close);
            ctx.strokeStyle = colors.ink;
            ctx.globalAlpha = 0.55;
            ctx.setLineDash([2, 4]);
            ctx.beginPath();
            ctx.moveTo(x, top);
            ctx.lineTo(x, top + plotHeight);
            ctx.moveTo(left, y);
            ctx.lineTo(plotRight, y);
            ctx.stroke();
            ctx.globalAlpha = 1;
            if (chartTooltipActive && !isCoarsePointer()) positionChartTooltip(x, y, width, height, selected);
            else hideChartTooltip();
        } else {
            hideChartTooltip();
        }
    }

    function drawExecutionBands(ctx, levels, geometry, colors) {
        const windowLevels = levels.filter((level) => level.inWindow || level.isGrid);
        if (!windowLevels.length) return;
        const gridLevel = levels.find((level) => level.isGrid);
        const highest = Math.max(...windowLevels.map((level) => level.price));
        const lowest = Math.min(...windowLevels.map((level) => level.price));
        const topY = Math.max(geometry.top, geometry.yForPrice(highest));
        const bottomY = Math.min(geometry.top + geometry.plotHeight, geometry.yForPrice(lowest));
        if (bottomY <= topY) return;

        ctx.save();
        if (gridLevel) {
            const gridY = Math.max(topY, Math.min(bottomY, geometry.yForPrice(gridLevel.price)));
            ctx.fillStyle = colors.sell;
            ctx.globalAlpha = 0.035;
            ctx.fillRect(geometry.left, topY, geometry.plotWidth, Math.max(0, gridY - topY));
            ctx.fillStyle = colors.buy;
            ctx.fillRect(geometry.left, gridY, geometry.plotWidth, Math.max(0, bottomY - gridY));
        } else {
            ctx.fillStyle = colors.position;
            ctx.globalAlpha = 0.025;
            ctx.fillRect(geometry.left, topY, geometry.plotWidth, bottomY - topY);
        }
        ctx.restore();
    }

    function drawGridLevels(ctx, levels, geometry, colors, overflow) {
        const activeLevels = [];
        const railPadTop = overflow && overflow.above && overflow.above.length ? 22 : 9;
        const railPadBottom = overflow && overflow.below && overflow.below.length ? 22 : 9;
        levels.forEach((level) => {
            const y = geometry.yForPrice(level.price);
            if (y < geometry.top - 1 || y > geometry.top + geometry.plotHeight + 1) return;
            const settings = {
                grid: [colors.grid, [], 1.5, 0.92],
                sell: [colors.sell, [], 1, 0.68],
                position: [colors.position, [2, 4], 1, 0.72],
                buy: [colors.buy, [7, 5], 1, 0.68],
                empty: [colors.mutedSoft, [2, 6], 1, 0.32],
                outside: [colors.mutedSoft, [1, 7], 1, 0.18]
            }[level.kind];
            ctx.save();
            ctx.strokeStyle = settings[0];
            ctx.setLineDash(settings[1]);
            ctx.lineWidth = settings[2];
            ctx.globalAlpha = settings[3];
            ctx.beginPath();
            ctx.moveTo(geometry.left, Math.round(y) + 0.5);
            ctx.lineTo(geometry.left + geometry.plotWidth, Math.round(y) + 0.5);
            ctx.stroke();
            ctx.restore();
            const labelLevels = overflow && overflow.labelLevels;
            const labeled = !labelLevels || labelLevels.indexOf(level) >= 0;
            if (level.markers.length && labeled) activeLevels.push({ level, y, color: settings[0] });
        });

        const labelItems = activeLevels.sort((a, b) => a.y - b.y).map((item) => ({
            ...item,
            labelY: item.y
        }));
        const railTop = geometry.top + railPadTop;
        const railBottom = geometry.top + geometry.plotHeight - railPadBottom;
        labelItems.forEach((item, index) => {
            item.labelY = Math.max(
                railTop,
                index ? Math.max(item.y, labelItems[index - 1].labelY + 19) : item.y
            );
        });
        const packedOverflow = labelItems.length
            ? labelItems[labelItems.length - 1].labelY - railBottom
            : 0;
        if (packedOverflow > 0) {
            for (let index = labelItems.length - 1; index >= 0; index--) {
                const nextY = index === labelItems.length - 1
                    ? railBottom
                    : labelItems[index + 1].labelY - 19;
                labelItems[index].labelY = Math.min(labelItems[index].labelY - packedOverflow, nextY);
            }
        }
        labelItems.forEach((item) => {
            const label = item.level.markers.join("·") + " " + compactPrice(item.level.price);
            const labelWidth = Math.max(42, geometry.railRight - geometry.railLeft - 2);
            const x = geometry.railLeft;
            ctx.save();
            ctx.strokeStyle = item.color;
            ctx.globalAlpha = 0.52;
            ctx.beginPath();
            ctx.moveTo(geometry.plotRight, item.y);
            ctx.lineTo(x - 2, item.labelY);
            ctx.stroke();
            ctx.restore();
            ctx.fillStyle = colors.labelBg;
            ctx.strokeStyle = item.color;
            ctx.setLineDash([]);
            ctx.lineWidth = 1;
            ctx.beginPath();
            ctx.roundRect(x, item.labelY - 8, labelWidth, 16, 3);
            ctx.fill();
            ctx.stroke();
            ctx.fillStyle = item.color;
            ctx.textAlign = "center";
            ctx.fillText(label, x + labelWidth / 2, item.labelY + 0.5, labelWidth - 6);
        });
    }

    function overflowChipText(levels, atTop, compact) {
        const counts = { B: 0, S: 0, P: 0, G: 0 };
        levels.forEach((level) => {
            (level.markers || []).forEach((marker) => {
                if (counts[marker] != null) counts[marker] += 1;
            });
        });
        const parts = [];
        if (compact) {
            if (counts.S) parts.push("S" + counts.S);
            else if (counts.P) parts.push("P" + counts.P);
            if (counts.B) parts.push("B" + counts.B);
        } else if (counts.S && counts.S === counts.P && !counts.B) {
            parts.push("S·P" + counts.S);
        } else {
            if (counts.S) parts.push("S" + counts.S);
            if (counts.P && counts.P !== counts.S) parts.push("P" + counts.P);
            if (counts.B) parts.push("B" + counts.B);
        }
        if (counts.G) parts.push("G");
        return (atTop ? "上 " : "下 ") + (parts.join("·") || String(levels.length));
    }

    function overflowChipColor(levels, colors) {
        if (levels.some((level) => level.hasSell)) return colors.sell;
        if (levels.some((level) => level.hasBuy)) return colors.buy;
        if (levels.some((level) => level.hasPosition)) return colors.position;
        if (levels.some((level) => level.isGrid)) return colors.grid;
        return colors.muted;
    }

    function drawOverflowChips(ctx, above, below, geometry, colors, compact) {
        const chips = [];
        if (above.length) chips.push({
            text: overflowChipText(above, true, compact),
            color: overflowChipColor(above, colors),
            y: geometry.top + 9
        });
        if (below.length) chips.push({
            text: overflowChipText(below, false, compact),
            color: overflowChipColor(below, colors),
            y: geometry.top + geometry.plotHeight - 9
        });
        const labelWidth = Math.max(42, geometry.railRight - geometry.railLeft - 2);
        const x = geometry.railLeft;
        chips.forEach((chip) => {
            ctx.save();
            ctx.fillStyle = colors.labelBg;
            ctx.strokeStyle = chip.color;
            ctx.setLineDash([]);
            ctx.lineWidth = 1;
            ctx.beginPath();
            ctx.roundRect(x, chip.y - 8, labelWidth, 16, 3);
            ctx.fill();
            ctx.stroke();
            ctx.fillStyle = chip.color;
            ctx.textAlign = "center";
            ctx.textBaseline = "middle";
            ctx.fillText(chip.text, x + labelWidth / 2, chip.y + 0.5, labelWidth - 6);
            ctx.beginPath();
            ctx.strokeStyle = chip.color;
            ctx.globalAlpha = 0.45;
            ctx.setLineDash([2, 3]);
            ctx.moveTo(geometry.plotRight, chip.y);
            ctx.lineTo(x - 2, chip.y);
            ctx.stroke();
            ctx.restore();
        });
    }

    function drawAxisPriceTag(ctx, text, y, x, foreground, background, maxWidth) {
        const width = Math.max(38, Math.min(maxWidth, ctx.measureText(text).width + 10));
        ctx.fillStyle = foreground;
        ctx.fillRect(x, y - 9, width, 18);
        ctx.fillStyle = background;
        ctx.textAlign = "center";
        ctx.fillText(text, x + width / 2, y + 0.5, width - 5);
    }

    function updateSelectedCandle() {
        if (!chartModel || chartSelection === null) return;
        const candle = chartModel.candles[chartSelection];
        if (!candle) return;
        const decimals = latestSnapshot && latestSnapshot.position
            ? (latestSnapshot.position.priceDecimals ?? 2)
            : 2;
        setText($("klineDetail"), candleDetail(candle, decimals));
    }

    function candleDetail(candle, decimals) {
        if (!candle) return "暂无 K 线明细";
        return formatCompactDateTime(candle.time) +
            " · 开 " + fmt(candle.open, decimals) +
            " · 高 " + fmt(candle.high, decimals) +
            " · 低 " + fmt(candle.low, decimals) +
            " · 收 " + fmt(candle.close, decimals) +
            " · " + (candle.isClosed ? "已完结" : "进行中");
    }

    function positionChartTooltip(x, y, width, height, candle) {
        const tooltip = $("klineTooltip");
        if (!tooltip) return;
        tooltip.hidden = false;
        tooltip.setAttribute("aria-hidden", "false");
        const decimals = latestSnapshot && latestSnapshot.position
            ? (latestSnapshot.position.priceDecimals ?? 2)
            : 2;
        tooltip.textContent = formatCompactDateTime(candle.time) + "\n" +
            "O " + fmt(candle.open, decimals) + "  H " + fmt(candle.high, decimals) + "\n" +
            "L " + fmt(candle.low, decimals) + "  C " + fmt(candle.close, decimals);
        const tooltipWidth = tooltip.offsetWidth || 178;
        const tooltipHeight = tooltip.offsetHeight || 58;
        const preferLeft = x > width * 0.58;
        const left = preferLeft
            ? Math.max(8, x - tooltipWidth - 12)
            : Math.max(8, Math.min(width - tooltipWidth - 8, x + 13));
        const top = Math.max(8, Math.min(height - tooltipHeight - 8, y - tooltipHeight / 2));
        tooltip.style.transform = "translate(" + left + "px," + top + "px)";
    }

    function hideChartTooltip() {
        const tooltip = $("klineTooltip");
        if (!tooltip) return;
        tooltip.hidden = true;
        tooltip.setAttribute("aria-hidden", "true");
    }

    function compactPrice(value) {
        const number = Number(value);
        if (!Number.isFinite(number)) return "—";
        if (Math.abs(number) >= 1000) return number.toLocaleString(undefined, { maximumFractionDigits: 2 });
        if (Math.abs(number) >= 1) return number.toLocaleString(undefined, { maximumFractionDigits: 4 });
        return number.toLocaleString(undefined, { maximumSignificantDigits: 5 });
    }

    function formatChartTime(value, candles) {
        const date = new Date(value);
        if (Number.isNaN(date.getTime())) return "—";
        const span = candles.length > 1 ? candles[candles.length - 1].time - candles[0].time : 0;
        return date.toLocaleString("zh-CN", span >= 24 * 60 * 60 * 1000 ? {
            month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false
        } : {
            hour: "2-digit", minute: "2-digit", hour12: false
        });
    }

    function renderRisk(risk) {
        $("riskMsg").textContent = risk.lastMsg || (risk.enabled ? "等待风控读数" : "主动风控未启用");
        const list = $("riskList");
        if (!risk.enabled) {
            replaceChildren(list, [element("div", "empty", "风控关闭")]);
            return;
        }

        const symbols = risk.symbols || [];
        if (!symbols.length) {
            replaceChildren(list, [element("div", "empty", "暂无监控币种数据")]);
            return;
        }

        const items = symbols.map((item) => {
            const bad = Boolean(item.abnormal || (risk.triggered && item.priceBelowMA));
            const warn = !bad && Boolean(item.status) && item.status !== "正常";
            const card = element("div", "risk-item " + (bad ? "bad" : (warn ? "warn" : "ok")));
            const top = element("div", "top");
            top.append(
                element("strong", "", item.symbol || "—"),
                element("span", "status", item.status || (bad ? "异常" : "正常"))
            );
            const meta = element(
                "div",
                "meta",
                "价 " + fmt(item.currentPrice, 4) +
                " / 均 " + fmt(item.avgPrice, 4) +
                " (" + fmt(item.priceDeviation, 2) + "%) · 量比 ×" + fmt(item.volumeRatio, 2)
            );
            card.append(top, meta);
            return card;
        });
        replaceChildren(list, items);
    }

    function renderLogs(logs) {
        const box = $("logs");
        if (!logs.length) {
            replaceChildren(box, [element("div", "empty", "暂无日志")]);
            return;
        }

        const allowedLevels = new Set(["DEBUG", "INFO", "WARN", "ERROR", "FATAL"]);
        const items = logs.slice().reverse().map((item) => {
            const level = allowedLevels.has(item.level) ? item.level : "";
            const time = item.time ? formatTime(item.time) : "";
            const row = element("div", "log" + (level ? " " + level : ""));
            row.append(
                element("time", "log-time", time || "—"),
                element("span", "log-level", level || "LOG"),
                element("span", "log-msg", item.message || "")
            );
            return row;
        });
        replaceChildren(box, items);
    }

    function pad2(value) {
        return String(value).padStart(2, "0");
    }

    function parseFilledAt(value) {
        if (!value) return null;
        const date = value instanceof Date ? value : new Date(value);
        return Number.isNaN(date.getTime()) ? null : date;
    }

    function startOfHour(date) {
        return new Date(date.getFullYear(), date.getMonth(), date.getDate(), date.getHours(), 0, 0, 0);
    }

    function buildHourlyFillModel(orders, now, options) {
        const current = now instanceof Date ? now : new Date(now || Date.now());
        const maxHours = Math.max(1, Number(options && options.maxHours) || 24);
        const snapshotBuckets = options && options.buckets;
        const parsed = [];
        (orders || []).forEach((order) => {
            const filledAt = parseFilledAt(order && order.filledAt);
            const side = order && order.side === "BUY" ? "BUY" : (order && order.side === "SELL" ? "SELL" : "");
            if (!filledAt || !side) return;
            parsed.push({
                filledAt,
                side,
                quantity: Number(order.quantity) || 0,
                realizedPnl: Number(order.realizedPnl) || 0
            });
        });

        const stats = {
            total: parsed.length,
            windowTotal: 0,
            buyCount: 0,
            sellCount: 0,
            buyQty: 0,
            sellQty: 0,
            realizedPnl: 0,
            peakHour: "",
            peakCount: 0,
            hours: 0,
            activeHours: 0,
            avgPerHour: 0,
            windowLabel: ""
        };

        const end = startOfHour(current);
        const start = new Date(end.getTime() - (maxHours - 1) * 3600000);
        const buckets = [];
        const index = new Map();
        for (let time = start.getTime(); time <= end.getTime(); time += 3600000) {
            const hour = new Date(time);
            const bucket = {
                key: hour.getTime(),
                hour,
                label: pad2(hour.getHours()) + ":00",
                dayLabel: pad2(hour.getMonth() + 1) + "/" + pad2(hour.getDate()),
                buy: 0,
                sell: 0,
                buyQty: 0,
                sellQty: 0,
                pnl: 0
            };
            index.set(bucket.key, bucket);
            buckets.push(bucket);
        }

        if (Array.isArray(snapshotBuckets) && snapshotBuckets.length) {
            snapshotBuckets.forEach((item) => {
                const hour = parseFilledAt(item && item.hour);
                const bucket = hour ? index.get(startOfHour(hour).getTime()) : null;
                if (!bucket) return;
                bucket.buy += Number(item.buy) || 0;
                bucket.sell += Number(item.sell) || 0;
                bucket.buyQty += Number(item.buyQty) || 0;
                bucket.sellQty += Number(item.sellQty) || 0;
                bucket.pnl += Number(item.pnl) || 0;
            });
        } else {
            parsed.forEach((item) => {
                const bucket = index.get(startOfHour(item.filledAt).getTime());
                if (!bucket) return;
                if (item.side === "BUY") {
                    bucket.buy += 1;
                    bucket.buyQty += item.quantity;
                } else {
                    bucket.sell += 1;
                    bucket.sellQty += item.quantity;
                    bucket.pnl += item.realizedPnl;
                }
            });
        }

        let maxCount = 0;
        let peakKey = -1;
        const spansDays = buckets.length > 1 &&
            buckets[0].dayLabel !== buckets[buckets.length - 1].dayLabel;
        buckets.forEach((bucket) => {
            const count = bucket.buy + bucket.sell;
            stats.windowTotal += count;
            stats.buyCount += bucket.buy;
            stats.sellCount += bucket.sell;
            stats.buyQty += bucket.buyQty;
            stats.sellQty += bucket.sellQty;
            stats.realizedPnl += bucket.pnl;
            if (count > 0) stats.activeHours += 1;
            if (bucket.buy > maxCount) maxCount = bucket.buy;
            if (bucket.sell > maxCount) maxCount = bucket.sell;
            if (count > stats.peakCount || (count === stats.peakCount && count > 0 && bucket.key > peakKey)) {
                stats.peakCount = count;
                stats.peakHour = bucket.label;
                peakKey = bucket.key;
            }
            if (spansDays && bucket.hour.getHours() === 0) {
                bucket.label = bucket.dayLabel + " " + bucket.label;
            }
        });
        stats.hours = buckets.length;
        stats.avgPerHour = stats.hours ? stats.windowTotal / stats.hours : 0;
        stats.windowLabel = buckets.length
            ? buckets[0].label + "–" + buckets[buckets.length - 1].label
            : "";
        return { buckets, stats, maxCount };
    }

    function renderTables(pos, quote) {
        const filledOrders = pos.filledOrders || [];
        const recentOrders = filledOrders.slice(0, 20);

        const filledOrderCount = pos.filledOrderCount ?? filledOrders.length;
        setText(
            $("filledCount"),
            filledOrderCount
                ? "本次运行 · " + filledOrderCount + " 笔 · 列表最近 " + recentOrders.length
                : "本次运行 · 0 笔"
        );
        renderHourlyFills(filledOrders, pos, quote);
        const filledRows = recentOrders.map((order) => {
            const row = document.createElement("tr");
            const side = order.side === "BUY" ? "BUY" : (order.side === "SELL" ? "SELL" : "—");
            const pnl = Number(order.realizedPnl || 0);
            appendCell(row, "成交时间", formatCompactDateTime(order.filledAt), "fill-time");
            appendCell(row, "方向", side, "trade-side " + side.toLowerCase());
            appendCell(row, "成交均价", fmt(order.price, pos.priceDecimals));
            appendCell(row, "成交数量", fmt(order.quantity, pos.quantityDecimals || 4));
            appendCell(
                row,
                "已实现盈亏",
                side === "SELL" ? fmtSigned(pnl, 6) + " " + quote : "—",
                "fill-pnl" + (side === "SELL" ? (pnl >= 0 ? " pos" : " neg") : "")
            );
            row.appendChild(orderIDCell(String(order.orderId || order.clientOrderId || "—")));
            return row;
        });
        replaceChildren(
            $("filledBody"),
            filledRows.length ? filledRows : [emptyRow("程序启动后暂无成交订单", 6)]
        );
    }

    function renderHourlyFills(orders, pos, quote) {
        const qtyDecimals = pos.quantityDecimals || 4;
        const model = buildHourlyFillModel(orders, Date.now(), { buckets: pos.filledHourly, maxHours: 24 });
        const stats = model.stats;
        const buyNode = $("fillBuyCount");
        const sellNode = $("fillSellCount");
        const pnlNode = $("fillPnl");
        setText(buyNode, stats.buyCount + " 笔");
        buyNode.classList.toggle("buy", stats.buyCount > 0);
        setText(sellNode, stats.sellCount + " 笔");
        sellNode.classList.toggle("sell", stats.sellCount > 0);
        setText($("fillQty"), fmt(stats.buyQty, qtyDecimals) + " / " + fmt(stats.sellQty, qtyDecimals));
        setText($("fillPeak"), stats.peakCount
            ? stats.peakHour + " · " + stats.peakCount + " 笔"
            : "—");
        setText(pnlNode, stats.windowTotal ? fmtSigned(stats.realizedPnl, 6) + " " + quote : "—");
        pnlNode.classList.remove("pos", "neg");
        if (stats.windowTotal && stats.realizedPnl > 0) pnlNode.classList.add("pos");
        if (stats.windowTotal && stats.realizedPnl < 0) pnlNode.classList.add("neg");
        setText($("fillAvg"), stats.hours
            ? fmt(stats.avgPerHour, 1) + " 笔/时"
            : "—");
        setText(
            $("fillWindow"),
            stats.hours
                ? "近 24 小时 · " + stats.windowLabel + " · " + stats.activeHours + "/" + stats.hours + " 小时有成交"
                : "近 24 小时"
        );
        setText($("fillAxisMax"), String(model.maxCount || 0));
        setText($("fillAxisMid"), String(Math.ceil((model.maxCount || 0) / 2)));

        const chart = $("fillHourlyChart");
        const empty = $("fillHourlyEmpty");
        const hasData = model.buckets.some((bucket) => bucket.buy + bucket.sell > 0);
        empty.hidden = hasData;
        chart.hidden = !hasData;
        if (!hasData) {
            selectedHourKey = null;
            setText($("fillHourlySummary"), "程序启动后暂无成交订单");
            setText($("fillHourDetail"), "近 24 小时暂无成交");
            replaceChildren(chart, []);
            return;
        }

        const lastKey = String(model.buckets[model.buckets.length - 1].key);
        const columns = model.buckets.map((bucket) => {
            const total = bucket.buy + bucket.sell;
            const col = element("button", "hour-col" + (total ? "" : " is-empty"));
            col.type = "button";
            col.dataset.hour = String(bucket.key);
            col.dataset.detail = hourDetailText(bucket, quote, qtyDecimals);
            if (String(bucket.key) === lastKey) col.classList.add("is-now");
            col.setAttribute(
                "aria-label",
                bucket.label + " 买单 " + bucket.buy + " 笔，卖单 " + bucket.sell + " 笔"
            );
            const bars = element("div", "hour-bars");
            bars.style.setProperty("--max", String(Math.max(model.maxCount, 1)));
            bars.append(hourBar("buy", bucket.buy, "买"), hourBar("sell", bucket.sell, "卖"));
            col.append(bars, element("div", "hour-label", pad2(bucket.hour.getHours())));
            return col;
        });
        replaceChildren(chart, columns);
        applyHourSelection(chart, selectedHourKey);
        setText(
            $("fillHourlySummary"),
            "近 " + stats.hours + " 小时成交 " + stats.windowTotal + " 笔，买单 " +
                stats.buyCount + "，卖单 " + stats.sellCount +
                (stats.peakCount ? "，峰值在 " + stats.peakHour + " 共 " + stats.peakCount + " 笔" : "")
        );
    }

    function hourDetailText(bucket, quote, qtyDecimals) {
        const total = bucket.buy + bucket.sell;
        if (!total) return bucket.label + "  无成交";
        return bucket.label +
            "  买 " + bucket.buy + " 笔 / 卖 " + bucket.sell + " 笔" +
            "  · 量 " + fmt(bucket.buyQty, qtyDecimals) + " / " + fmt(bucket.sellQty, qtyDecimals) +
            "  · 盈亏 " + fmtSigned(bucket.pnl, 6) + " " + quote;
    }

    function applyHourSelection(chart, key) {
        const columns = Array.from(chart.children);
        if (!columns.length) return;
        let selected = columns.find((col) => col.dataset.hour === key);
        if (!selected) {
            selected = columns.find((col) => col.classList.contains("is-now") && !col.classList.contains("is-empty"))
                || columns.filter((col) => !col.classList.contains("is-empty")).pop()
                || columns[columns.length - 1];
        }
        columns.forEach((col) => {
            const on = col === selected;
            col.classList.toggle("is-selected", on);
            col.setAttribute("aria-pressed", on ? "true" : "false");
        });
        selectedHourKey = selected.dataset.hour;
        setText($("fillHourDetail"), selected.dataset.detail || "");
    }

    function initHourlyChart() {
        const chart = $("fillHourlyChart");
        if (!chart) return;
        chart.addEventListener("click", (event) => {
            const col = event.target.closest(".hour-col");
            if (!col || !chart.contains(col)) return;
            applyHourSelection(chart, col.dataset.hour);
        });
        chart.addEventListener("keydown", (event) => {
            if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
            const columns = Array.from(chart.querySelectorAll(".hour-col"));
            if (!columns.length) return;
            const current = columns.findIndex((col) => col.dataset.hour === selectedHourKey);
            let next = current < 0 ? columns.length - 1 : current;
            if (event.key === "ArrowLeft") next = Math.max(0, next - 1);
            if (event.key === "ArrowRight") next = Math.min(columns.length - 1, next + 1);
            if (event.key === "Home") next = 0;
            if (event.key === "End") next = columns.length - 1;
            event.preventDefault();
            applyHourSelection(chart, columns[next].dataset.hour);
            columns[next].focus();
        });
    }

    function hourBar(side, count, label) {
        const wrap = element("div", "hour-bar-wrap");
        const value = element("span", "hour-n", count ? String(count) : "");
        value.setAttribute("aria-hidden", "true");
        const track = element("div", "hour-track");
        const bar = element("div", "hour-bar " + side + (count ? " is-on" : ""));
        bar.style.setProperty("--n", String(count));
        bar.setAttribute("title", label + " " + count);
        track.appendChild(bar);
        wrap.append(value, track);
        return wrap;
    }

    function appendCell(row, label, value, className) {
        const cell = element("td", className || "", value);
        cell.dataset.label = label;
        row.appendChild(cell);
    }

    function orderIDCell(id) {
        const cell = element("td", "order-id");
        cell.dataset.label = "ID";
        const code = element("code", "order-code");
        code.title = id;
        code.setAttribute("aria-label", id);
        code.append(
            element("span", "order-id-full", id),
            element("span", "order-id-short", shortenId(id))
        );
        const button = element("button", "copy-id", "复制");
        button.type = "button";
        button.dataset.copy = id;
        button.setAttribute("aria-label", "复制订单 ID " + id);
        cell.append(code, button);
        return cell;
    }

    function showToast(message) {
        const node = $("deskToast");
        if (!node) return;
        node.textContent = message;
        node.classList.add("is-on");
        clearTimeout(toastTimer);
        toastTimer = setTimeout(() => node.classList.remove("is-on"), 2200);
    }

    async function copyText(value) {
        try {
            if (navigator.clipboard && navigator.clipboard.writeText) {
                await navigator.clipboard.writeText(value);
                return true;
            }
        } catch (_error) {
            // Fall through to the execCommand path.
        }
        try {
            const input = document.createElement("textarea");
            input.value = value;
            input.setAttribute("readonly", "");
            input.style.position = "fixed";
            input.style.left = "-9999px";
            document.body.appendChild(input);
            input.select();
            const ok = document.execCommand("copy");
            input.remove();
            return ok;
        } catch (_error) {
            return false;
        }
    }

    function initCopyOrders() {
        const body = $("filledBody");
        if (!body) return;
        body.addEventListener("click", async (event) => {
            const button = event.target.closest(".copy-id");
            if (!button) return;
            const value = button.dataset.copy || "";
            const ok = Boolean(value) && await copyText(value);
            showToast(ok ? "已复制订单 ID" : "复制失败");
        });
    }

    function setActiveNav(hash) {
        const nav = $("deskNav");
        if (!nav) return;
        nav.querySelectorAll("a[href^='#']").forEach((link) => {
            const on = link.hash === hash;
            link.classList.toggle("is-active", on);
            if (on) link.setAttribute("aria-current", "page");
            else link.removeAttribute("aria-current");
        });
    }

    function initDeskNav() {
        const nav = $("deskNav");
        if (!nav) return;
        const links = Array.from(nav.querySelectorAll("a[href^='#']"));
        let navLockUntil = 0;
        let scrollTimer = 0;

        const updateFromScroll = () => {
            if (Date.now() < navLockUntil) return;
            const line = Math.max(64, (document.querySelector(".mast")?.getBoundingClientRect().bottom || 64) + 8);
            let current = links[0];
            links.forEach((link) => {
                const section = document.querySelector(link.hash);
                if (!section) return;
                if (section.getBoundingClientRect().top - line <= 12) current = link;
            });
            if (current) setActiveNav(current.hash);
        };

        nav.addEventListener("click", (event) => {
            const link = event.target.closest("a[href^='#']");
            if (!link || !nav.contains(link)) return;
            const target = document.querySelector(link.hash);
            if (!target) return;
            event.preventDefault();
            navLockUntil = Date.now() + 1200;
            setActiveNav(link.hash);
            target.scrollIntoView({
                behavior: prefersReducedMotion() ? "auto" : "smooth",
                block: "start"
            });
            history.replaceState(null, "", link.hash);
        });
        if (location.hash) setActiveNav(location.hash);
        window.addEventListener("scroll", () => {
            clearTimeout(scrollTimer);
            scrollTimer = setTimeout(updateFromScroll, 80);
        }, { passive: true });
        updateFromScroll();
    }

    function emptyRow(message, colSpan) {
        const row = document.createElement("tr");
        const cell = element("td", "muted", message);
        cell.colSpan = colSpan || 4;
        row.appendChild(cell);
        return row;
    }

    function shortenId(value) {
        if (value.length <= 22) return value;
        return value.slice(0, 11) + "…" + value.slice(-7);
    }

    function formatTime(value) {
        if (!value) return "—";
        const date = new Date(value);
        return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString();
    }

    function formatDateTime(value) {
        if (!value) return "—";
        const date = new Date(value);
        return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString();
    }

    function formatCompactDateTime(value) {
        if (!value) return "—";
        const date = new Date(value);
        if (Number.isNaN(date.getTime())) return "—";
        return date.toLocaleString("zh-CN", {
            month: "2-digit",
            day: "2-digit",
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
            hour12: false
        });
    }

    function formatDuration(value) {
        const seconds = Number(value);
        if (!Number.isFinite(seconds) || seconds < 0) return "—";
        const total = Math.floor(seconds);
        const days = Math.floor(total / 86400);
        const hours = Math.floor((total % 86400) / 3600);
        const minutes = Math.floor((total % 3600) / 60);
        const secs = total % 60;
        const clock = [hours, minutes, secs].map((part) => String(part).padStart(2, "0")).join(":");
        return days > 0 ? days + "天 " + clock : clock;
    }

    function fmtSigned(value, digits) {
        const number = Number(value);
        if (!Number.isFinite(number)) return "—";
        return (number > 0 ? "+" : "") + fmt(number, digits);
    }

    async function pullRest() {
        try {
            const response = await fetch("/api/snapshot", {
                headers: snapshotHeaders(),
                cache: "no-store"
            });
            if (response.status === 401) {
                showTokenModal("令牌无效，请重新输入");
                setConnectionState("down", "需要验证");
                return false;
            }
            if (!response.ok) throw new Error("snapshot " + response.status);
            if (token) saveSessionToken(token);
            scheduleRender(await response.json());
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                setConnectionState("fallback", "轮询回退");
            }
            return true;
        } catch (_error) {
            if (!ws || ws.readyState !== WebSocket.OPEN) {
                const recent = lastSnapshotAt && Date.now() - lastSnapshotAt < 8000;
                setConnectionState(recent ? "fallback" : "down", recent ? "轮询异常" : "数据已中断");
            }
            return false;
        }
    }

    function connect() {
        if (authBlocked) return;
        if (ws) {
            ws.close();
            ws = null;
        }
        setConnectionState("connecting", latestSnapshot ? "正在重连" : "连接中");
        try {
            ws = new WebSocket(websocketURL());
        } catch (_error) {
            setConnectionState(lastSnapshotAt ? "fallback" : "down", lastSnapshotAt ? "轮询回退" : "连接失败");
            scheduleReconnect();
            return;
        }

        ws.onopen = () => {
            setConnectionState("live", "实时连接");
            hideTokenModal();
        };
        ws.onclose = () => {
            if (authBlocked) return;
            const recent = lastSnapshotAt && Date.now() - lastSnapshotAt < 8000;
            setConnectionState(recent ? "fallback" : "connecting", recent ? "轮询回退" : "正在重连");
            scheduleReconnect();
        };
        ws.onerror = () => {
            if (!authBlocked) {
                const recent = lastSnapshotAt && Date.now() - lastSnapshotAt < 8000;
                setConnectionState(recent ? "fallback" : "down", recent ? "轮询回退" : "连接异常");
            }
        };
        ws.onmessage = (event) => {
            try {
                const message = JSON.parse(event.data);
                scheduleRender(message && message.type === "snapshot" ? message.data : message);
            } catch (_error) {
                // Ignore malformed frames and keep the last valid snapshot visible.
            }
        };
    }

    function scheduleReconnect() {
        clearTimeout(reconnectTimer);
        reconnectTimer = setTimeout(() => {
            if (!authBlocked) connect();
        }, 2000);
    }

    initKlineChart();
    initHourlyChart();
    initCopyOrders();
    initDeskNav();
    pullRest();
    connect();
    restTimer = setInterval(() => {
        if (!authBlocked && (!ws || ws.readyState !== WebSocket.OPEN)) pullRest();
    }, 2000);
    freshnessTimer = setInterval(updateFreshness, 1000);

    window.addEventListener("beforeunload", () => {
        clearTimeout(reconnectTimer);
        clearInterval(restTimer);
        clearInterval(freshnessTimer);
        if (chartResizeObserver) chartResizeObserver.disconnect();
        if (ws) ws.close();
    });
})();
