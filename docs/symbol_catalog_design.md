# 標的目錄與交易所介面(Symbol catalog & venue adapters)— 設計稿

狀態:**已審,§11 已決定**(2026-11-24)。可以從 P1 開始實作。

## 1. 要解決什麼

- **現在**:標的寫死在 `market/resolve.go` 的 17 筆對照表,`cmd/web` 透過 `resolveWebSymbol`(handlers.go:3749)約 40 處呼叫它,⌘K 選單裡也手寫了 6 個標的。想看一檔清單外的標的(例如 BingX 上的 TSLA、EURUSD、NASDAQ100),只能改程式。
- **目標**:
  1. 像交易所 / TradingView 一樣,**搜尋並打開任何標的**的圖表、成交量、結構分析,也能問 AI。
  2. 每個標的可以看**各策略在它身上的回測**,判斷有沒有邊。
  3. 留好**接其他交易所 / 市場的口**:台股是第一個,之後要接 Binance、OKX 等,只是多寫一個 adapter。
- **不做**:
  - 不擴大**可交易**的範圍。下單、自動交易、日誌、保護單仍只接受現在逐一調過、過 gate 的清單(維持「只在少數調過的標的上交易」的原則)。
  - 不做全市場掃描(1358 個合約 × 多週期,會撞 rate limit,也不是現在的需求)。

## 2. 現況(2026-11-24 實測)

BingX 公開 API `GET /openApi/swap/v2/quote/contracts`(不需要 key,`bingx/types.go:15` 已有常數):

| 項目 | 數字 |
|---|---|
| 合約總數 | 1358 |
| `status 1`(正常) | 988 |
| `status 25`(看起來是下架/暫停,例如 RACE、MS) | 370,目錄排除 |
| `NCSK…` 美股 | 673 |
| `NCFX…` 外匯 | 42 |
| `NCCO…` 商品 | 27 |
| `NCSI…` 指數 | 13 |
| 其餘 | 加密貨幣 |

每筆有 `symbol`(合約代碼)、`displayName`(人看的名字:`NVDA-USDT`、`GOLD(XAU)-USDT`、`EURUSD-USDT`、`NASDAQ100-USDT`)、`asset`、`pricePrecision`、`quantityPrecision`、`tradeMinUSDT` 等。

附帶發現:`cmd/web/handlers.go:1240` 有一張從 `cmd/contracts` 手抄的精度表。目錄上線後可以改成自動讀,但那是交易路徑,**不在本設計範圍**,另開一項。

## 3. 核心決定:「看」和「交易」分兩層

| | 可看(viewable) | 可交易(tradable) |
|---|---|---|
| 範圍 | 目錄裡任何標的 | 現在的 17 筆清單,不變 |
| 圖表、成交量、結構、AI 問答、回測 | ✅ | ✅ |
| 下單、移動止損、平倉、保護單、自動交易、日誌 | ❌ | ✅ |

**做法:預設安全。**

- `resolveWebSymbol` **保持原樣**,語意改名為「解析可交易標的」。所有沒被刻意改動的呼叫點(下單、止損、平倉、protect、autotrade、journal…)因此自動維持只接受清單內標的。
- 新增 `resolveViewable(s)`:先查清單,查不到再查目錄。**只有**圖表、K 線、結構、AI 工具、回測這些唯讀路徑改用它。
- 加一個測試:列出所有交易路徑的 handler,用目錄裡的非清單標的打一次,全部必須被拒絕。之後有人誤把交易路徑改成 viewable,測試會擋下來。

## 4. 資料模型

```go
// package venue

// ID 是跨市場唯一的標的識別:"<venue>:<code>"。
// 沒有前綴 = bingx,所以現有的 "BTC-USDT"、日誌裡的 "BTC" 全部照舊。
type ID string // "bingx:NCSKTSLA2USD-USDT", "twse:2330"

type Class string // crypto | us-stock | fx | commodity | index | tw-stock

type Instrument struct {
    ID       ID
    Venue    string // "bingx", "twse"
    Code     string // 交易所原始代碼
    Display  string // 顯示用:"TSLA"、"GOLD(XAU)"、"2330 台積電"
    Class    Class
    Quote    string // USDT, TWD
    PricePrecision int
    // Session 描述交易時段;nil = 24/7。台股 09:00-13:30 Asia/Taipei。
    Session *Session
    Tradable bool // 在本系統可交易(= 在清單內),不是「交易所能不能交易」
}
```

- **網址相容**:`/chart?symbol=BTC` 照舊;新標的用 `/chart?symbol=bingx:NCSKTSLA2USD-USDT`,畫面上顯示 `TSLA`。
- **短名稱**:清單外的標的沒有短名稱,`Display` 只用來顯示,不能拿來解析。原因是 `displayName` 在不同類別間可能撞名,例如外匯和加密貨幣都可能出現同名代碼。
- `market.Symbol` 繼續是 BingX 合約代碼字串,這次不改型別,避免 40 處呼叫點和日誌格式一起動。非 BingX 的市場透過 `venue.ID` 進來,只走唯讀路徑。

## 5. 交易所介面(給未來的設計口)

只定義**行情**介面。交易介面這次不抽象,BingX 是唯一會下單的地方;等真的要在第二家交易所下單時,再依那家的實際需求抽(現在抽會抽錯形狀)。

```go
// MarketData 是一個市場的唯讀行情來源。每家交易所 / 每個市場寫一個。
type MarketData interface {
    Venue() string
    // Instruments 回傳可看的全部標的(已排除下架)。呼叫方會快取。
    Instruments(ctx context.Context) ([]Instrument, error)
    // Klines 回傳已收盤 + 正在形成的 K 線,與 bingx.Client.Klines 同語意。
    Klines(ctx context.Context, code string, tf market.Timeframe, limit int) ([]market.Candle, error)
    // Timeframes 是這個市場支援的週期(台股免費資料只有日線)。
    Timeframes() []market.Timeframe
}

// Registry 依 ID 的 venue 前綴分派到對應的 MarketData。
type Registry struct{ /* venue → MarketData, 目錄快取 */ }
func (r *Registry) Lookup(id ID) (Instrument, MarketData, error)
func (r *Registry) Search(q string, limit int) []Instrument
```

**加一家新交易所要做的事**(這就是「設計口」):

1. 寫一個 `venue/<name>` 實作 `MarketData`,含它自己的 rate limit 和錯誤處理。
2. 在 `cmd/web/main.go` 註冊到 Registry。
3. 補它的 `Class` 對應和交易時段。

圖表、結構分析、AI 工具、回測都只認 `MarketData`,不用改。

**第一批 adapter**:

| venue | 來源 | 週期 | 備註 |
|---|---|---|---|
| `bingx` | 現有 `bingx.Client` 包一層 | 現有全部 | P1 |
| `twse` | 證交所公開資料(`twse` 套件已有基本面) | 日線 | P4;上櫃(TPEx)另一個來源;盤中分 K 另案查證 |

## 6. 目錄(catalog)

- **來源**:啟動時呼叫各 venue 的 `Instruments`,之後每 24 小時刷新一次。
- **失敗時**:沿用上一次成功的結果。結果會存一份到磁碟(與日誌同一個資料夾),所以重啟時 BingX 剛好連不上,也有目錄可用。
- **搜尋**:比對 `Display`、`Code`、`asset`,不分大小寫;清單內標的排最前,再依類別排序(加密 → 美股 → 商品 → 指數 → 外匯)。
- **K 線快取**:非清單標的的 K 線放 LRU,上限約 50 個標的,避免有人連續翻 100 檔把記憶體或 BingX rate limit 吃滿。

## 7. 介面

- **選單**:擴充現有 ⌘K 指令面板(partials.html:164),不另做一個。輸入文字時,呼叫 `/api/instruments?q=` 取得搜尋結果,每筆顯示名稱、類別、交易所,以及是否可交易的標記。
- **/chart 標的列**:沿用現在的列,前面多一個「+」。
  - 預設顯示清單內標的。
  - 加上「自選清單」:你從選單加進來的標的。
- **自選清單存哪裡**(§11 決定):
  - 瀏覽器 localStorage:最簡單,但換裝置就沒了。
  - 伺服器上的一個 JSON 檔:手機和電腦同步,AI 和 monitor 也讀得到。我建議這個。
- **不可交易的標的**:「記錄模擬單」「保護單」這類按鈕不顯示;標題旁標「僅分析」。

## 8. 分析:各標的各自的策略

- **可以直接用的**(不需要調參數):K 線、成交量、結構分析(高低點、BOS/CHoCH、樞紐區)、開盤價、EQH/EQL、Volume Profile。
- **要誠實標示的**:引擎分數。清單外的標的,`signal.StrategyFor`(strategy_momentum.go:201)會預設套 MR,算出來的分數從沒在那個標的上驗證過。顯示時要標「未校準:預設 MR,未經此標的回測」,不能和清單內標的的分數看起來一樣。
- **「策略適配」面板**(P3):選一個標的按一次,用現有回測工具跑各策略 60/90/120 天,每個策略附 `shipgate` 判定(PASS / FAIL / UNDECIDED)和交易筆數。
  - 是使用者按了才跑,不會背景自動跑。
  - 結果快取到下一根日線收盤。
  - 顯示的是「這檔上哪個策略有邊、樣本多少」。**不會**因此自動把標的加進可交易清單;要加,仍走原本的 gate 流程。

## 9. AI copilot

- `analyze_symbol` 等唯讀工具改用 `resolveViewable`,所以可以問「分析 TSLA 1h」。
- 工具回傳要帶 `Tradable` 和「引擎未校準」的標記,系統提示也要求 AI 對清單外標的不給進場建議,只描述結構。
- `needsData`(cmd/web/copilot.go)現在用 `uiSymbols` 判斷問題有沒有提到標的,改成也查目錄。
- 這也是 copilot P2「自動帶入頁面資訊」的前置:AI 需要知道你在 /chart 選的是哪一檔。

## 10. gRPC 能派上用場嗎

**現況**:`proto/marketdata/v1` 已有 `MarketData` 服務(`GetMarkPrice`、`StreamMarkPrices`),`grpcsrv` 實作了它,但正式環境沒有任何使用者,gRPC 是關著的(見 grpc.md)。

**這次的設計怎麼跟 gRPC 接**:§5 的 `MarketData` 介面刻意只用純資料型別(ID、Instrument、Candle、context),沒有 BingX 專屬型別外漏。這代表它可以一對一對應成 proto:

```proto
service MarketData {
  rpc GetMarkPrice(...)        returns (...);           // 既有
  rpc StreamMarkPrices(...)    returns (stream ...);    // 既有
  rpc ListInstruments(ListInstrumentsRequest) returns (ListInstrumentsResponse);   // 新
  rpc GetKlines(GetKlinesRequest)             returns (GetKlinesResponse);         // 新
}
```

之後如果行情 adapter 搬到獨立的 process(一個「行情閘道」),只要寫一個 gRPC client 實作同一個 Go 介面,web、monitor、回測工具都不用改。

**現在要不要真的走 gRPC:建議不要**。全部程式跑在同一台 VPS、同一個 process 裡,多一層網路呼叫沒有好處。gRPC 真正划算的時機是:

1. **多個 process 要共用行情**:web 和 monitor 現在各自打 BingX,同一檔的 K 線各抓一次,一起吃 rate limit。接的交易所變多後,一個共用的行情閘道(統一快取、統一限速)就有實際價值,gRPC 是它對外的合約。
2. **adapter 要用別的語言寫**:例如某家券商只有 Python SDK(台股常見),可以包成一個 Python 的 gRPC 服務,Go 這邊照樣只認介面。

所以這次:**介面照 gRPC 能對應的形狀設計,proto 先寫進設計、不實作**。等上面任一個時機出現,再做。(面試上這也是個好故事:先定合約、延後部署邊界。)

## 11. 決定(2026-11-24,使用者)

1. **自選清單存伺服器上的 JSON 檔**(與日誌同一個資料夾),手機和電腦同步,AI 與 monitor 也讀得到。
2. **目錄全開**:BingX 988 個正常合約全部可看,外匯也開。
3. **清單外標的的引擎分數照常顯示,標「未校準:預設 MR,未經此標的回測」**。
4. **「策略適配」回測要做**,做成互動介面:使用者自己決定要不要按,按了才跑,UI 顯示進度與結果(各策略 60/90/120 天 + shipgate 判定 + 筆數)。不自動跑、不自動把標的加進可交易清單。

## 12. 分階段

| 階段 | 內容 | 驗收 |
|---|---|---|
| **P1** | `venue` 套件 + BingX adapter + 目錄(含磁碟備份)+ `resolveViewable`;/chart 能開任何 BingX 標的;交易路徑拒絕清單外標的的測試 | 打開 TSLA 1h 有 K 線和結構;所有交易 handler 對 TSLA 回錯 |
| **P2** | ⌘K 搜尋選單 + 自選清單 + 標的列;非可交易標的隱藏交易按鈕、引擎標未校準 | 手機和電腦都能加/移除自選 |
| **P3** | 「策略適配」面板(各策略 60/90/120 天 + gate) | 對清單內一檔跑出的數字與現有回測工具一致 |
| **P4** | `twse` adapter(日線)+ 台股圖表 + 側欄基本面 | 打開 `twse:2330` 有日 K、成交量、結構、/tw 的基本面數字 |
| **P5** | copilot 工具支援任意可看標的 + 頁面資訊帶入 | 在 TSLA 圖上問 AI,它知道是 TSLA,且不給進場建議 |
