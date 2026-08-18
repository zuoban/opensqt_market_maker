(function () {
    const params = new URLSearchParams(location.search);
    let token = params.get("token") || localStorage.getItem("opensqt_dash_token") || "";
    let ws = null;
    let reconnectTimer = null;
    let restTimer = null;
    let showOutside = false;

    const $ = (id) => document.getElementById(id);

    $("showOutside").addEventListener("change", (e) => {
        showOutside = e.target.checked;
    });

    $("tokenForm").addEventListener("submit", (e) => {
        e.preventDefault();
        token = $("tokenInput").value.trim();
        localStorage.setItem("opensqt_dash_token", token);
        $("tokenMask").classList.add("hidden");
        $("tokenErr").textContent = "";
        connect();
    });

    function withToken(url) {
        if (!token) return url;
        const u = new URL(url, location.origin);
        u.searchParams.set("token", token);
        return u.toString();
    }

    function fmt(n, d) {
        if (n === undefined || n === null || Number.isNaN(n)) return "—";
        return Number(n).toLocaleString("en-US", {
            minimumFractionDigits: d ?? 2,
            maximumFractionDigits: d ?? 2
        });
    }

    function ago(ms) {
        if (ms === undefined || ms === null) return "";
        if (ms < 1000) return ms + "ms 前";
        if (ms < 60000) return Math.round(ms / 1000) + "s 前";
        return Math.round(ms / 60000) + "m 前";
    }

    function isActive(status) {
        return status === "PLACED" || status === "CONFIRMED" || status === "PARTIALLY_FILLED";
    }

    function setPill(el, live) {
        el.classList.toggle("live", !!live);
        el.classList.toggle("down", !live);
        el.querySelector("span").textContent = live ? "Live" : "断开";
    }

    function render(s) {
        if (!s) return;
        const app = s.app || {};
        const pos = s.position || {};
        const risk = s.risk || {};
        const acc = s.account || {};
        const price = s.price || {};
        const dec = pos.priceDecimals ?? 2;

        $("pair").textContent = (app.exchange || "—") + " · " + (app.symbol || pos.symbol || "—");
        $("lastPrice").textContent = price.lastText || fmt(price.last, dec);
        $("priceMeta").textContent = price.ageMs != null ? ("报价 " + ago(price.ageMs)) : "等待价格";
        $("version").textContent = s.version || "";

        const triggered = !!risk.triggered;
        const riskPill = $("riskPill");
        riskPill.textContent = !risk.enabled ? "风控关闭" : (triggered ? "风控已触发 · 暂停买单" : "风控正常");
        riskPill.classList.toggle("hot", triggered);
        riskPill.classList.toggle("ok", risk.enabled && !triggered);

        const quote = acc.quoteAsset || "USDT";
        const realized = pos.realizedPnl != null ? pos.realizedPnl : 0;
        const estimated = pos.estimatedProfit || 0;
        const profitCls = realized >= 0 ? "pos" : "neg";
        const estimatedCls = estimated >= 0 ? "pos" : "neg";
        const pnlCls = (acc.unrealizedPnl || 0) >= 0 ? "pos" : "neg";
        $("kpis").innerHTML = [
            kpi("可用余额", fmt(acc.available) + " " + quote, acc.stale ? "warn" : ""),
            kpi("保证金", fmt(acc.margin) + " " + quote, acc.stale ? "warn" : ""),
            kpi("未实现盈亏", fmt(acc.unrealizedPnl) + " " + quote, pnlCls),
            kpi("已实现盈亏", fmt(realized) + " " + quote, profitCls),
            kpi("预计盈利", fmt(estimated) + " " + quote, estimatedCls),
            kpi("累计买 / 卖", fmt(pos.totalBuyQty, 4) + " / " + fmt(pos.totalSellQty, 4)),
            kpi("活动买 / 卖", (pos.activeBuyOrders || 0) + " / " + (pos.activeSellOrders || 0)),
            kpi("持仓槽位", (pos.filledSlotCount || 0) + " · " + fmt(pos.positionQty, pos.quantityDecimals || 4)),
            kpi("对账", pos.lastReconcileTime ? new Date(pos.lastReconcileTime).toLocaleTimeString() : "—"),
            kpi("保证金锁", pos.marginLocked ? (fmt(pos.marginLockRemainingSec, 0) + "s") : "未锁定", pos.marginLocked ? "warn" : ""),
            kpi("网格 / 间距", fmt(pos.gridPrice, dec) + " / " + fmt(pos.priceInterval, dec)),
            kpi("每单金额", fmt(pos.orderQuantity || app.orderQuantity)),
            kpi("窗口 买/卖", (pos.buyWindowSize || 0) + " / " + (pos.sellWindowSize || 0))
        ].join("");

        renderLadder(pos);
        renderRisk(risk);
        renderLogs(s.logs || []);
        renderTables(pos);
    }

    function kpi(lab, val, cls) {
        return '<div class="kpi"><div class="lab">' + lab + '</div><div class="val ' + (cls || "") + '">' + val + "</div></div>";
    }

    function renderLadder(pos) {
        const slots = pos.slots || [];
        const empty = $("ladderEmpty");
        const box = $("ladder");
        if (!slots.length) {
            empty.style.display = "block";
            box.innerHTML = "";
            return;
        }
        empty.style.display = "none";
        const grid = pos.gridPrice;
        const rows = [];
        slots.forEach((slot) => {
            const inWin = slot.inBuyWindow || slot.inSellWindow;
            if (!inWin && !showOutside && Math.abs(slot.price - grid) > 1e-12) return;
            let cls = "is-out";
            if (Math.abs(slot.price - grid) <= 1e-12) cls = "is-mark";
            else if (slot.orderSide === "SELL" && isActive(slot.orderStatus)) cls = "is-sell";
            else if (slot.positionStatus === "FILLED") cls = "is-pos";
            else if (slot.orderSide === "BUY" && isActive(slot.orderStatus)) cls = "is-buy";
            else if (slot.inBuyWindow) cls = "is-empty";
            const width = Math.min(100, Math.max(8, (slot.positionQty || 0.01) * 800));
            const color = cls === "is-buy" ? "var(--buy)" : cls === "is-sell" ? "var(--sell)" : "var(--brass)";
            rows.push(
                '<div class="row ' + cls + '">' +
                    '<span class="px">' + (slot.priceText || fmt(slot.price, pos.priceDecimals)) + "</span>" +
                    '<span class="bar"><i style="--w:' + width + "%;--c:" + color + '"></i></span>' +
                    "<span>" + (slot.orderSide || "—") + " " + (slot.orderStatus || "") + "</span>" +
                    "<span>" + (slot.positionQtyText || fmt(slot.positionQty, pos.quantityDecimals || 4)) + "</span>" +
                    "<span>" + (slot.slotStatus || "") + "</span>" +
                "</div>"
            );
        });
        box.innerHTML = rows.join("") || '<div class="empty">窗口内暂无槽位</div>';
    }

    function renderRisk(risk) {
        $("riskMsg").textContent = risk.lastMsg || (risk.enabled ? "等待风控读数" : "主动风控未启用");
        const list = $("riskList");
        if (!risk.enabled) {
            list.innerHTML = '<div class="empty">风控关闭</div>';
            return;
        }
        const symbols = risk.symbols || [];
        if (!symbols.length) {
            list.innerHTML = '<div class="empty">暂无监控币种数据</div>';
            return;
        }
        list.innerHTML = symbols.map((x) => {
            const bad = x.abnormal || risk.triggered && x.priceBelowMA;
            return '<div class="risk-item ' + (bad ? "bad" : "ok") + '">' +
                '<div class="top"><strong>' + x.symbol + "</strong><span>" + (x.status || "—") + "</span></div>" +
                '<div class="meta">价 ' + fmt(x.currentPrice, 4) + " / 均 " + fmt(x.avgPrice, 4) +
                " (" + fmt(x.priceDeviation, 2) + "%) · 量比 ×" + fmt(x.volumeRatio, 2) + "</div></div>";
        }).join("");
    }

    function renderLogs(logs) {
        const box = $("logs");
        if (!logs.length) {
            box.innerHTML = '<div class="empty">暂无日志</div>';
            return;
        }
        box.innerHTML = logs.slice().reverse().map((l) => {
            const t = l.time ? new Date(l.time).toLocaleTimeString() : "";
            return '<div class="log ' + (l.level || "") + '">[' + t + " " + (l.level || "") + "] " +
                escapeHtml(l.message || "") + "</div>";
        }).join("");
    }

    function renderTables(pos) {
        const slots = pos.slots || [];
        const filled = slots.filter((s) => s.positionStatus === "FILLED" && s.positionQty > 0.001);
        const orders = slots.filter((s) => isActive(s.orderStatus));
        $("posBody").innerHTML = filled.length ? filled.map((s) =>
            "<tr><td>" + s.priceText + "</td><td>" + s.positionQtyText + "</td><td>" +
            (s.orderSide || "—") + " " + (s.orderStatus || "") + "</td><td>" + (s.slotStatus || "") + "</td></tr>"
        ).join("") : '<tr><td colspan="4" class="muted">无持仓</td></tr>';
        $("ordBody").innerHTML = orders.length ? orders.map((s) =>
            "<tr><td>" + (s.orderSide || "") + "</td><td>" + s.priceText + "</td><td>" +
            (s.orderStatus || "") + "</td><td>" + (s.orderId || s.clientOid || "") + "</td></tr>"
        ).join("") : '<tr><td colspan="4" class="muted">无活动订单</td></tr>';
    }

    function escapeHtml(s) {
        return String(s).replace(/[&<>"']/g, (c) => ({
            "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
        }[c]));
    }

    async function pullRest() {
        try {
            const res = await fetch(withToken("/api/snapshot"));
            if (res.status === 401) {
                $("tokenMask").classList.remove("hidden");
                $("tokenErr").textContent = "令牌无效";
                return;
            }
            if (!res.ok) throw new Error("snapshot " + res.status);
            render(await res.json());
        } catch (e) {
            setPill($("wsPill"), false);
        }
    }

    function connect() {
        if (ws) {
            ws.close();
            ws = null;
        }
        const proto = location.protocol === "https:" ? "wss:" : "ws:";
        const url = withToken(proto + "//" + location.host + "/ws");
        try {
            ws = new WebSocket(url);
        } catch (e) {
            setPill($("wsPill"), false);
            return;
        }
        ws.onopen = () => {
            setPill($("wsPill"), true);
            $("tokenMask").classList.add("hidden");
        };
        ws.onclose = () => {
            setPill($("wsPill"), false);
            scheduleReconnect();
        };
        ws.onerror = () => setPill($("wsPill"), false);
        ws.onmessage = (ev) => {
            try {
                const msg = JSON.parse(ev.data);
                if (msg && msg.type === "snapshot") render(msg.data);
                else render(msg);
            } catch (e) { /* ignore */ }
        };
    }

    function scheduleReconnect() {
        clearTimeout(reconnectTimer);
        reconnectTimer = setTimeout(connect, 2000);
    }

    pullRest();
    connect();
    restTimer = setInterval(() => {
        if (!ws || ws.readyState !== WebSocket.OPEN) pullRest();
    }, 2000);
})();
