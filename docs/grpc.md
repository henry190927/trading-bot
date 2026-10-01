# gRPC MarketData — 設計與操作說明

> 技術名詞（gRPC / proto / stream / reflection / backpressure / GracefulStop 等）保留原文。
> 指令、路徑、程式碼識別字、log 輸出一律原樣英文。

這個服務刻意只有兩個 RPC：

| RPC | 型態 | 用途 |
|---|---|---|
| `GetMarkPrice(symbol) -> price` | unary | 單一 symbol 的最新 mark price |
| `StreamMarkPrices(symbols) -> stream price` | server-streaming | 價格變動時持續推送 |

兩者都讀**同一份**資料來源：`cmd/web/stream.go` 裡那個已經在跑的 `tickerHub`。
gRPC 在這裡是**第二種 transport，不是第二條資料路徑**。

---

## 1. 檔案位置

```
proto/marketdata/v1/marketdata.proto        # 介面定義（唯一的真實來源）
proto/marketdata/v1/marketdata.pb.go        # 產生檔，已 commit
proto/marketdata/v1/marketdata_grpc.pb.go   # 產生檔，已 commit
grpcsrv/server.go                           # service 實作 + Serve()
grpcsrv/server_test.go                      # bufconn 端對端測試（兩個 RPC 都測）
cmd/web/grpc.go                             # 接線：hub adapter + GRPC_BIND + SIGTERM
```

---

## 2. 重新產生程式碼

產生檔是 **commit 進 repo 的**，所以 `go build` / CI / 部署都不需要 `protoc`。
只有改動 `.proto` 當天才需要下面這些。

一次性安裝工具：

```bash
brew install protobuf grpcurl    # protoc 本體 + 手動呼叫用的 client
make proto-tools                 # protoc-gen-go v1.36.11 + protoc-gen-go-grpc v1.5.1
```

改完 `.proto` 之後：

```bash
make proto
```

輸出：

```
✅ regenerated proto/marketdata/v1/*.pb.go
```

`make proto` 會先檢查 `protoc` 和兩個 plugin 在不在，不在就直接告訴你要跑哪個指令，
不會丟一個看不懂的 exec 錯誤。plugin 版本在 Makefile 裡是**寫死的**（pin 住），
因為 `protoc-gen-go` 的版本會寫進產生檔的 header；不 pin 的話，兩個人各自跑
`make proto` 就會產生無意義的 diff。

> **注意**：`protoc-gen-go` 的版本必須和 `go.mod` 裡的 `google.golang.org/protobuf`
> 對得上。這次 `go get google.golang.org/grpc@v1.80.0` 把 protobuf 從 v1.36.10
> 升到 v1.36.11，所以 plugin 也跟著重裝成 v1.36.11 並重新產生一次。

---

## 3. 啟動 server

gRPC 是 **opt-in** 的。`GRPC_BIND` 沒設 → 不開 listener、不裝 signal handler、
行為跟以前完全一樣。

```bash
make grpc                      # 等同 GRPC_BIND=127.0.0.1:9090 go run ./cmd/web
make grpc 0.0.0.0:9090         # 指定 bind
GRPC_BIND=127.0.0.1:9090 go run ./cmd/web
```

啟動成功的 log：

```
2026/10/01 11:08:33 trading-bot-web listening on 127.0.0.1:18080 (Asia/Taipei)
2026/10/01 11:08:33 grpc: MarketData listening on 127.0.0.1:19090 (reflection on)
```

port 被佔用時**不會**把 web 拖下水：

```
grpc: NOT started — listen 127.0.0.1:9090: ... (HTTP unaffected)
```

---

## 4. 用 grpcurl 呼叫

reflection 是開的，所以不需要把 `.proto` 檔帶到那台機器上。

```bash
make grpc-probe                # 一次跑完下面三件事
```

或手動：

```bash
grpcurl -plaintext 127.0.0.1:9090 list
grpcurl -plaintext -d '{"symbol":"BTC"}' \
  127.0.0.1:9090 tradingbot.marketdata.v1.MarketData/GetMarkPrice
grpcurl -plaintext -d '{"symbols":["BTC","ETH"]}' \
  127.0.0.1:9090 tradingbot.marketdata.v1.MarketData/StreamMarkPrices
```

實際輸出（2026-10-01 本機實跑）：

```
$ grpcurl -plaintext -d '{"symbol":"BTC"}' 127.0.0.1:19090 tradingbot.marketdata.v1.MarketData/GetMarkPrice
{
  "price": {
    "symbol": "BTC",
    "price": 83436.3,
    "asOf": "2026-10-01T03:08:46.836753Z"
  }
}

$ grpcurl -plaintext -d '{"symbol":"doge"}' 127.0.0.1:19090 tradingbot.marketdata.v1.MarketData/GetMarkPrice
ERROR:
  Code: NotFound
  Message: unknown symbol "DOGE"

$ grpcurl -plaintext -d '{"symbols":["BTC","ETH"]}' 127.0.0.1:19090 tradingbot.marketdata.v1.MarketData/StreamMarkPrices
{
  "symbol": "BTC",
  "price": 83436.3,
  "asOf": "2026-10-01T03:08:47.238290Z"
}
{
  "symbol": "ETH",
  "price": 2685.6,
  "asOf": "2026-10-01T03:08:47.238290Z"
}
{
  "symbol": "BTC",
  "price": 83436.3,
  "live": true,
  "asOf": "2026-10-01T03:08:48.242205Z"
}
```

注意前兩筆**沒有** `live: true`，第三筆開始才有。這不是 bug：hub 的 REST 快照先到，
mark price websocket 的第一個 frame 稍後才到。`live` 是**逐 symbol** 的事實，
不是整條連線的狀態 —— 某個 symbol 的 ws 還沒來，它就繼續用 30s 新鮮度的 REST 價，
而不是整排消失。這一點是直接沿用 hub 原本的 overlay 設計。

> proto3 的預設值不會出現在 JSON 輸出裡，所以 `live: false` 會整個欄位消失。
> 這是 protobuf 的行為，不是漏送。

---

## 5. 設計決策

### 5.1 為什麼 gRPC server 放在 `cmd/web` 裡，而不是獨立的 `cmd/grpc`

這是整個工作裡唯一真正需要選邊的地方。

`tickerHub` 擁有**唯一一條**連到交易所的 mark price websocket，而且它是**per-process**
擁有的。一個獨立的 `cmd/grpc` 行程沒有辦法 attach 到 web 行程裡的那個 hub，
它只能自己再建一個 —— 第二條 websocket、第二組 REST poller，
然後兩邊對「BTC 現在多少錢」可能給出不同答案。

也就是說，「重用既有 hub」和「獨立行程」在這裡是**互斥的**，而重用的價值明顯比較高：

- 上游成本不隨 transport 數量增加
- SSE 的瀏覽器分頁和 gRPC client 看到的數字**不可能**不一致
- hub 既有的 conflation（500ms）、drop-oldest backpressure、
  refcount 生命週期（第一個 subscriber 啟動、最後一個離開時關閉）全部免費繼承

代價是 gRPC 和 web 行程同生共死。這個代價用兩件事限制住：介面是**唯讀**的，
而且整個 listener 是 **opt-in**（`GRPC_BIND` 沒設就什麼都不會發生）。
`main.go` 裡的 `PPROF_BIND` 早就做過同樣的取捨，所以這也符合這個 repo 既有的習慣。

### 5.2 怎麼重用 hub：`PriceSource` 這個 seam

`tickerHub` 在 `package main`（`cmd/web`），**無法被 import**。所以 `grpcsrv`
不依賴它，只依賴一個介面：

```go
type PriceSource interface {
	Subscribe() (<-chan []byte, []byte, func())
}
```

這剛好就是 `tickerHub.subscribe()` 的簽章。`cmd/web/grpc.go` 補一個一行的 wrapper：

```go
func (h *tickerHub) Subscribe() (<-chan []byte, []byte, func()) { return h.subscribe() }

var _ grpcsrv.PriceSource = (*tickerHub)(nil)
```

為什麼是 wrapper 而不是把 `subscribe` 改名成 `Subscribe`：Go 的介面**只能**由
exported method 滿足，但改名會動到 `stream.go` 和它的測試，而那個改動沒有任何
行為上的理由。留著 wrapper，SSE 那條路徑在 diff 裡就是**一個字都沒改**。
那行 `var _` 是編譯期的證明：真的 hub（不只是測試用的 fake）確實合得上這個 seam。

**payload 為什麼是 JSON `[]byte`。** hub 刻意只 marshal 一次，然後把同一份 bytes
fan-out 給所有 subscriber。如果改成讓它交出 typed struct，那次 marshal 就會被推到
每個 SSE client 身上 —— 那是在為了 gRPC 的方便而讓既有的 hot path 變慢。
所以 `grpcsrv` 這邊自己 decode。11 個 symbol、每秒最多 2 份快照，這個成本是雜訊。

**unary 也走 `Subscribe()`。** `GetMarkPrice` 沒有去讀什麼 cache global，
它跟 stream 走完全同一條路。一條路徑的意思是：unary 不可能讀到一個 stream 不會送出的值。
副作用是 unary 呼叫也會觸發 hub 的 lazy start（確實在 log 裡看得到，
`stats loop started` 然後 3 秒後 `stopped`），這是對的 —— refcount 本來就該這樣動。

### 5.3 `GracefulStop` vs `Stop`

**選 `GracefulStop`，但前面必須先加一個 `Drain()`。**

單靠 `GracefulStop` 在這裡是**會壞掉的**。`GracefulStop` 的語意是「停止接受新連線，
然後等 in-flight 的 RPC 自己結束」。但 `StreamMarkPrices` 只在**client 離開時**才結束 ——
一個健康的、行為完全正確的 client 會讓 `GracefulStop` 等到天荒地老。

所以關機是兩段式的（`grpcsrv.Serve`）：

1. `svc.Drain()` — 關掉一個 channel，所有開著的 stream handler 從 `select` 醒來，
   回傳 `UNAVAILABLE`。`UNAVAILABLE` 是 gRPC client 預設會 retry 的 code，
   語意剛好對：「這台現在不行，等一下再來」。
2. `gs.GracefulStop()` — 把已經在飛的 unary call 做完、送出 GOAWAY、關閉。

用 `Stop()` 取代的話，會在 frame 中間直接砍斷每一條連線，client 端看到的是一個
沒有理由的 transport error。對一個 client 普遍長壽的行情服務來說，這個差別就是全部的重點。

即使如此，`GracefulStop` 仍然**有上限**：卡在 flow control 的 client 可以讓一個
in-flight `Send` 永遠完不成。15 秒（`shutdownGrace`）之後改用 `Stop()` 硬關。
理由很實際：會 hang 的關機，systemd 最後還是會用 SIGKILL 收掉，
那還不如自己決定什麼時候放棄。

實跑驗證（SIGTERM，當下有一個 grpcurl stream 開著）：

```
2026/10/01 11:09:02 grpc: draining streams, then GracefulStop (grace 15s)
2026/10/01 11:09:02 stream: stats loop stopped (no subscribers)
2026/10/01 11:09:02 stream: markprice ws stopped after 3s (no subscribers)
2026/10/01 11:09:02 grpc: stopped gracefully
2026/10/01 11:09:02 grpc: shutdown complete — exiting
```

client 端：

```
ERROR:
  Code: Unavailable
  Message: server is shutting down
```

中間那兩行 `no subscribers` 很重要：它證明 gRPC stream 真的是 hub 的 subscriber，
而且離開時把 refcount 還回去了 —— 所以 hub 關掉了上游 websocket。

### 5.4 SIGTERM 的接管，以及為什麼 HTTP 端**沒有**一起做 graceful

`cmd/web` 以前完全沒有 signal handler，SIGTERM 就是直接殺掉行程。
`signal.NotifyContext` 會**取消掉**那個預設行為，所以 `startGRPC` 裡每一條離開的路徑
都必須真的讓行程結束，否則 `systemctl restart trading-web` 會卡到 systemd 的
stop timeout 用完再 SIGKILL —— 那是一個貨真價實的 regression。

所以：

- 正常關機路徑：drain → GracefulStop → `os.Exit(0)`。
- `Serve` 自己死掉（不是因為 signal）：log 一行，`stop()` 把 SIGTERM 還給預設 handler，
  web 繼續跑。少了 gRPC 不是把 dashboard 一起關掉的理由。

**HTTP / SSE 端刻意沒有改成 graceful**，有兩個理由：

1. 任務範圍明確說不要改既有 endpoint 的行為。`r.Run(bind)` 一個字都沒動。
2. 就算要改也沒那麼單純：SSE stream **永遠不會自己結束**，所以
   `http.Server.Shutdown` 必然會等到 timeout。真要做就得再設計一次，
   而那是另一件事，不該夾帶在這個 commit 裡。

淨效果：SIGTERM 之後，HTTP 端的體驗和**今天完全一樣**（行程直接消失），
gRPC 端則多了一次有秩序的收尾。

### 5.5 stream client 斷線時會發生什麼

handler 收到的**唯一**斷線訊號是 `stream.Context()` 被 cancel。
gRPC 對三種情況都做同一件事：client 主動 cancel、client deadline 到期、TCP 斷掉。
沒有「client 走了」的 callback。

```go
case <-ctx.Done():
	return status.FromContextError(ctx.Err()).Err()
```

`return` 會跑到 `defer release()`，refcount 減一。如果它剛好是最後一個 subscriber，
hub 就 cancel 自己的 loop、關掉上游 websocket。
`TestStreamReleasesTheHubWhenTheClientGoesAway` 測的就是這件事 ——
漏掉 `release` 的話，上游 websocket 會永遠開著，而且從 `/ops` 上看起來一切正常。

`FromContextError` 而不是直接 `ctx.Err()`：前者會把 `Canceled` 對到 `CANCELLED`、
`DeadlineExceeded` 對到 `DEADLINE_EXCEEDED`，直接丟 `ctx.Err()` 會被攤平成 `UNKNOWN`。

### 5.6 context 怎麼往下傳

- **unary**：client 的 deadline 從 metadata 進來，變成 handler 的 `ctx`。
  client 沒設 deadline 時，server 自己補一個 10 秒上限（`unaryWait`）。
  理由是冷啟動的 hub 要等第一份 REST 快照才有東西可回，
  沒有上限的話，generated client 的預設是永遠等下去。
- **streaming**：`stream.Context()` 在 RPC 的整個生命週期都活著，
  被 cancel 就是上面 5.5 的那件事。
- **往上游的方向沒有傳。** hub 的生命週期是 refcount 管的，不是某一個 caller 的 ctx 管的 ——
  本來就該如此：一個 client 的 cancel 不可以把其他人的價格來源關掉。
  gRPC 的 ctx 只控制「這一個 RPC」。

### 5.7 backpressure

gRPC client 不讀的時候，`stream.Send` 會卡在 HTTP/2 flow control。這件事是**被圍住的**：

1. 卡住的是這個 handler 的 goroutine，
2. 它那個 hub channel 會塞滿，
3. hub 對**這一個** subscriber 丟掉過期的 pending 快照（drop-oldest），別人不受影響。

也就是說，一個慢的 gRPC client 拖不慢瀏覽器分頁。這是 hub 原本就寫好的性質，
這裡只是沒有把它破壞掉。

另外做了 **per-symbol 去重**：hub 只要有**任何**一個 symbol 動了就廣播整份快照，
不去重的話，BTC 跳一下會順便把另外十個沒變的 row 也推給 client。
跟 drop-oldest 可以成立的理由一樣 —— 這條 feed 是 last-value-wins。

### 5.8 Keepalive

server 每 30 秒（閒置時）ping 一次，10 秒沒回應就斷。
沒有 keepalive 的話，一個沒送 FIN 就消失的 peer（闔上筆電、NAT timeout、VPN 掉線）
會一直佔著 hub 的 subscription，直到作業系統自己放棄那個 socket。
`EnforcementPolicy.MinTime = 10s` 則是反方向：讓 ping 的預算由 server 決定，
而不是由 client 決定。

### 5.9 reflection：**開**

- 這是唯讀的公開行情資料，schema 不是秘密。
- bind 位址由 `GRPC_BIND` 決定，實務上是 loopback 或 tailnet，跟 web UI 一樣。
- 沒有 reflection 的話，要 debug 就得先把 `.proto` 檔弄到那台機器上。
  凌晨兩點在 VPS 上，這就是「能不能查」的差別。

如果哪天這個 port 直接面向公開網路，這個決定要翻過來，
理由會是：reflection endpoint 等於免費把完整的 method 清單交給攻擊者。

### 5.10 錯誤碼

| 情況 | code |
|---|---|
| `symbol` 空字串 | `INVALID_ARGUMENT` |
| symbol 不在 hub 的 universe 裡 | `NOT_FOUND` |
| symbol 存在但那一列沒有價格（上游掛了） | `UNAVAILABLE` |
| hub 在 deadline 之前還沒有任何快照 | `DEADLINE_EXCEEDED` |
| server 正在關機 | `UNAVAILABLE` |

`NOT_FOUND` 和 `UNAVAILABLE` 分開是刻意的：caller 必須能分辨「我打錯字」和「上游掛了」。

stream 這邊，要求了不存在的 symbol 會**立刻**回 `NOT_FOUND`，而不是開著一條永遠安靜的
stream。一條永遠安靜的 stream 和一條死掉的 feed，在 client 看起來一模一樣。

---

## 6. 測試

```bash
go test -race ./grpcsrv/
```

測試用 `bufconn`（記憶體內的 listener）跑**真的** `grpc.Server`：
真的 codec、真的 stream、真的 status 傳遞，但不綁 port、不跟別的測試搶、CI 可跑。
hub 用一個 fake 取代，它複製了 hub 契約裡會影響這個服務的三件事：
1-slot drop-oldest buffer、replay 給新 subscriber 的 `last`、只能呼叫一次的 release。

涵蓋的項目：

- `TestGetMarkPriceServesTheHubSnapshot` — unary 正常路徑 + 不漏 subscription
- `TestGetMarkPriceWaitsForTheFirstSnapshot` — 冷啟動的 hub
- `TestGetMarkPriceErrorCodes` — 三個錯誤碼
- `TestGetMarkPriceDeadlineOnASilentHub` — deadline 傳遞
- `TestStreamMarkPricesFiltersAndDeduplicates` — symbol 過濾 + per-symbol 去重
- `TestStreamMarkPricesEmptyRequestMeansEverySymbol` — 空請求的語意
- `TestStreamMarkPricesRejectsAnUnknownSymbol` — 打錯字會大聲失敗
- `TestStreamReleasesTheHubWhenTheClientGoesAway` — refcount 有還回去
- `TestDrainEndsAnOpenStream` — 關機能結束 stream，而且是 `UNAVAILABLE` 不是 EOF
- `TestDrainIsIdempotent` — SIGTERM 之後再來一個 SIGINT 不會 panic

---

## 7. 假設（規格沒講、由我決定的部分）

這一節全部是**我自己做的決定**，不是規格要求的：

1. **`StreamMarkPricesRequest.symbols` 留空 = 全部 symbol。** 另一個選項是回
   `INVALID_ARGUMENT`。選「全部」是因為它對應 SSE 那條路的行為（整條 ticker strip）。
2. **symbol 不分大小寫**，server 端一律轉大寫。`grpcurl -d '{"symbol":"btc"}'` 可以動。
3. **symbol 用 UI 的短代號**（`BTC`、`ETH`、`XAU`），不是交易所代號（`BTC-USDT`）。
   因為 hub 的快照就是用短代號當 key 的。
4. **`as_of` 是 server 時間，不是交易所時間。** hub 的快照裡沒有交易所的 timestamp，
   所以這個欄位的意思是「這個值是什麼時候被 fan-out 的」。
   想做真正的交易所時間，要從 `bingx.MarkPrice` 一路帶過來，那是另一個改動。
5. **unary 沒有 deadline 時補 10 秒上限。**
6. **`GracefulStop` 的上限是 15 秒。**
7. **沒有 TLS、沒有 auth。** 跟 web UI 的姿態一致：bind 在 loopback / tailnet，
   由網路層負責。真要開到公開網路，`NewServer` 要加 `grpc.Creds(...)`
   和一個 auth interceptor，而且 reflection 要關掉（見 5.9）。
8. **`GRPC_BIND` 預設關閉。** 要在 VPS 上跑，systemd unit 需要加
   `Environment=GRPC_BIND=...`。**這次沒有改 systemd unit。**
9. **`.golangci.yml` 沒有動。** golangci-lint v2 預設的
   `exclusions.generated: lax` 已經靠 `// Code generated ... DO NOT EDIT.` header
   認出產生檔，`golangci-lint run ./proto/...` 實測 `0 issues`。
   既然沒有 linter 被踩到，就不加一條用不到的 path exclusion。

---

## 8. 已知限制

- **每條 gRPC stream 各自 decode 一次 hub 的快照 JSON。** 在 11 個 symbol、
  每秒 2 份快照的規模下這是雜訊；如果 gRPC subscriber 真的變多，
  正確的做法是在 hub 和所有 subscriber 之間做一次共用的 decode，而不是現在就先寫。
- **`live: false` 在 JSON 輸出裡會整個欄位消失**（proto3 預設值不序列化）。
  Go client 讀 `GetLive()` 不受影響。
- **gRPC 和 web 行程同生共死**（見 5.1，這是重用 hub 的代價）。
- **HTTP/SSE 端沒有 graceful shutdown**（見 5.4，刻意沒做）。
