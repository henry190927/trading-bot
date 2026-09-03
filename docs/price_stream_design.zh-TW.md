# 即時價格串流 — 設計文件（中譯＋解說）

> 本檔為 `price_stream_design.md` 的繁體中文翻譯。技術名詞（SSE / WebSocket / hub /
> backpressure / last-value-wins 等）保留原文。`> 💡` 開頭的段落是**譯註解說**，
> 原文沒有，用來補上「為什麼這樣寫」的背景。

這份文件同時放了兩件事：

1. **已經上線的東西**（2026-09-03）：由 server 擁有的 fan-out，走 **SSE**。第 1–4 節。
2. **刻意「沒有」實作的 WebSocket 設計**，以 SD（system design）練習的形式寫下來。
   第 5–9 節。它是一份**設計紀錄**，不是 TODO。

> 💡 這個結構本身就是重點：把「做了什麼」和「為什麼不做另一種」分開寫。面試場合
> 「我知道 WS，但這裡不需要，理由如下」比直接上 WS 更有說服力。

---

## 1. 問題

首頁的行情表格、以及 chart 頁上方的 ticker 條，兩邊都在顯示價格。每一個開著的分頁
都各自跑一份 `setInterval` + `fetch('/api/tickers')`。

後果：

- **延遲下限 = 輪詢間隔。** 設 10s 時表格看起來像靜止的，tick 閃動效果幾乎不會觸發。
  降到 5s 手感變好，但對上游（交易所）的呼叫次數直接乘 2。
- **每個 client 重複做同一份工。** 每次刷新 11 個 symbol × 2 次 REST 呼叫。
  server 端 3s 的 cache 讓上游流量大致持平，但**每個瀏覽器仍然要付一次來回**，
  而且各自再解析一次 JSON。
- **永遠在輪詢。** 只要有分頁開著就一直打交易所，即使根本沒人在看。

> 💡 三個症狀對應三種成本：延遲（使用者感受）、重複計算（浪費）、閒置浪費（無人看仍消耗）。
> 分開列出來，才能在第 3 節逐條說「這個解法各解掉哪一條」。

## 2. 需求（在選傳輸協定「之前」先寫下來）

傳輸協定的選擇是從這些需求推導出來的，所以需求先講。

| | |
|---|---|
| 方向 | **只有 server → client。** client 永遠不會在這條通道上送東西。 |
| 值的語意 | **Last-value-wins（後值覆蓋前值）。** 被取代的舊價格不帶任何資訊。 |
| 容忍遺失 | **高。** 漏掉中間某一個 tick 使用者看不出來；漏掉**最新的**那個才會被看出來。 |
| 順序 | 不可以在新價格之後又顯示舊價格。 |
| Fan-out | 讀者數量無上限，上游只有一條。 |
| 新鮮度目標 | ~3s，與交易所 REST 的 cache 對齊。 |
| Auth | 不需要 — 公開行情資料，沒有 per-user 狀態。 |

> 💡 這張表是整份文件的樞紐。第 3 節的每一個決定（選 SSE、drop-oldest、只序列化一次）
> 都直接引用這張表的某一列。**先定語意，再選機制**，而不是反過來。

## 3. 決策：單一上游 + SSE

**Ownership（採 design B）。** 由一個 server 端 goroutine 負責刷新，瀏覽器只是訂閱者。
上游成本與觀眾人數無關。另一個選項——讓每個瀏覽器各自連交易所——會把憑證與
rate-limit 的暴露面推到 client，而且 N 個分頁就是 N 條上游連線。

**傳輸：選 SSE，不選 WebSocket。** 對照第 2 節那張表，WS 的雙向能力是多餘的重量。
SSE 就是純 HTTP/1.1（`text/event-stream`），瀏覽器會自己重連，也沒有 upgrade
handshake 可以搞砸。什麼情況下這個結論會反轉，見第 5 節。

**Backpressure：容量 1 的 bounded channel，drop-OLDEST（丟舊的）。** 一個被切到背景、
或網路卡住的分頁，不可以拖垮 hub 或其他訂閱者。每個 subscriber 只有一格 buffer；
滿了就把**還沒送出的**那份 snapshot 丟掉、換成新的。

> Drop-oldest 的正當性來自**值的語意**，不是來自方便。在價格 feed 上，那個還沒送出的
> 舊值本來就沒有價值——丟掉是零成本，慢的 client 下次讀取時就會 resync 回真相。
> 同樣的策略如果套在 order feed、trade feed 或成交回報上，就是**bug**：那些是
> event-semantics，每一則訊息都是一個獨立的事實。**先講清楚語意，再挑丟棄策略。**

> 💡 這是整份文件最值得記住的一句。同一份程式碼（bounded channel + 丟棄）在 A 場景是
> 正確設計、在 B 場景是資料遺失事故，差別只在資料語意。第 10b 節在 WS 上游那邊
> 又重申了一次同樣的警告。

**每個 tick 只序列化一次。** payload 對所有人都相同，所以只 marshal 一次，同一份
`[]byte` 交給每一個 subscriber。這是唯一一個「會隨觀眾數量成長」的 per-client 成本。

**Lazy lifecycle（惰性生命週期）。** poller 在第一個 subscriber 進來時啟動，最後一個
離開時停止，所以閒置的部署對交易所是零呼叫。

**Replay-on-connect（連上就補一份現值）。** 新的 subscriber 一連上就立刻收到最新的
snapshot，所以新開的分頁不用等一個間隔才有畫面。

## 4. 已上線的形狀

```
BingX REST ──► buildTickers() ──► tickerHub.refresh()
  (唯一呼叫者)                        │ 只 marshal 一次
                                      ▼
                              broadcast() ──► chan[1] ──► SSE ──► 分頁
                                          ──► chan[1] ──► SSE ──► 分頁
                                          ──► chan[1] ──► SSE ──► 分頁
```

- `cmd/web/stream.go` — `tickerHub`、`handleStreamTickers`
- `cmd/web/chart_bias.go` — `buildTickers`（抽出來的；`/api/tickers` 與 hub 共用，
  所以兩種傳輸不可能給出不同的數字）
- `cmd/web/templates/today.html` — `EventSource`、輪詢 fallback
- `GET /api/stream/tickers`

**資料來源是注入進去的**（`func(context.Context) any`），而不是讓 hub 反過來去摸
`*server`。這讓 hub 不需要交易所就能測試，而且**這正是未來換成交易所 websocket 時
要動的那條縫**：只有 `pollLoop` 會變，subscriber / backpressure / 傳輸層都不用動。

> 💡 「seam（接縫）」這個設計選擇在第 10b 節得到回報——WS 上游上線時，hub 一行沒改。

Client 端值得一提的行為：

- 先用 REST 畫第一幀，再交棒給 SSE — 連線期間不會出現空白表格。
- 輪詢是在**收到第一個 event 時**才拆掉，不是在 `onopen`。因為若有 proxy 把
  `text/event-stream` 緩衝成靜默，只看 `onopen` 會讓表格卡在「連線是開的但沒資料」。
- `onerror` **不是**失敗路徑 — EventSource 每次短暫斷線都會觸發它，然後自己重連。
  它只在**斷線期間**重新啟用輪詢。
- 每 20s 送一個 heartbeat 註解（`: ping`），避免中間設備把安靜的連線回收。
  `X-Accel-Buffering: no` 用來擋掉 nginx 那類的 response buffering。

**部署後實測**（3 個並發 client）：三邊拿到完全相同的值、在完全相同的時刻，約 3s 一拍，
一次 session 只有一組 `hub started` / `hub stopped`。

---

## 5. WebSocket：什麼時候它才是對的選擇

SSE 在這裡贏，**是因為**第 2 節那張表。把其中任何一列翻掉，答案就改變：

| 改變 | 為什麼 WS 就變成正確解 |
|---|---|
| client 需要送東西（訂閱部分 symbol、下單） | SSE 沒有上行通道；你得再補一個 POST endpoint，然後就有兩個東西要保持同步 |
| Binary payload | SSE 只有 UTF-8 文字 framing；二進位要 base64，約 33% 額外開銷 |
| 每個 client 要開很多條 stream | HTTP/1.1 每個 origin 大約只允許 6 條連線，而每條 SSE 就佔一條。HTTP/2 能多工解掉這點，但前提是整條路徑都在你控制之下 |
| 次秒級、高頻的 tick | SSE 的 per-event framing 開銷與文字編碼開始有感 |
| server 需要精確知道對方是否還活著 | WS 有協定層級的 ping/pong；SSE 只能從寫入錯誤去「推論」對方死了 |

對**這個**系統來說，目前一條都不成立。真正會觸發改變的是「從瀏覽器下單」——而那是一條
需求完全不同的通道（auth、exactly-once、event-semantics），所以它比較可能是**自己開一條
新連線**，而不是把這一條改成 WS 的理由。

## 6. WS 版本必須自己解決、但 SSE 免費給你的東西

這一節最值得反覆演練 — SSE 幫你藏起了四個問題。

**6.1 重連與 backfill。** EventSource 會自動重連，並帶上 `Last-Event-ID`。WS 什麼都不給：
重連迴圈、exponential backoff、jitter（避免 N 個分頁同時衝擊剛復原的 server）、上限，
全部要自己寫。而且你得決定「斷線那段空缺」怎麼辦。

對 last-value-wins 的資料答案很簡單 — **不要 backfill，連上時直接重送當前狀態**。
這句話要**明講出來**，因為在 event-semantics 的 stream 上這麼做就是錯的，
而評審正在看你知不知道自己手上是哪一種。

> 💡 對應記憶中的 feedback：SD 面試「講出來」才算分，寫在心裡不算。

**6.2 Keepalive 與 half-open 偵測。** 一條 TCP 連線可能已經死了，但兩端都還以為它開著
（NAT timeout、筆電睡眠）。WS 有 ping/pong frame：server 每 ~30s ping 一次，期待 ~10s 內
收到 pong，否則關閉。沒有這個機制，server 會累積殭屍 subscriber，並持續為沒有人的對象
做序列化。

**6.3 Upgrade handshake。** `Connection: Upgrade`、`Sec-WebSocket-Key`、那個 magic GUID、
SHA-1/base64 的 accept 值。**不轉發 upgrade header 的 proxy** 是經典的正式環境驚喜。
另外還有 `Origin` 檢查 — WS **不**受 same-origin policy 約束，所以除非你自己檢查
`Origin`，任何網站都能對你的 server 開一條 WS。（SSE 因為行為近似 `fetch`，是受 CORS
管制的。）這是實打實的安全性差異，不是細節。

**6.4 每條連線的寫入序列化。** 對同一條 WS 連線並行寫入會弄壞 frame stream。
每條連線都需要**單一 writer goroutine** 從它的 channel 消費 — 而 bounded-buffer 的
backpressure 設計就住在那裡，策略與第 3 節相同。

## 7. 如果真的要蓋，草圖長這樣

```
                   ┌──────────── 每個 process 一份 ────────────┐
exchange WS ──► upstreamClient ──► hub ──► sessions map
   │              (reconnect,        │      ┌── session ──┐
   │               resubscribe,      ├─────►│ chan[N]     │──► writer goroutine ──► conn
   │               seq gap detect)   │      │ ping timer  │◄── reader goroutine ◄── conn
   └── ping/pong                     │      └─────────────┘        (pong, close)
                                     └─────► ...
```

- **`upstreamClient`** 只擁有一條交易所連線。斷線時：backoff+jitter 重連，然後
  **把每一個訂閱重送一次**（交易所不會幫你記住），並把狀態標記為 stale 直到第一則
  訊息抵達。
- **`hub`** 與 SSE 版本完全相同 — 一樣的 broadcast、一樣的 drop-oldest。
  這就是把接縫放在 `source` 換來的回報。
- **`session`** = 一個 client。reader goroutine 處理 pong/close 與任何 client→server 的
  訊息；writer goroutine 是**唯一**會碰那條連線的東西。
- **依 symbol 子集訂閱**是 WS 在這裡唯一真正解鎖的功能：只看 BTC 的分頁就只收到 BTC。
  但這會讓 `broadcast` 從「一份共用的 `[]byte`」變成 per-topic payload —
  也就把第 3 節消掉的 per-client marshal 成本又請回來。只有在 payload 大小真的造成
  痛點時才划算；11 個 symbol 的規模並不會。

## 8. 超過單一 process 之後

上面所有內容都是單 process。一旦變成兩個：

- 每個 process 各自持有自己的上游連線 — 可以接受（是個小常數），或者選出一個 publisher，
  透過 Redis pub/sub 推給其他人。
- 對 last-value-wins 來說**不需要 sticky session**：任何 process 都能服務任何 client，
  因為它們最終都收斂到同一個當前值。這點值得主動指出來 — 它正好說明了**資料語意
  比傳輸協定更重要**。
- process 之間的共用匯流排（Redis/NATS）在匯流排邊界上沿用同一套 drop-oldest 策略。

## 9. Cross-cutting concerns（橫切關注點）

明確寫下來，因為面試官評分的是你**說出來**的東西
（對應 feedback-sd-state-explicitly）：

- **Auth**：不需要 — 公開行情、無 per-user 狀態。但下單通道會需要在 handshake 時做
  per-connection auth，而且重連時要重新 auth。
- **Origin/CSRF**：SSE 受 CORS 管制；**WS 不受** — 要在 server 端檢查 `Origin`（§6.3）。
- **TLS**：只用 `wss://` / `https://`。從 `https://` 頁面發出的明文 `ws://` 本來就會被
  當作 mixed content 擋掉。
- **Rate limiting**：限制每個 IP 的連線數，並對總 subscriber 數設上限，
  以免連線洪水把 file descriptor 耗盡。
- **Observability**：subscriber gauge、drop counter（drops/sec 就是 backpressure 訊號 —
  當時**尚未**埋點，見下節）、上游 reconnect counter、broadcast latency。
- **Load shedding**：超過 subscriber 上限就用 `503` + `Retry-After` 拒絕新連線，
  而不是讓所有人一起變慢。
- **Graceful shutdown**：用 normal-closure code 關閉 session，讓 client 立刻重連，
  而不是等到 timeout。

## 10. Observability（2026-09-03 補上）

`hub.stats()` 已呈現在 `/ops` 的「價格推流」區塊，以及 `/ops/services` JSON 的
`stream` 欄位：

| 欄位 | 為什麼需要它 |
|---|---|
| `running` / `subscribers` / `peak_subs` | poller 有沒有在跑、為誰在跑。`peak_subs` 是高水位標記，**刻意不會**隨著即時人數回落 |
| **`dropped` / `drop_pct`** | **backpressure 訊號。** 出現 drop 代表有 client 落後了一個 3s 間隔 |
| `delivered` | `drop_pct` 的分母；以「每 subscriber 每次 broadcast」成長 |
| `refreshes` / `refresh_errs` | 上游健康度，與 client 健康度互相獨立 |
| `last_age_sec` | 陳舊度。`-1` = 還沒 broadcast 過。當有 subscriber 且 age > 3 個間隔時會標警示 |

`reportDrops()` 只在「自上次回報以來有新的 drop」時記錄一筆**差量**，
所以健康的 stream 是安靜的，而被餓到的 stream 會在 journal 裡看得見，
又不會出現 per-drop 的逐筆日誌（3s × N subscriber 那會是洗版）。

正式環境已驗證：subscriber 1 → 3 → 1 追蹤正確，人走光後 `peak_subs` 停在 3，
`refreshes` 每 3s +1，`delivered` 以 subscribers × broadcasts 成長。
drop 路徑改用**精確期望值的單元測試**覆蓋（`TestDropAndDeliverCounters`），
而不是在正式環境示範 — 要真的餓死一個 client 需要約 2 分鐘把 socket buffer 灌滿。

> 💡 注意這裡的取捨：能在正式環境驗證的就驗證，不能安全重現的（drop、下面的 WS DOWN）
> 就用單元測試釘住狀態機。文件把「哪些是實測、哪些是測試覆蓋」分得很清楚。

## 10b. WebSocket 上游 — 2026-09-03 上線

§5–§9 把 WS 設計描述成「沒有實作」。現在**上游那一半已經蓋好了**；
面向 client 的傳輸仍然是 SSE，正如 §3 論證的那樣。

> 💡 這是重點：WS 進來的是**上游**（我們 ← 交易所），不是下游（瀏覽器 ← 我們）。
> §3 的結論沒有被推翻，只是被實作補完。

**協定事實全部是用 `cmd/wsprobe` 從線上實際封包讀出來的，不是抄文件。**
這很重要：既有的 `bingx/ws.go` scaffold 帶著上一次研究留下的註記，
實測確認了其中一條，而如果有錯的那條也會被抓出來。

| 事實 | 細節 |
|---|---|
| framing | **BINARY** frame，內容是 **gzip**。145 bytes 的 frame 裝約 120 bytes 的 JSON — 這種大小壓縮幾乎不划算，但它不是可選的。 |
| keepalive | 應用層的 **TEXT `"Ping"` → 回 `"Pong"`**。**不是**協定層 ping，所以 gorilla 的自動處理器根本看不到它。漏回就會被 server 踢掉。 |
| multiplexing | **一條連線承載 N 個訂閱。** 已用同一個 socket 上的 4 個 dataType 驗證，各自約 1 frame/sec 獨立推送。這就是為什麼 hub 只需要一條上游連線，而不是每個 symbol 一條。 |
| cadence | `@markPrice` 每個 symbol 約 **1 Hz**。這是一個**每秒**的 feed，**不是**逐筆成交的 tick stream。 |
| symbols | 美股合成標的（`NCSKSNDK2USD-USDT`）與加密貨幣對走同一條 stream。 |
| `@ticker` | 提供真正的 24h `h`/`l`，但它的 `o`/`p`/`P` 是**短窗（約 2 分鐘）**的，不是 24h — 所以真正的 24h 漲跌幅仍然要靠 klines。 |

**上游現在被拆成兩路，這正是當初注入 `source` 那條接縫買到的東西：**

```
bingx ws @markPrice ──► markLoop  ──┐  (價格, ~1 Hz, 一條連線, 11 個 symbol)
                                    ├─► emit(): overlay + 只 marshal 一次 ──► broadcast
REST buildTickers ──► statsLoop ────┘  (24h 基準 / funding / range, 每 30s)
                        broadcastLoop 合併到 <= 每 500ms 一次
```

- `statsInterval` 從 **3s 改成 30s**。REST 之前之所以是新鮮度下限，只是因為它是
  **唯一**的來源；它現在還負責的那些欄位，變動尺度都是以分鐘計。實測：32s 內
  `refreshes` 從 3 成長到 4。
- `broadcastInterval` 500ms 做合併。11 symbol × 1 Hz 若不合併，等於每個分頁每秒
  約 11 個 SSE frame，只為了改一個數字。
- **Overlay 是非破壞性的。** 沒有 WS 價格的 symbol 會保留它的 REST 價格，
  所以 feed 部分失效或斷掉時是**逐 symbol 降級**，而不是整條 ticker 條變空白。
  `emit()` 是**複製**而不是就地修改 `statRows`，否則下一次 REST refresh 會拿被 WS
  污染過的值去做 diff。
- 只有**價格真的變了**的 mark 才會設 dirty flag — BingX 在沒有成交時會重複推送同一個價格。
- `RunMarkPrices` 以 **exponential backoff + jitter** 監管（§6.1），每次斷線都記錄，
  而且在一條連線存活超過 1 分鐘後**重置 backoff**，這樣一個糟糕的夜晚不會把延遲
  永遠釘在上限值。
- 送進 mark channel 是 **non-blocking drop** — 跟 §3 同樣的 last-value-wins 授權，
  也帶同樣的警告：**不要把這招複製到成交回報（fills）串流上。**

**新鮮度下限現在是 ~1s，原本是 3s。** 正式環境已驗證：14s 內 15 個 event、
11 個 symbol 全部標記為 `live`、zero drops，只有一次
`ws connecting (11 symbols, attempt 1)`，沒有反覆重連。

`DecodeMarkFrame` 是純函式，協定測試都掛在它身上（18 個案例：gzip 與明文、
四種空白變體的 Ping、訂閱 ack、共用 socket 上的其他頻道、非零 `code`、
零或無法解析的價格，以及美股合成標的）。

## 11. 剩下的缺口

- **沒有 subscriber 上限。** 沒有任何東西會拒絕第 10,000 條連線。§9 的 load shedding
  只是設計，沒有實作。
- **下限現在是交易所 `@markPrice` 的 ~1 Hz 節奏**，不再是我們的輪詢。要再往下就需要
  `@trade`（逐筆）與一套不同的聚合策略；`SubscribeTrades` 還是 stub。
- ~~`/ops` 沒有 websocket 連線健康度的顯示~~ — **已完成**。`/ops` 現在顯示**兩列**，
  因為那是兩種獨立的失效：交易所 websocket 可能已死，而 SSE fan-out 卻完全健康
  （它會繼續送 30s 前的 REST 價格），反之亦然。單一個合併的「stream OK」燈號
  正好會把這種情況藏起來。
  - `RunMarkPrices` 的 hook 從「預先格式化好的字串」改成 **`StreamEvent` struct**。
    第一版回傳的是日誌句子，導致想**呈現**健康度的呼叫端得把英文句子反解析回欄位。
    `bingx` package 擁有**事實**，呼叫端擁有**措辭**。
  - `BackoffFor(attempt)` 匯出且為純函式，所以重試排程是被**斷言**的（每次 attempt 的
    上下界、上限 30s、attempt 40 不溢位），而不是用文字描述 — 其中包含一個測試驗證
    jitter **真的有在變動**，因為不會變的 jitter 等於失去它唯一的作用。
  - **連上 ≠ 正常運作。** `ws_up` 只由「收到第一個 FRAME」設定，絕不由「撥號成功」設定；
    當連線在上但資料陳舊（~1Hz 的 feed 卻超過 10s 沒動）時，卡片會標示
    「連線中但無資料」。`ws_live_syms / ws_total_syms` 則暴露部分失效的 feed —
    有些 symbol 在推，有些悄悄沒推。
  - DOWN 這條路徑無法在活著的機器上重現（那等於真的把 feed 弄壞），所以改用狀態機的
    單元測試覆蓋，而不是正式環境示範。線上實測：
    `ws_up true, 11/11 live, age 0s, connects 1, drops 0`。
- `SubscribeKlines` / `SubscribeDepth` 仍是 stub；目前沒有東西需要它們。
- **僅支援單一 process** — 兩個以上會有什麼改變見 §8。
- 沒有處理 `Last-Event-ID`。在這裡無害（last-value-wins 表示重連只需要當前狀態，
  而 replay-on-connect 已經提供了），但如果要做可續傳的 stream，那就是掛勾點。
