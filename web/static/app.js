(function () {
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
        renderKeys.ladder = "";
        if (latestSnapshot) renderLadder(latestSnapshot.position || {});
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
        setText($("lastPrice"), price.lastText || fmt(price.last, dec));
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

        updateSection("ladder", {
            slots: pos.slots || [],
            gridPrice: pos.gridPrice,
            priceDecimals: pos.priceDecimals,
            quantityDecimals: pos.quantityDecimals,
            showOutside
        }, () => renderLadder(pos));
        updateSection("risk", risk, () => renderRisk(risk));
        updateSection("logs", snapshot.logs || [], () => renderLogs(snapshot.logs || []));
        updateSection("tables", {
            slots: pos.slots || [],
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

    function renderLadder(pos) {
        const slots = pos.slots || [];
        const empty = $("ladderEmpty");
        const box = $("ladder");
        if (!slots.length) {
            empty.hidden = false;
            box.replaceChildren();
            return;
        }

        empty.hidden = true;
        const grid = pos.gridPrice;
        const rows = [];
        slots.forEach((slot) => {
            const inWindow = slot.inBuyWindow || slot.inSellWindow;
            if (!inWindow && !showOutside && Math.abs(slot.price - grid) > 1e-12) return;

            let stateClass = "is-out";
            if (Math.abs(slot.price - grid) <= 1e-12) stateClass = "is-mark";
            else if (slot.orderSide === "SELL" && isActive(slot.orderStatus)) stateClass = "is-sell";
            else if (slot.positionStatus === "FILLED") stateClass = "is-pos";
            else if (slot.orderSide === "BUY" && isActive(slot.orderStatus)) stateClass = "is-buy";
            else if (slot.inBuyWindow) stateClass = "is-empty";

            const row = element("div", "row " + stateClass);
            row.setAttribute("role", "listitem");
            if (stateClass === "is-mark") row.setAttribute("aria-current", "true");

            const price = element("span", "px", slot.priceText || fmt(slot.price, pos.priceDecimals));
            const bar = element("span", "bar");
            bar.setAttribute("aria-hidden", "true");
            const fill = element("i");
            const width = Math.min(100, Math.max(8, (slot.positionQty || 0.01) * 800));
            const color = stateClass === "is-buy"
                ? "var(--buy)"
                : (stateClass === "is-sell" ? "var(--sell)" : "var(--brass)");
            fill.style.setProperty("--w", width + "%");
            fill.style.setProperty("--c", color);
            bar.appendChild(fill);

            let orderLabel = ((slot.orderSide || "") + " " + (slot.orderStatus || "")).trim() || "—";
            if (stateClass === "is-mark") orderLabel = "现价";
            else if (stateClass === "is-empty") orderLabel = "空槽";
            const order = element("span", "order-state", orderLabel);
            const quantity = element("span", "", slot.positionQtyText || fmt(slot.positionQty, pos.quantityDecimals || 4));
            const status = element("span", "", slot.slotStatus || "—");
            row.append(price, bar, order, quantity, status);
            rows.push(row);
        });

        const scrollTop = box.scrollTop;
        box.setAttribute("role", "list");
        replaceChildren(box, rows.length ? rows : [element("div", "empty", "窗口内暂无槽位")]);
        box.scrollTop = scrollTop;
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
        const slots = pos.slots || [];
        const filledOrders = pos.filledOrders || [];
        const orders = slots.filter((slot) => isActive(slot.orderStatus));

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

        const orderRows = orders.map((slot) => {
            const row = document.createElement("tr");
            appendCell(row, "方向", slot.orderSide || "—");
            appendCell(row, "价格", slot.priceText || fmt(slot.price, pos.priceDecimals));
            appendCell(row, "状态", slot.orderStatus || "—");
            row.appendChild(orderIDCell(String(slot.orderId || slot.clientOid || "—")));
            return row;
        });
        replaceChildren($("ordBody"), orderRows.length ? orderRows : [emptyRow("无活动订单")]);
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
        if (ws) ws.close();
    });
})();
