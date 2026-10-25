# 下單、掛單、止盈止損與成交偵測：現況盤點與事件驅動設計（草稿，待審）

> 狀態：**草稿，只有盤點與設計，沒有改任何程式**。第 5 節是待你決定的選項。
> 起因：2026-10-25 #93 SUI 多單移動停損後已經平倉，但頁面沒偵測到。

---

## 1. #93 實際發生了什麼（只讀查證）

**journal.csv 現在的那一列**：持倉中（沒有 `closed_at`、`exit_price`、`outcome`）

```
#93 SUI long 15m  進場 0.920912  停損 0.909  TP1 —  TP2 0.948  100x  保證金 15u
filled_at       2026-10-25 01:37:25
entry_order_id  2114039166723649536
stop_order_id   2114229730156326912   ← 已被「移動停損」改過
tp2_order_id    ATTACHED:2114039166723649536
```

**交易所（/ops/fills，BingX fillHistory）**：

| 時間 (TPE) | 方向 | 數量 | 均價 | 已實現損益 | 手續費 | orderId |
|---|---|---|---|---|---|---|
| 10-25 01:37:23 | BUY（開多） | 1628 | 0.9209 | 0 | −0.2998 | 2114039166723649536（= entry_order_id） |
| 10-25 13:38:16 | SELL（平多） | 1628 | 0.92493 | **+6.5623** | −0.7529 | 2114230036026585088 |

**/ops/verify**：交易所**沒有 SUI 部位、沒有停損單**，判定「journal 記為已成交，但交易所沒有部位」。

**結論**
- 部位在 **13:38:16 全數平倉**，出場均價 0.92493。
- 以 journal 的停損 0.909 為 R 基準：(0.92493 − 0.920912) / (0.920912 − 0.909) ≈ **+0.34R**。
  毛利 +6.56u，兩邊手續費合計 −1.05u，淨約 **+5.51u**。
- 平倉單的 orderId 不是 `stop_order_id`。推論（**未直接查證**）：BingX 的 orderId 前幾碼帶時間戳，
  依編號推算，新停損單約在 13:37:03 掛上，73 秒後的 13:38:16 被觸發，觸發後由交易所產生一張新的市價單成交
  ——所以是「移動後的停損打到」，出場價略低於停損價屬於停損市價單的滑價。
  要百分之百確認，得用 OrderHistory 查 `2114229730156326912` 的狀態。
- **journal 沒有更新是設計使然，不是 bug**：系統目前**沒有任何「自動平倉回填」**（見第 2、3 節）。
  我沒有改 journal，等你決定要手動平倉，還是等第 5 節的功能做好再回填。

---

## 2. 現況盤點：每一段是誰、在哪裡、多久一次、依據什麼

| 階段 | 誰負責 | 在哪 | 頻率 | 依據什麼判斷 |
|---|---|---|---|---|
| **開倉（手動）** | cmd/web | `/journal/new` → `placeEntryOnBingX`；`/chart` 快速記錄勾「開倉」走同一條路 | 使用者按下時 | 下限價單；停損、TP2 用 BingX 的「附掛」綁在進場單上，journal 記 `ATTACHED:<entryId>` |
| **開倉（自動）** | cmd/monitor 的 autoexec | `cmd/monitor/autoexec.go` | 每 60 秒 | **目前只有紙上模式**，寫 `autotrade_paper.jsonl`，不碰 journal |
| **進場成交偵測** | cmd/web `tradeSweepLoop` pass 1 | `handlers.go` `sweepTrades` | 每 30 秒 | **推論，不是交易所確認**：開倉後的 K 棒最低/最高價碰到進場價，或現價已越過進場價，就寫 `filled_at` |
| **停損 / TP2** | 交易所 | 附掛在進場單上 | 成交瞬間 | 進場一成交，交易所自動啟用 |
| **TP1 補掛** | cmd/web sweep pass 2 | `placeTP1OnBingX` | 每 30 秒 | `filled_at` 有值、`tp1_auto` 開、`tp1_order_id` 空 → 補掛 reduce-only 限價單（只掛一部分，所以不能用附掛） |
| **TP1 分批成交** | cmd/web sweep pass 3 | `detectTP1Legs` | 每 30 秒 | **交易所確認**：OrderHistory 找 `tp1_order_id`，FILLED 才寫入 TP1 成交價、比例、時間 |
| **移動停損** | cmd/web | `/today` → `POST /api/trade/:id/move-stop` | 使用者按下時 | 先掛新停損、再取消舊的；**journal 的 stop 不變**（它是 R 基準），只更新 `stop_order_id` |
| **裸倉偵測** | cmd/monitor bracket guard | `cmd/monitor/bracket.go` | 每 30 秒 | **交易所是狀態**：有部位但沒有 reduce-only 停損 → 推播。只讀 journal，從不寫入 |
| **部位已消失偵測** | cmd/monitor bracket guard | 同上，`StateStale` | 每 30 秒 | journal 說持倉、交易所沒有部位 → **只寫一行 log**，不推播、不告警、不回填 |
| **平倉回填** | **沒有** | — | — | 停損 / TP2 打掉剩餘部位後，journal 要使用者到平倉表單手動填 |

**兩個重點**

1. **「部位已消失」其實已經偵測到了。** bracket guard 每 30 秒就會判定 #93 是 `stale`，
   但程式刻意只寫 log（註解：「不是緊急狀況，也不能採取動作」）。所以缺的不是偵測，是**通知和回填**。
2. **cmd/web 是 journal 唯一的寫入者。** `handlers.go` 寫明理由：monitor 若也寫 journal，
   兩個 process 同時 read-modify-write 同一個 CSV 會讓列消失；sweep 期間也會重讀 journal、
   用 `mergeSweep` 只帶回自己負責的欄位。**任何自動回填都必須在 cmd/web、在 `sweepMu` 底下做。**

**另一個連帶的弱點**：進場成交是用 K 線「碰到」推論的。限價單被碰到不一定成交（排隊沒輪到），
這種「碰到沒成交」會被記成已成交。TP1 那段已經用 OrderHistory 做交易所確認，進場可以照做。

---

## 3. BingX 有沒有可以觸發的 hook？

**沒有 HTTP webhook**（不會主動打你的 URL）。**有 user data stream**，已對照 BingX 官方 GitHub
（BingX-API/api-ai-skills 的 `swap-ws-account`）：

| 項目 | 內容 |
|---|---|
| 建立 listenKey | `POST /openApi/user/auth/userDataStream`（要簽名），有效 **1 小時** |
| 延長 | `PUT` 同路徑，延長為 60 分鐘，**建議每 30 分鐘一次** |
| 刪除 | `DELETE` 同路徑 |
| WebSocket | `wss://open-api-swap.bingx.com/swap-market?listenKey=<key>`，連上後自動推送，不用訂閱 |
| 協定 | gzip 二進位訊息、文字 `Ping` 要回 `Pong`——**跟現有 `bingx/ws.go` 的行情串流一樣**，可以共用 |

**事件**（注意名稱是 `TRADE_UPDATE`，不是 Binance 的 `ORDER_TRADE_UPDATE`）：

| 事件 | 什麼時候 | 對我們有用的欄位 |
|---|---|---|
| `TRADE_UPDATE` | 下單、成交、取消、過期 | `o.i` orderId、`o.X` 狀態（NEW/PARTIALLY_FILLED/FILLED/CANCELED/EXPIRED）、`o.x` 執行類型、`o.o` 單別（含 STOP_MARKET、TAKE_PROFIT_MARKET、LIQUIDATION）、`o.ap` 均價、`o.z` 累計成交量、`o.rp` 已實現損益、`o.ps` 持倉方向、**`o.li` 附掛停損/止盈的母單 id** |
| `ACCOUNT_UPDATE` | 成交、入出金、資金費影響餘額或部位時 | `a.m` 原因（ORDER/FUNDING_FEE/…）、`a.P[].pa` 部位數量、`a.P[].s` 交易對、`a.P[].ps` 方向 |
| `listenKeyExpired` | listenKey 過期 | 要重建 key 重連 |
| `ACCOUNT_CONFIG_UPDATE` | 連上時一次 + 每 5 秒 | 槓桿、保證金模式 |

`o.li` 特別有用：附掛的停損/TP2 觸發時，它指回進場單 id，而 journal 正好記著 `entry_order_id`
——**可以直接對回 journal 的哪一列**。

**WebSocket 的本質限制**：斷線期間的事件不會補送。所以事件串流只能讓偵測「變快」，
**不能取代**定期向 REST 對帳（不然斷線 5 分鐘就漏一筆平倉）。

---

## 4. 設計選項

### 方案 A：把現成的 `stale` 偵測變成通知 + 一鍵帶入（最小）

- monitor 的 `StateStale` 改成發 alert（鈴鐺 + ntfy，嚴重度 warning），而不是只寫 log。
- `/today` 持倉卡偵測到「交易所已無部位」時，顯示「依交易所成交平倉」按鈕：
  打開平倉表單，**預填**出場均價（fillHistory 裡 `entry_order_id` 之後、方向相反的成交加權平均）、
  平倉時間、建議的 outcome；**使用者確認後才寫入**。
- 優點：改動小、零自動寫入風險、立刻解決「沒偵測到」。缺點：還是要人按。

### 方案 B：REST 輪詢自動平倉（在現有 sweep 裡）

- sweep 多一個 pass：對「已成交、還沒平倉」的 trade 查部位；**連續兩輪**（約 60 秒）都沒有部位，
  才查 fillHistory、算出場均價、寫入平倉、取消殘留掛單。
- 兩輪確認是為了避開交易所 API 偶發回空（`risk` 套件已記錄過「部位列表回空、但保證金顯示有倉」的矛盾）。
- 優點：不用 WebSocket，邏輯全在既有的 sweep 和 `sweepMu` 底下；延遲最多約 60 秒。
- 缺點：每 30 秒多打部位 API（只在有持倉時）；要處理部分平倉、手動平倉混合等邊界。

### 方案 C：事件驅動（user data stream）+ REST 對帳當保險

```
BingX user stream ──► cmd/web 的 listener goroutine ──► 標記「這個 symbol 有變動」
                                                          │
                                                          ▼
                                     立刻觸發一次 sweep（同一個 sweepMu、同一套回填邏輯）
                         定時 sweep（30 秒）照跑 ── 斷線期間的事件由它補上
```

- listener **只負責叫醒 sweep**，不直接寫 journal——寫入規則（唯一寫入者、`sweepMu`、`mergeSweep`）完全不變。
- 需要：listenKey 建立與每 30 分鐘延長、斷線重連（指數退避）、`listenKeyExpired` 處理、健康狀態接進 alert。
- 優點：平倉後幾秒內回填；`o.li`、`o.rp` 讓對帳更精準；之後要做 Kafka 時，這些事件正好是**真實、有順序、會重複**的事件來源。
- 缺點：最多的程式和維運面（多一條長連線、多一種故障模式）；**仍然需要 B 的對帳邏輯**，所以 C 是建立在 B 之上。

### 不管選哪個，自動平倉都要回答的問題

| 問題 | 建議 |
|---|---|
| outcome 怎麼判斷？ | 平倉單是 STOP_MARKET → `stop`；TAKE_PROFIT_MARKET 且是 TP2 → `tp2`；LIQUIDATION → `liquidated`；其他 → `manual`。REST 版沒有單別時，用出場價離哪個價位近推斷，並在 close_notes 寫明「自動推斷」 |
| 移動停損後打到的，算 stop 嗎？ | 算 `stop`，但 R 照 journal 的停損（R 基準）計算，所以會是 +0.34R 這種正值。這跟現有「journal stop 是 R 基準」的設計一致 |
| 有 TP1 分批過？ | 剩餘部位的出場價用 `BlendedExit` 跟 TP1 腿合併，跟手動平倉表單的「blend_tp1」同一套 |
| 使用者手動平了一半？ | 部位還在就不動；只有部位歸零才平倉。部分平倉先不自動處理 |
| 殘留掛單？ | 照平倉表單現有行為取消（預設開） |
| 寫錯了怎麼辦？ | close_notes 標「auto-closed from exchange fills」，保留所有成交 id；journal 每天有備份可以還原 |
| 進場成交偵測 | 順便改成用 OrderHistory 確認 `entry_order_id` 為 FILLED（取代 K 線碰到推論），成交價、時間都用交易所的 |

---

## 5. 待你決定

1. **#93 現在怎麼處理？** (a) 你手動平倉（出場 0.92493、13:38:16、outcome `stop`，約 +0.34R）
   (b) 先放著，等功能做好讓它自動回填（順便當第一個實測案例）
2. **要做哪個方案？** 建議 **先 A，再 B**；C 等 B 跑穩、或你想拿它當 Kafka 的事件來源時再做。
3. **自動平倉的寫入權限**：B/C 要直接寫 journal，還是一律「預填、等你確認」（= 永遠停在 A 的模式）？
4. **進場成交偵測**要不要一起改成交易所確認？（建議要，同一批改動）
5. **要不要留白給你寫？** 適合的片段：outcome 判斷函式、「連續兩輪沒有部位」的狀態機、
   user stream 的重連與 listenKey 延長迴圈（goroutine + context，正好是練習題）。
