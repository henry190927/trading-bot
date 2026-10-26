# AI Copilot —— 把分散的 AI 分析收斂成一個聊天框

> 狀態:**設計稿,暫停**(2026-10-27)—— 使用者決定等之後整體討論 AI 時再一起定(§9、§11 的未決題)。不寫程式。
> 沿用的限制(使用者先前定的):① API key 由使用者提供,不寫死 ② 所有 agent 用 Quant 角色
> ③ 用多代理編排省主模型 token。

---

## 0. 現況:五個互不相通的 AI 入口

| # | 入口 | 做什麼 | 問題 |
|---|---|---|---|
| 1 | /journal「AI 分析」 | `/ai/analyze/:id` 檢討一筆交易 | 一次問答,不能追問 |
| 2 | /scan、/chart「AI 分析」 | `/ai/analyze/symbol/:short/:tf` | 同上;context 是寫死組好的 |
| 3 | /validate 結果頁 | `/ai/analyze/validate` 評估進場計畫 | 同上 |
| 4 | `cmd/mcp` | 6 個唯讀工具(get_trade、list_trades、positions、candles、macro、backtest 表)給 Claude Desktop | 工具只存在 MCP 裡,web 用不到 |
| 5 | `ai/fanout.go` | scatter-gather 骨架 | 沒接上 |

`ai.Provider`(Anthropic / Gemini)只有一次性的 `Send`,**不支援 tool calling**。

**目標:** 一個到處都叫得出來的聊天框。它自己決定要查什麼(工具),能被追問,
要改東西時先給人看、按確認才動。現在的三個「AI 分析」按鈕變成「帶著這頁的上下文開聊天框」。

---

## 1. 名詞對照:每一塊在 trading-bot 裡是什麼

| 專業名稱 | 在這裡是什麼 |
|---|---|
| **Tool Use / Function Calling** | 一份共用的工具清單(§3),MCP server 和 copilot 用【同一份】 |
| **Structured Output** | ① 工具參數由 Go 依 JSON Schema 驗證 ② 「建議的動作」必須是固定格式(§4)才會變成確認卡 |
| **Grounding(廣義 RAG)** | 不上網搜尋;grounding 來源 = 交易所即時資料、journal、回測表、引擎訊號 —— 全部經由工具取得,AI 不能憑空報價 |
| **Human-in-the-Loop** | 會寫入的工具(下單、移停損、記錄、平倉)AI 只能【提議】,畫面出確認卡,人按下才走既有的受保護端點 |
| **Agent Loop / ReAct** | 想 → 呼叫工具 → 看結果 → 再想,有步數、token、時間上限(§5) |
| **BFF / LLM Gateway** | cmd/web:代管 key、轉發請求、記錄每次工具呼叫;瀏覽器永遠不直接打 LLM |
| **Streaming(SSE)** | 回覆逐字出現;web 已有 SSE hub(stream.go)可沿用模式 |
| **Conversational UI** | 每頁右側可展開的聊天抽屜;自動帶入「這頁在看什麼」當上下文 |
| **Multi-agent(fan-out)** | 「深度分析」是一個工具,內部把各面向派給便宜模型平行跑,再由主模型彙整(ai/fanout.go) |
| ❌ Generative UI | **刻意不做**:畫面元件固定(確認卡、K 線連結),AI 只產生文字與資料 |

---

## 2. 架構

```
瀏覽器(聊天抽屜)
   │  POST /api/copilot/chat {conversation, page_context}      ← 回 SSE 串流
   ▼
cmd/web  ── BFF / LLM Gateway ──────────────────────────────────────────┐
   │  ① 組 system prompt(Quant 角色)+ page_context                    │
   │  ② Agent loop(§5)                                                 │
   │       ├─ LLM 回覆文字 ──────────────────────────► 串流給瀏覽器      │
   │       ├─ LLM 要求呼叫【讀取】工具 → 直接執行 → 結果回給 LLM          │
   │       └─ LLM 要求呼叫【寫入】工具 → 不執行,產生「提議」──► 確認卡   │
   │  ③ 每次工具呼叫寫進 audit log                                      │
   └───────────────────────────────────────────────────────────────────────┘
           │ 讀取工具                          │ 確認後的寫入(人按下)
           ▼                                   ▼
   tools 套件(共用)                    既有的受保護端點
   journal / 交易所 / K 線 / 總經 /       /api/trade/:id/move-stop(先預覽)
   回測表 / 引擎訊號 / validator          /journal/:id/close(版本檢查)…
           ▲
           └── cmd/mcp 也改用同一份 tools(不再自己實作)
```

---

## 3. 工具清單(單一來源)

把 cmd/mcp 裡的 6 個工具抽成 `tools` 套件,MCP 和 copilot 都從這裡註冊。

**讀取(自動執行):**

| 工具 | 來源 |
|---|---|
| get_trade / list_trades | 既有 MCP |
| get_positions | 既有 MCP(交易所即時) |
| get_candles | 既有 MCP(摘要,不是原始 K 線) |
| get_macro_events | 既有 MCP |
| get_backtest_table | 既有 MCP |
| scan_symbol | 新:引擎訊號 + 結構(/scan 的單一標的版) |
| validate_setup | 新:validator 對一個 entry/stop/side 打分 |
| journal_stats | 新:勝率、R、分組統計 |
| get_live_stop | 新:交易所上實際的停損(/today 卡片那個) |
| deep_analysis | 新:fan-out 多面向分析(§6) |

**寫入(只能提議,HITL):**

| 提議 | 確認後走的路 |
|---|---|
| propose_move_stop | `POST /api/trade/:id/move-stop`(本來就是預覽 → confirm 兩段) |
| propose_record_trade | /chart 快速記錄(可選擇是否下單,仍有 window.confirm) |
| propose_close_trade | 帶入 /journal/:id/close(版本檢查照舊) |

寫入提議【不新增任何執行路徑】:確認卡只是把參數帶進既有、已經有風控和防呆的端點。

---

## 4. Structured Output:提議的格式

```json
{
  "action": "move_stop",
  "trade_id": 93,
  "params": {"stop": 0.9150},
  "reason": "結構低點上移到 0.9162,停損移到其下",
  "risk_note": "打到時整筆 +0.12R"
}
```

Go 端驗證:action 在白名單、參數型別與範圍正確(停損在現價的正確那一側等)。
**驗證不過 = 不產生確認卡**,錯誤訊息回給 LLM 讓它修正 —— 不是丟給使用者。

---

## 5. Agent loop

```
messages = [system(Quant), page_context, 對話紀錄]
for step := 0; step < MaxSteps; step++ {          // 例如 6
    resp := provider.SendWithTools(ctx, messages, tools)   // ctx 有總時限
    串流 resp 的文字
    if 沒有工具呼叫 { 結束 }
    for 每個工具呼叫 {                               // 讀取工具可平行
        讀取 → 執行(各自 timeout)→ 結果附加到 messages
        寫入 → 驗證 → 產生確認卡,結果「已提議,等使用者確認」附加到 messages
    }
}
超過步數 / token 預算 / 時限 → 誠實回報「沒完成」,不假裝有答案
```

需要的改動:`ai.Provider` 加上 tool calling(Anthropic tool_use / Gemini functionCall,兩者格式不同,
由各自的 client 轉換成同一個內部型別)。

---

## 6. 多代理:deep_analysis 工具

沿用 `ai/fanout.go` 的 scatter-gather:結構、盤勢、執行、回測四個面向,各自只拿自己的 context,
用便宜模型平行跑,主模型只讀四份結構化摘要再彙整 —— 主模型 token 省在這裡。
平常的對話不觸發;使用者問「完整分析」或 LLM 判斷需要時才呼叫。

---

## 7. 安全與成本

- **Prompt injection:** journal 的備註是使用者寫的文字,工具結果一律當資料、不當指令;寫入只能經確認卡。
- **Key:** 使用者自備(限制 ①)。存法見 §9 決定。
- **成本:** 每次對話記 token 數;每日上限;fan-out 只在需要時用。
- **Audit log:** 每次工具呼叫(誰、什麼參數、結果大小、耗時)寫一行 jsonl。

---

## 8. 分期

| 期 | 內容 | 價值 |
|---|---|---|
| P1 | tools 套件(MCP 改用)+ Provider tool calling + agent loop + 最簡聊天抽屜(不串流) | 能追問、AI 自己查資料 |
| P2 | SSE 串流 + 每頁自動帶上下文;三個「AI 分析」按鈕改成開聊天框 | 收斂入口 |
| P3 | 寫入提議 + 確認卡(HITL) | 從「建議」到「一鍵執行」 |
| P4 | deep_analysis(fan-out) | 省 token、深度 |

---

## 9. 待你決定

- [ ] **Key 放哪?** (a) 伺服器的 .env(現在的做法,單人用最簡單)(b) 使用者在設定頁輸入、伺服器加密保存(多使用者時需要)(c) 只存在瀏覽器、每次請求帶上、伺服器不保存
- [x] **P1 先接哪個供應商的 tool calling?** → **Gemini**(2026-10-26 決定)
- [ ] **寫入提議第一批要哪幾個?** 建議只先做 propose_move_stop(已有預覽 → 確認流程)
- [ ] **哪些由你寫?** 候選:agent loop(§5,迴圈 + 上限 + 平行讀取工具,有併發可練)、提議的驗證(§4)

---

## 10. 為什麼 BFF 不放 Cloudflare Worker(2026-10-26)

使用者的另一個專案(trip-planner)把 BFF 放在 Cloudflare Worker。那裡合適,這裡不合適 ——
**BFF 放哪,取決於工具和資料在哪。**

| | trip-planner | trading-bot |
|---|---|---|
| 自己的後端 | 沒有(靜態前端) | 有(cmd/web 常駐 VPS) |
| 資料 | 無 | journal.csv、jsonl 在 VPS 硬碟 |
| 外部 API | 只有 LLM | BingX key 綁 VPS 的 IP 白名單 |
| BFF 做什麼 | 藏 key + 轉發 | 藏 key + **執行工具** |

放 Worker 的代價:工具仍得回 VPS 執行(Worker 讀不到硬碟、IP 不在 BingX 白名單)→ 多繞一圈;
cmd/web 目前只在 Tailscale 內網,要讓 Worker 呼叫就得對外開放或架 Tunnel + 驗證 → 攻擊面變大;
引擎 / validator / 風控是 Go,要重寫或包 API;免費方案每請求 50 個子請求,agent loop 容易碰到。

**Cloudflare 的合理位置:** 之後多使用者、對外公開時,放在最前面做 Tunnel + Access(登入、限流、DDoS)。
agent loop 與工具留在 Go,貼近資料。

---

## 11. ⚠️ Gemini 免費方案:每個模型每天 20 次(2026-10-26 實測,轉述)

使用者在 trip-planner 實測(另一個 session 轉述,詳見 docs/ai_fanout_design.md §8.8):
免費金鑰**每個模型每天 20 個請求**,依模型分開計算,台灣時間約 15:00 重置;Google Search grounding 沒有免費額度;
可用的模型依金鑰而異(那把金鑰:3.6-flash、3.1-flash-lite 可用,2.5-flash 對新使用者 404)。

**對這份設計的影響:**

- agent loop 每一步都是一次 LLM 請求。一則訊息若查 3 次工具 ≈ 4 個請求 →
  **一個模型一天大約只夠 5 則訊息**。§5 的 MaxSteps=6 在免費方案下太奢侈。
- 限制 ③「多代理省 token」在免費方案下要改寫:瓶頸是**請求次數**不是 token。
  §6 的 deep_analysis 若派 4 個 LLM worker,一次就吃掉 5 個請求。

**調整:**

1. **工具不呼叫 LLM。** 所有讀取工具都是確定性的 Go 程式(本來就是),scatter 交給工具、gather 只呼叫一次 LLM ——
   跟 trip-planner 用 Tavily 做 scatter 是同一個想法。
2. **deep_analysis 改成「確定性 scatter + 一次 LLM 彙整」**:結構 / 盤勢 / 執行 / 回測四個面向的資料由 Go 一次算好,
   LLM 只寫一次彙整。免費方案下不做 LLM worker 平行。
3. **模型備援鏈 + 每日斷路器:** 主模型遇到 per-day 429 → 記下該模型到重置時間前都跳過 → 換下一個模型;
   per-minute 429 才 backoff 重試。可用模型清單在啟動時(或設定頁)依金鑰實測。
4. **少一步:** 每頁自動帶上下文(§1 Conversational UI)本身就能省掉「先查一下現在在看什麼」那一步。
5. **額度顯示:** 聊天框顯示今天各模型剩幾次,用完時誠實說「今天的免費額度用完了,15:00 重置」。

**新增待決定:** 免費方案的額度大約一天 5–10 則對話(看備援鏈有幾個模型)。
夠用嗎?不夠的話,選項是 Gemini 付費金鑰,或改用 Anthropic(已有 client,但要另外加 tool calling)。
