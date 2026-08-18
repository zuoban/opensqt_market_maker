(function () {
    if (typeof module !== "undefined" && module.exports) {
        module.exports = { buildKlineGridModel };
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
    let chartResizeObserver = null;
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
        setText(riskPill, !risk.enabled
            ? "风控关闭"
            : (triggered ? "风控已触发 · 暂停买单" : "风控正常"));
        riskPill.classList.toggle("hot", triggered);
        riskPill.classList.toggle("ok", Boolean(risk.enabled) && !triggered);

        const realized = pos.realizedPnl != null ? pos.realizedPnl : 0;
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
            metric("持仓", (pos.filledSlotCount || 0) + " 槽 · " + fmt(pos.positionQty, pos.quantityDecimals || 4)),
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

        const priceValues = [];
        candles.forEach((candle) => priceValues.push(candle.low, candle.high));
        levels.forEach((level) => priceValues.push(level.price));
        let priceMin = priceValues.length ? Math.min(...priceValues) : 0;
        let priceMax = priceValues.length ? Math.max(...priceValues) : 0;
        if (priceMin > 0 && priceMax > 0) {
            const span = Math.max(priceMax - priceMin, priceMax * 0.002);
            priceMin -= span * 0.08;
            priceMax += span * 0.08;
        }

        const first = candles[0];
        const latest = candles[candles.length - 1];
        const change = first && latest ? latest.close - first.open : 0;
        const changePct = first && first.open ? change / first.open * 100 : 0;
        const candleLow = candles.length ? Math.min(...candles.map((candle) => candle.low)) : 0;
        const candleHigh = candles.length ? Math.max(...candles.map((candle) => candle.high)) : 0;
        const execution = levels.reduce((result, level) => {
            if (level.hasBuy) result.buy += 1;
            if (level.hasSell) result.sell += 1;
            if (level.hasPosition) result.position += 1;
            return result;
        }, { buy: 0, sell: 0, position: 0 });
        return {
            interval: kline.interval || "1m",
            historyReady: Boolean(kline.historyReady),
            degraded: Boolean(kline.degraded),
            candles,
            levels,
            grid,
            priceMin,
            priceMax,
            latest,
            candleLow,
            candleHigh,
            change,
            changePct,
            execution
        };
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
        setText($("klineCount"), chartModel.candles.length + " 根");

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
            replaceChildren($("klineBody"), [emptyRow("K 线数据尚未就绪", 6)]);
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
        $("klineCanvas").setAttribute("aria-label", summary + "。可使用左右方向键逐根查看。");
        renderKlineStats(chartModel, decimals);
        if (chartTooltipActive && chartSelection !== null) updateSelectedCandle();
        else setText($("klineDetail"), candleDetail(latest, decimals));
        renderKlineTable(chartModel.candles, decimals);
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

    function renderKlineTable(candles, decimals) {
        const rows = candles.slice().reverse().map((candle) => {
            const row = document.createElement("tr");
            const values = [
                ["时间", formatCompactDateTime(candle.time)],
                ["开", fmt(candle.open, decimals)],
                ["高", fmt(candle.high, decimals)],
                ["低", fmt(candle.low, decimals)],
                ["收", fmt(candle.close, decimals)],
                ["状态", candle.isClosed ? "已完结" : "进行中"]
            ];
            values.forEach(([label, value], index) => {
                const cell = element("td", index === 0 ? "fill-time" : "", value);
                cell.dataset.label = label;
                row.appendChild(cell);
            });
            return row;
        });
        replaceChildren($("klineBody"), rows);
    }

    function initKlineChart() {
        const canvas = $("klineCanvas");
        const stage = $("klineStage");
        if (!canvas || !stage) return;

        const selectAt = (clientX) => {
            if (!chartModel || !chartModel.candles.length || !chartGeometry) return;
            const rect = canvas.getBoundingClientRect();
            const localX = clientX - rect.left;
            const slot = chartGeometry.plotWidth / chartModel.candles.length;
            const index = Math.max(0, Math.min(
                chartModel.candles.length - 1,
                Math.floor((localX - chartGeometry.left) / slot)
            ));
            chartSelection = index;
            chartTooltipActive = true;
            updateSelectedCandle();
            drawKlineChart();
        };

        canvas.addEventListener("pointermove", (event) => selectAt(event.clientX));
        canvas.addEventListener("pointerdown", (event) => {
            selectAt(event.clientX);
            canvas.focus({ preventScroll: true });
        });
        canvas.addEventListener("pointerleave", () => {
            if (document.activeElement !== canvas) {
                chartTooltipActive = false;
                hideChartTooltip();
                drawKlineChart();
            }
        });
        canvas.addEventListener("focus", () => {
            if (chartModel && chartModel.candles.length) {
                if (chartSelection === null) chartSelection = chartModel.candles.length - 1;
                chartTooltipActive = true;
                updateSelectedCandle();
                drawKlineChart();
            }
        });
        canvas.addEventListener("blur", () => {
            chartTooltipActive = false;
            hideChartTooltip();
            drawKlineChart();
        });
        canvas.addEventListener("keydown", (event) => {
            if (!chartModel || !chartModel.candles.length || !["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
            event.preventDefault();
            if (chartSelection === null) chartSelection = chartModel.candles.length - 1;
            if (event.key === "ArrowLeft") chartSelection = Math.max(0, chartSelection - 1);
            if (event.key === "ArrowRight") chartSelection = Math.min(chartModel.candles.length - 1, chartSelection + 1);
            if (event.key === "Home") chartSelection = 0;
            if (event.key === "End") chartSelection = chartModel.candles.length - 1;
            chartTooltipActive = true;
            updateSelectedCandle();
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
            ink: color("--ink", "#edf7f3"),
            muted: color("--muted", "#91aaa3"),
            mutedSoft: color("--muted-soft", "#718b84"),
            line: color("--line", "rgba(143,190,177,.18)"),
            lineStrong: color("--line-strong", "rgba(143,190,177,.34)"),
            up: color("--positive", "#48dda3"),
            down: color("--negative", "#ff7d77"),
            buy: color("--buy", "#ff7d77"),
            sell: color("--sell", "#48dda3"),
            position: color("--cyan", "#5bc8ff"),
            grid: color("--warn", "#ffc857"),
            surface: color("--surface", "#0a1714")
        };
        const compact = width < 520;
        const left = compact ? 9 : 14;
        const axisWidth = compact ? 58 : 72;
        const railWidth = compact ? 68 : 98;
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
        ctx.fillStyle = "rgba(2, 9, 7, 0.5)";
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

        drawGridLevels(ctx, chartModel.levels, chartGeometry, colors);

        const candles = chartModel.candles;
        const slotWidth = plotWidth / candles.length;
        const bodyWidth = Math.max(2, Math.min(10, slotWidth * 0.62));
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
        candles.forEach((candle, index) => {
            const x = left + slotWidth * (index + 0.5);
            const openY = yForPrice(candle.open);
            const closeY = yForPrice(candle.close);
            const highY = yForPrice(candle.high);
            const lowY = yForPrice(candle.low);
            const bullish = candle.close >= candle.open;
            const candleColor = bullish ? colors.up : colors.down;
            ctx.setLineDash([]);
            ctx.strokeStyle = candleColor;
            ctx.lineWidth = candle.isClosed ? 1 : 1.5;
            ctx.beginPath();
            ctx.moveTo(x, highY);
            ctx.lineTo(x, lowY);
            ctx.stroke();
            const bodyTop = Math.min(openY, closeY);
            const bodyHeight = Math.max(1.5, Math.abs(openY - closeY));
            if (bullish) {
                ctx.fillStyle = candleColor;
                ctx.fillRect(x - bodyWidth / 2, bodyTop, bodyWidth, bodyHeight);
            } else {
                ctx.fillStyle = colors.surface;
                ctx.fillRect(x - bodyWidth / 2, bodyTop, bodyWidth, bodyHeight);
                ctx.strokeRect(x - bodyWidth / 2, bodyTop, bodyWidth, bodyHeight);
            }
            if (!candle.isClosed) {
                ctx.strokeStyle = colors.position;
                ctx.strokeRect(x - bodyWidth / 2 - 1, bodyTop - 1, bodyWidth + 2, bodyHeight + 2);
            }
        });

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

        if (chartTooltipActive && chartSelection !== null && candles[chartSelection]) {
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
            positionChartTooltip(x, y, width, height, selected);
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

    function drawGridLevels(ctx, levels, geometry, colors) {
        const activeLevels = [];
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
            if (level.markers.length) activeLevels.push({ level, y, color: settings[0] });
        });

        const labelItems = activeLevels.sort((a, b) => a.y - b.y).map((item) => ({
            ...item,
            labelY: item.y
        }));
        labelItems.forEach((item, index) => {
            item.labelY = Math.max(
                geometry.top + 9,
                index ? Math.max(item.y, labelItems[index - 1].labelY + 19) : item.y
            );
        });
        const overflow = labelItems.length
            ? labelItems[labelItems.length - 1].labelY - (geometry.top + geometry.plotHeight - 9)
            : 0;
        if (overflow > 0) {
            for (let index = labelItems.length - 1; index >= 0; index--) {
                const nextY = index === labelItems.length - 1
                    ? geometry.top + geometry.plotHeight - 9
                    : labelItems[index + 1].labelY - 19;
                labelItems[index].labelY = Math.min(labelItems[index].labelY - overflow, nextY);
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
            ctx.fillStyle = "rgba(3, 9, 7, 0.88)";
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
        const left = Math.max(8, Math.min(width - tooltipWidth - 8, x + 13));
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
            const card = element("div", "risk-item " + (bad ? "bad" : "ok"));
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
            return element(
                "div",
                "log" + (level ? " " + level : ""),
                "[" + time + (level ? " " + level : "") + "] " + (item.message || "")
            );
        });
        replaceChildren(box, items);
    }

    function renderTables(pos, quote) {
        const filledOrders = pos.filledOrders || [];

        const filledOrderCount = pos.filledOrderCount ?? filledOrders.length;
        setText($("filledCount"), "本次运行 · " + filledOrderCount + " 笔");
        const filledRows = filledOrders.map((order) => {
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
        cell.appendChild(code);
        return cell;
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
