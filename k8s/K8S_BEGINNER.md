# K8s 那塊,從頭講

## 先講要解決什麼問題

你有一支程式。要讓它**在別人的機器上也跑得起來、而且掛了會自己活過來** —— 這是兩個不同的問題:

```
問題 1  怎麼把程式連同它需要的東西打包起來?   → Docker(容器)
問題 2  怎麼讓它掛了自動重啟、流量自動繞開?   → Kubernetes
```

**我們兩個都做了,但只有第一個真的上生產。** 這個判斷本身就是面試素材,等等講。

---

## Part 1 — 打包:Dockerfile

### 容器不是虛擬機

這是最容易誤會的點:

```
虛擬機   模擬一整台電腦,要裝完整作業系統   → 好幾 GB
容器     ⭐ 共用主機的 kernel,只裝「你的程式會讀到的檔案」
```

所以容器映像裡裝的**不是作業系統,是一包檔案** —— 可以想成一個解壓縮後會變成 `/` 的 zip。

### `FROM scratch` = 從空的開始

```dockerfile
FROM ubuntu:22.04    # 我的檔案包,從 Ubuntu 那包開始疊
FROM scratch         # 從【零個檔案】開始
```

一般程式做不到這個:Python 要直譯器、Node 要 runtime、Java 要 JVM。
**但 Go 編譯出來是一個「什麼都不依賴」的執行檔** —— 標準庫、第三方套件、連你的 HTML 模板(因為用了 `//go:embed`)全部塞在那一個檔案裡。

結果:

```
golang:1.25(編譯用的映像)   ~800 MB
trading-web:scratch            27 MB    ⭐ 其中 26.8MB 就是你的程式
redis:7-alpine                 41.7 MB  ← 你的比 redis 還小
tenmax-video-generator:beta    1.38 GB  ← 51 倍
```

**「多階段建置」**就是:第一階段用 800MB 的映像編譯,第二階段只把編出來的那一個檔案撈過去。工具鏈完全不進最終映像。

### 🧪 但「空的」是有代價的 —— 我們實測了

`scratch` 裡沒有 CA 憑證,所以 HTTPS 驗不了對方的身分。我建了一個**拿掉那行**的版本對照:

| | 有憑證 | 沒憑證 |
|---|---|---|
| `/health` | 200 ✅ | **200 ✅** |
| `/api/tickers` | 200 ✅ | **200 ✅** |
| x509 錯誤 | 0 | **1** 🔴 |

```
econcal: tls: failed to verify certificate: x509: certificate signed by unknown authority
         (calendar shows curated events only)
```

⭐ **重點不是「會壞」,是「壞得很安靜而且只壞一半」。** 容器健康、主要 API 正常,只有經濟日曆默默退化成內建資料。

**一個只問「我還活著嗎」的健康檢查,會對一個半殘的服務說 OK。** 這句話直接帶出下一段。

---

## Part 2 — 健康檢查:三種探針

K8s 會定期打你的服務問「你還好嗎」。但「好」有三種意思:

```
liveness    「你卡死了嗎?」      失敗 → 殺掉容器重啟
readiness   「你能接客了嗎?」    失敗 → 從路由移除,【不重啟】
startup     「你還在開機嗎?」    通過前接管 liveness,避免開機中途被殺
```

### 🔴 最常被答錯的一題:liveness 不能檢查外部相依

想一下如果 liveness 去 ping 交易所:

```
交易所掛掉 → liveness 失敗 → K8s 重啟你的 pod
          → 重啟後交易所還是掛的 → 又失敗 → 又重啟 → CrashLoopBackOff
```

**重啟你的程式不會修好別人的服務。** 你把「別人的降級」變成了「自己的崩潰」。

### 🧪 我們把兩個都故意弄壞,看差別

**實驗 A:readiness 指向不存在的路徑**
```
READY      1/1 → 0/1
STATUS     Running(沒變)
RESTARTS   0           ⭐ 一次都沒重啟
端點       從 Service 移除 → ENDPOINTS 變空
還原後     自動加回來
```

**實驗 B:liveness 指向不存在的路徑**
```
+32s   1/1  RESTARTS 0
+40s   0/1  RESTARTS 1   ⭐ 容器被殺掉重啟
+72s   1/1  RESTARTS 2   ⭐ 又一次
→ 每 ~32 秒重啟一次,再幾輪進 CrashLoopBackOff
```

**這就是親眼看到「探針失敗 = 無限重啟」。**

### 🔴 而我自己踩了一個經典的坑

我幫你寫的第一版 `/readyz` 檢查了 `hub.Running`,結果**部署後永遠 503**。

原因:你的 ticker hub 是**懶的** —— 第一個訂閱者來才啟動,沒人看就停。所以 `Running` 的意思是「現在有人在看」,不是「服務健康」。

```
沒有訂閱者 → Running=false → not ready
  → K8s 不送流量進來
    → 永遠不會有訂閱者
      → 永遠 not ready   🔴 死結
```

⭐ **教訓:readiness 問的是「我能服務嗎」,不是「我正在服務嗎」。** 一個剛啟動、零流量的 pod **是 ready 的**,它只是還沒被叫到。

---

## Part 3 — Go 在容器裡的兩個坑

這兩個是你在面試裡最能拉開差距的地方,因為多數人只會講 Node/Python 的肥映像。

### 坑 1:`GOMEMLIMIT` —— 不設會被**無聲** OOM-kill

Go 的 GC 用 `GOGC`(預設 100)決定何時回收:**讓 heap 長到上次的兩倍**。
它**看不到容器的記憶體上限**。

```
limits.memory=256Mi
  → Go 讓 heap 長過 256Mi
    → kernel SIGKILL
      → K8s 重啟 → 看起來像無來由的 CrashLoopBackOff
        → 🔴 Go 這邊【完全沒有錯誤訊息】,因為是被 SIGKILL 的
```

`GOMEMLIMIT` 給 runtime 一個軟上限,逼近時 GC 轉積極。設在上限的 ~80%(留空間給不算在 heap 但算在容器裡的東西)。

### 坑 2:`GOMAXPROCS` —— Go 看的是主機 CPU 數

```
主機 10 核 + limits.cpu=500m → Go 還是開 10 個 P
  → 過度 context switch、GC worker 數量算錯 → 被 throttle 時延遲很難看
```

### 📌 順帶一提:CPU 和記憶體的超限行為不一樣

```
CPU 超限    → throttle(變慢),程式還活著
記憶體超限  → 🔴 SIGKILL,沒有優雅關閉、沒有 log、defer 不會執行
```

---

## Part 4 — 最後:我們刻意**沒有**上生產

```
你的工作負載   3 個 Go 執行檔 · 1 台機器 · 單人使用
K8s 帶來的    control plane · etcd · CNI 網路 · Ingress · 大量 YAML
```

**單節點叢集 = 多一整層會壞的東西,換來零個你需要的功能。**

K8s 解的是「幾十個服務、自動擴縮、滾動更新、多人協作」—— 你一個都沒有。

**但 Docker 對你有實質好處**:環境可重現,換供應商變成一行指令 —— 那直接支援架構文件裡「讓重建變便宜」的目標。

---

## 所以面試時怎麼講

> 「我把 trading-bot 容器化了,也寫了 K8s 清單並在 minikube 上跑過,包含三種探針、
> resource limits 和 GOMEMLIMIT。
>
> 但生產環境我**刻意沒有**上 K8s —— 三個執行檔、一台機器,單節點叢集是淨成本。
>
> 這套清單的價值在於我知道 probe 在解什麼問題,而且我踩過坑:第一版的 readiness
> 檢查了一個 lazy 的 hub,結果做出一個**永遠起不來的服務**。那次之後我才真的理解
> readiness 問的是『我能服務嗎』不是『我正在服務嗎』。」

⭐ **「知道什麼時候不該用」+「踩過具體的坑」,比「會寫 YAML」更能顯示資深度。**

---

產出都在這:

```
trading-bot/Dockerfile            48 行
trading-bot/k8s/deployment.yaml  166 行   三種探針 + resources + GOMEMLIMIT + Service
trading-bot/k8s/README.md        343 行   ⭐ 含名詞對照、實驗紀錄、面試定位
trading-bot/cmd/web/main.go               新增 /readyz
```

`k8s/README.md` 的「名詞對照」那節是給你白天讀的 —— Pod / Deployment / Service / kubelet / etcd 各是什麼,以及一次部署實際發生什麼。
