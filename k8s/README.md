# trading-web on Kubernetes — 建置紀錄與面試筆記

> 2026-10-12 實作。**生產環境刻意不跑 K8s**(理由見最後一節)。
> 這份的價值是:**證明我理解 probe / resource / GOMEMLIMIT 在解什麼問題**,
> 而且每個結論都有自己跑過的實驗。

```
Dockerfile           多階段 → FROM scratch
k8s/deployment.yaml  Deployment + Service
cmd/web/main.go      新增 /readyz(readiness),/health 維持 liveness 語意
```

---

## 成果

```
trading-web:scratch          27 MB     ⭐
  └ 26.8 MB  binary(-ldflags="-s -w" 從 40MB 剝到 26.8MB)
  └  224 kB  ca-certificates.crt
  └      0 B  ENV / USER / EXPOSE / ENTRYPOINT(純 metadata)

對照
  redis:7-alpine              41.7 MB   ← 比 redis 還小
  postgres:15                  467 MB
  tenmax-video-generator:beta 1.38 GB   ← 51 倍
```

minikube 載入只要 **4.8 秒**,部署到 Ready **4 秒**。

---

## 這個 app 為什麼適合 `FROM scratch`

```
✅ CGO_ENABLED=0 可建置              → scratch 沒有 libc
✅ time.FixedZone 不是 LoadLocation  → 不需要 tzdata
   (寫 LoadLocation 的話容器一跑就 panic)
✅ //go:embed templates + static     → 不需要磁碟上的資產
🔴 只缺 CA 憑證                      → 從 builder COPY 一行
```

### 🧪 實驗:證明 `ca-certificates` 那行的必要性

建了一個拿掉該行的對照映像:

| | 有憑證 | 沒憑證 |
|---|---|---|
| `/health` | 200 ✅ | **200 ✅** |
| `/api/tickers` | 200 ✅ | **200 ✅** |
| x509 錯誤 | 0 | **1** 🔴 |

```
econcal: Get "https://nfs.faireconomy.media/...":
  tls: failed to verify certificate: x509: certificate signed by unknown authority
  (calendar shows curated events only)
```

⭐ **重點不是「會壞」,是「壞得很安靜而且只壞一半」。**
容器健康、主要 API 正常,只有經濟日曆默默退化成內建資料。
一個只問「我還活著嗎」的健康檢查,會對一個半殘的服務說 OK。

---

## 三種探針 + 實測

```
startup    啟動期間【接管】liveness,通過後交棒
           沒有它 → 啟動慢的服務會在開機途中被殺 → 無限重啟
           failureThreshold × periodSeconds = 允許的最長啟動時間

liveness   「我卡死了嗎?」失敗 → 殺掉容器重啟
           🔴 絕對不可以檢查外部相依

readiness  「我現在能服務流量嗎?」失敗 → 從 Service 端點移除,【不重啟】
           這裡才是檢查相依的地方
```

### 🧪 實驗 A:把 readiness 指向 404 路徑

```
READY      1/1 → 0/1
STATUS     Running(沒變)
RESTARTS   0        ⭐ 一次都沒重啟
端點       {"notReadyAddresses":[{"ip":"10.244.0.4"}]}
           ENDPOINTS 欄位變空 → Service 沒有可用後端
kubelet    Readiness probe failed: HTTP probe failed with statuscode: 404
還原後     自動加回 10.244.0.5:8080
```

### 🧪 實驗 B:把 liveness 指向 404 路徑

```
+32s   1/1  Running  RESTARTS 0
+40s   0/1  Running  RESTARTS 1   ⭐ 容器被殺掉重啟
+72s   1/1  Running  RESTARTS 2   ⭐ 又一次
→ 每 ~32 秒重啟一次,再幾輪進 CrashLoopBackOff
```

### 🔴 所以 liveness 不能碰外部相依

親眼看到的因果鏈:

```
探針失敗 → 容器被殺 → 重啟 → 探針還是失敗 → 再殺... → CrashLoopBackOff
```

如果那個探針打的是 BingX:

```
BingX 掛掉 → 你【全部】的 pod 開始重啟迴圈
→ 重啟你的程式不會修好別人的服務
→ 而且在對方正要恢復的時候,你所有副本都在冷啟動
```

**把別人的降級,變成了自己的崩潰。**

---

## 🔴 自己踩到的設計 bug:readiness 死結

第一版的 `/readyz` 寫成:

```go
if !s.Running || s.LastAgeSec < 0 || s.LastAgeSec > readyMaxAgeSec { → 503 }
```

部署後 **readiness 永遠 503**。原因:

```go
// stream.go:235 — hub 是 LAZY 的
if !h.running { ...啟動三個迴圈... }   // 第一個訂閱者來才跑
// stream.go:250
if len(h.subs) == 0 && h.running { h.running = false }   // 沒人看就停
```

`hub.Running` 的意思是**「現在有人在看 SSE」**,不是「服務健康」。於是:

```
沒有訂閱者 → Running=false → not ready
  → kubelet 不送流量進來
    → 永遠不會有訂閱者
      → 永遠 not ready   🔴 死結
```

### ⭐ 教訓

> **readiness 問的是「我能服務嗎」,不是「我正在服務嗎」。**
>
> 一個剛啟動、零流量的 pod **是 ready 的** —— 它只是還沒被叫到。
> 拿「目前有無流量」當健康指標,就會做出永遠起不來的服務。

修正後:陳舊檢查**只在 hub 真的在跑時**才套用。閒置的 hub 是正常狀態,
在跑卻資料老舊才是異常。

```
閒置      ready hub_running=false last_age=-1s  live=0/17    200 ✅
有訂閱者  ready hub_running=true  last_age=0s   live=17/17   200 ✅
```

---

## ⭐ Go 在容器裡的兩個坑

### 坑 1:`GOMEMLIMIT` —— 不設會被無聲 OOM-kill

Go 的 GC 用 `GOGC`(預設 100)決定何時回收:**讓 heap 長到上次的兩倍**。
它**看不到 cgroup 的記憶體上限**。

```
limits.memory=256Mi
  → Go 可能讓 heap 長過 256Mi
    → kernel SIGKILL 這個 process
      → K8s 重啟 → 看起來像無來由的 CrashLoopBackOff
        → 🔴 Go 這邊【沒有任何錯誤訊息】,因為是被 SIGKILL 的
```

`GOMEMLIMIT`(Go 1.19+)給 runtime 一個軟上限,逼近時 GC 轉積極。
設在 `limits.memory` 的 ~80%,**留空間給 goroutine 堆疊和 runtime 結構** ——
那些不算在 heap 裡,但算在 cgroup 裡。

```yaml
GOMEMLIMIT: "200MiB"        # limits.memory 256Mi 的 78%
```

### 坑 2:`GOMAXPROCS` —— Go 看的是主機 CPU 數,不是 CPU limit

```
主機 10 核 + limits.cpu=500m
  → Go 還是開 10 個 P
    → 過度 context switch
    → GC 背景 worker 數量也算錯
      → 被 cgroup throttle 時延遲很難看
```

手動設,或 `import _ "go.uber.org/automaxprocs"`(會自己讀 cgroup)。

### 📌 CPU 與記憶體的超限行為不同

```
CPU 超過 limit    → throttle(變慢),程式還活著
記憶體超過 limit  → 🔴 SIGKILL,沒有優雅關閉、沒有 log、沒有 defer
```

---

## 為什麼 replicas: 1

不是保守,是**這個服務現在無法水平擴展**:

```
· 會寫 journal.csv → 多副本互相覆寫
· 跑背景 daemon    → 每個副本各自連交易所 websocket
· 本機狀態檔       → chart state / setups / env
```

要能加副本,得先:**狀態外部化(DB)· 寫入路徑抽成單一寫入者 · websocket 訂閱收斂**。

**「能不能加副本」是架構問題,不是把數字改大。**

同理用 `strategy: Recreate` 而非 `RollingUpdate` —— 單副本滾動更新會讓新舊
兩個 pod 同時連 websocket、同時寫同一份狀態。短暫斷線換取不重疊。

---

## 🎯 面試時的定位

> 「我把 trading-bot 容器化了,也寫了 K8s 清單並在 kind/minikube 上跑過,
> 包含三種探針、resource limits 和 GOMEMLIMIT。
>
> 但生產環境我**刻意沒有**上 K8s —— 三個 Go 執行檔、一台機器、單人使用,
> 單節點叢集是淨成本:多了 control plane、etcd、CNI 一整層會壞的東西,
> 換來零個我需要的功能。Docker 本身倒是有用,因為它讓『重建機器』變便宜。
>
> 這套清單的價值在於我知道 probe 在解什麼問題 —— 而且我踩過坑:
> 第一版的 readiness 檢查了一個 lazy 的 hub,結果做出一個永遠起不來的服務。
> 那次之後我才真的理解 readiness 問的是『我能服務嗎』不是『我正在服務嗎』。」

⭐ **「知道什麼時候不該用」+「踩過具體的坑」,比「會寫 YAML」更能顯示資深度。**

獨家素材(多數人只能講 Node/Python 的肥映像):
```
Go 靜態連結 → FROM scratch → 27MB
GOMEMLIMIT  → Go GC 看不到 cgroup limit 的那個無聲 OOM
```

---

## 📖 名詞對照(第一次接觸 K8s 看這段)

### Kubernetes 在解什麼問題

本來是**管一群伺服器**的系統:你有 N 台機器,它決定哪個服務跑在哪台、
掛了怎麼搬、流量怎麼導、怎麼滾動更新不斷線。

**minikube** = 把一整個叢集塞進筆電裡的一個 Docker 容器,讓你不用租機器就能學。

今天實際疊了四層:

```
MacBook
└── Docker Desktop        一個 Linux 虛擬機
    └── minikube 容器     假裝自己是一台「節點」伺服器
        └── Kubernetes    API server · etcd · kubelet · scheduler
            └── Pod
                └── trading-web 容器(27MB)
```

⭐ **容器裡跑著叢集,叢集裡跑著容器** —— 所以 minikube 看不到本機的映像,
要 `minikube image load`。它們是兩個不同的 Docker daemon。

### 名詞

| 名詞 | 是什麼 | 類比 |
|---|---|---|
| **Pod** | 部署的最小單位,一或多個容器共用網路與儲存 | 一個「程式實例」 |
| **Deployment** | 管 Pod 的副本數、滾動更新、版本回退 | 「我要 3 份,壞了自己補」 |
| **Service** | 穩定的網路入口,靠 **label** 找 Pod(不是靠名字) | 內部的負載平衡器 |
| **Node** | 一台實際的機器(minikube 裡只有一個) | 伺服器 |
| **kubelet** | 每台 Node 上的代理人 —— **執行探針、啟停容器** | 現場工頭 |
| **etcd** | 存整個叢集狀態的資料庫 | 叢集的真相來源 |
| **kubectl** | 你的遙控器,透過 API server 下指令 | CLI |
| **ClusterIP** | 只有叢集內部能打的 Service 類型 | 內網 IP |
| **port-forward** | 從筆電挖一條隧道進叢集 | 臨時後門,只給開發用 |

### 一次部署實際發生什麼

```
kubectl apply -f deployment.yaml
   ↓  送給 API server
API server 寫進 etcd:「我要一個 trading-web,1 個副本」
   ↓
Scheduler 決定:放在 minikube 這個 Node 上
   ↓
該 Node 的 kubelet 收到指令
   ↓  拉映像(本機已有 → IfNotPresent 不去 registry)
   ↓  啟動容器
   ↓  開始跑 startupProbe(每 2 秒打 /health)
   ↓  startup 通過 → 交棒給 liveness + readiness
   ↓
readiness 通過 → Pod 的 IP 被加進 Service 的端點清單
   ↓
流量才會進來
```

**「READY 1/1」的 1/1 就是 readiness 的結果**,不是「容器有沒有啟動」。

### 今天用到的指令

```bash
minikube start/stop/delete      叢集開關(stop 保留狀態,delete 砍掉重練)
minikube image load <映像>      把本機映像塞進叢集的 daemon
kubectl apply -f <yaml>         套用設定
kubectl get pods -w             看 Pod 狀態(-w = 持續監看)
kubectl describe pod <名字>     ⭐ 查問題第一個用這個,下面有 Events
kubectl logs <pod>              看容器 stdout
kubectl get endpoints <svc>     ⭐ 看 Service 現在有哪些可用後端
kubectl port-forward svc/x 8080:80   從筆電打進去
```

> 🔎 **排錯口訣:`describe` 看 Events,`logs` 看程式。**
> 探針失敗、拉不到映像、資源不足,全都在 `describe` 的 Events 區。


---

## 重現

```bash
docker build -t trading-web:scratch .
minikube start --driver=docker --cpus=2 --memory=2200
minikube image load trading-web:scratch
kubectl apply -f k8s/deployment.yaml
kubectl get pods -l app=trading-web -w

kubectl port-forward svc/trading-web 18090:80
curl localhost:18090/health
curl localhost:18090/readyz

# 清理
kubectl delete -f k8s/deployment.yaml
minikube stop
```
