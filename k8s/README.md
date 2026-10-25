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

## ⭐ monitor:「活著但卡住」的 liveness(2026-10-25)

`k8s/monitor.yaml` · `cmd/monitor/health.go` · `cmd/monitor/fault.go`

### 問題

monitor 沒有 HTTP,映像是 `FROM scratch`(7.4 MB,沒有 shell)→ liveness 不能 `exec cat`。
而它最危險的故障不是「掛掉」(K8s 本來就會重啟),是 **process 活著、某個迴圈卡在
網路呼叫裡** —— 裸倉守衛安靜地停了,容器看起來一切正常。

### 設計(兩層)

```
/livez    只看關鍵迴圈(bracket 裸倉守衛、autoexec 自動交易)→ kubelet 重啟
/healthz  全部迴圈狀態(JSON)→ 給人、給 /ops
/metrics  每個迴圈距上次心跳幾秒 → Prometheus,非關鍵迴圈用長門檻【只通知不重啟】
```

為了「背景資料晚一點更新」重啟整個 daemon,會連裸倉守衛一起中斷 —— 所以只有
**重啟修得好、而且值得中斷一切**的才進 /livez。

判斷核心 `criticalStalled`(自己寫的):

```
· 卡住 = heartbeat.Loop.Stalled(now):Running && 距上次心跳 > max(3×間隔, 2 分鐘)
· 被設定關掉(Off)、或根本沒註冊的迴圈【不算】—— 重啟修不好設定,
  算進去只會 CrashLoop
· Idle 不豁免:idle 的迴圈每輪照樣打心跳,停了就是卡住
```

### 探針數字怎麼來的

```
bracket 卡住 ──(2 分鐘:Stalled 門檻)──> /livez 503 ──(15s × 4)──> 重啟
目標:卡住後 3 分鐘內重啟 → 留給 K8s 的只有 1 分鐘

periodSeconds 15 · failureThreshold 4 · timeoutSeconds 3
· failureThreshold 不能是 1:一次 GC 停頓或 CPU throttle 就殺掉健康的服務,
  幾輪後進 CrashLoopBackOff —— 停機比你想防的卡住還久
· timeoutSeconds 要遠小於 period:HTTP 本身卡住時,120s 的 timeout × 4 = 8 分鐘
· 不需要 startupProbe:hb.Start 把啟動算成一次心跳
```

### 🧪 實驗:故障注入 `MONITOR_FAULT_STALL=autoexec:90s`

開機滿 90 秒後的那一輪讓 autoexec 永久卡住(HTTP 照常)。autoexec 每 60s 一輪,
門檻 3 分鐘。**先寫預期:** 2:00 卡住(1:00 那輪還不到 90s)→ 5:00 開始 503 → ≤6:00 重啟。

```
13:09:31  health endpoints on :9090
13:11:31  FAULT INJECTION: autoexec loop now blocked      ← 開機後 2:00 ✅

+  0s … +273s   livez 200 {"ok":true,"stalled":[]}              RESTARTS 0
+288s … +333s   livez 503 {"ok":false,"stalled":["autoexec"]}
+349s           livez 200                                        RESTARTS 1  ← 503 後 ~57s ✅

Warning  Unhealthy  Liveness probe failed: HTTP probe failed with statuscode: 503
Normal   Killing    Container monitor failed liveness probe, will be restarted
```

觀察:
- **重啟的是容器,不是 pod**(pod 名字不變,RESTARTS +1)。改 env 則是新的 ReplicaSet、新 pod。
- 開關沒關時,重啟後 2 分鐘又會卡 → 一直循環就是 CrashLoopBackOff。
  **重啟只救暫時性的卡住;程式本身的 bug 每次都會再卡,最後還是要人修。**

---

## ⭐ Secret 與 ConfigMap(2026-10-25)

`k8s/config.yaml` · `k8s/secret.example.yaml`(真值不進檔案)

### Secret:base64 不是加密

```
$ kubectl get secret trading-secrets -o jsonpath='{.data}'
{"BINGX_API_KEY":"ZHVtbXkta2V5LWZvci1taW5pa3ViZQ==", ...}
$ echo ZHVtbXkta2V5LWZvci1taW5pa3ViZQ== | base64 -d
dummy-key-for-minikube
```

base64 是**編碼**(讓二進位能放進 YAML),一行就還原。commit 一份 secret.yaml = 把明碼 key
放進 git 歷史,刪檔也救不回來,只能換 key。所以真的 Secret 用 `kubectl create secret … --from-literal`
建,`k8s/secret.yaml` 在 .gitignore。

真正保護 Secret 的是:RBAC(誰能 get secret)· etcd encryption at rest · 外部密鑰管理
(Vault / 雲端 Secret Manager)· Sealed Secrets / SOPS(用叢集公鑰加密後【可以】commit,GitOps 用)。

### 🧪 實驗:改 ConfigMap,不重啟,誰會變?

```
改:OI_SAMPLE_SEC 300 → 120(envFrom)· autotrade.json enabled true → false(volume)

+  0~46s  autoexec idle=False                            | oisample every=300
+ 61s     autoexec idle=True「enabled=false → 每輪短路」 | oisample every=300
+166s     autoexec idle=True                             | oisample every=300
同一個 pod,RESTARTS 0
```

| | 方式 | 不重啟會生效? | 原因 |
|---|---|---|---|
| autotrade.json | volume(整個目錄) | ✅ ~1 分鐘 | kubelet 改寫檔案 + `autotrade.Load()` 每輪重讀 |
| OI_SAMPLE_SEC | envFrom | ❌ | 環境變數只在容器啟動時複製一次 |

兩個前提:程式要會重讀檔案;**不能用 subPath**(subPath 是一次性複製,永遠不更新)。
環境變數要生效只能重啟:`kubectl rollout restart`,或讓 ConfigMap 變動自動觸發滾動更新
(Helm 的 checksum annotation、Kustomize configMapGenerator 的雜湊後綴)。

---

## ⭐ PVC:pod 換掉,資料還在(2026-10-25)

`k8s/monitor-pvc.yaml`(accessModes 與容量由我自己決定)

```
accessModes  ReadWriteOncePod   ← 單一寫入者:擋的是【pod】
storage      100Mi              ← 資料約 5 MB 且會長;standard 不支援擴容,一開始就留餘裕
```

**為什麼不是 ReadWriteOnce:** RWO 擋的是【節點】。單節點叢集上兩個 monitor pod 都在同一台,
兩個都掛得上、同時寫 oi.jsonl、同時下單。RWOP 才是「整個叢集只有一個 pod 能用」。
**容量單位:** `storage: 5` 是 5 bytes —— 跟 periodSeconds 同一類坑,沒寫單位就是最小單位。

### 🧪 實驗(oi.jsonl 每次取樣寫 17 行 = 17 個標的)

```
A emptyDir   刪 pod 前 51 行 → 新 pod 17 行   舊資料全沒了(17 是新 pod 自己寫的)
B PVC        刪 pod 前 51 行 → 新 pod 68 行   51 + 17,接著寫 ✅
C 第二個 pod 掛同一個 RWOP claim → Pending
  FailedScheduling: 1 node has pod using PersistentVolumeClaim with the same name
                    and ReadWriteOncePod access mode.
```

- **Pending 不是「跑起來但寫不進去」,是排程器根本不讓它啟動** —— 第二個寫入者不存在,
  程式不必自己處理寫入衝突。
- minikube hostpath 不是 CSI,RWOP 照樣被強制(先前不確定,實驗確認)。
- **沒觀察到的:** B 刪 pod 後 +5s 就只剩新 pod Running —— monitor 收到 SIGTERM 幾秒內就乾淨結束,
  空間立刻釋放,交接時「新 pod 先 Pending」的那一刻沒抓到。要看到得用 preStop 拖住舊 pod + 1s 取樣。
- **檢查 scratch 容器的檔案:** 沒有 shell 不能 exec,用 `kubectl debug <pod> --image=busybox --target=monitor`
  共用 process namespace,從 `/proc/1/root/opt/trading/` 讀。

### Deployment + PVC,還是 StatefulSet?

replicas 固定 1 時,Deployment + RWOP PVC + `strategy: Recreate` 已經保證單一寫入者。
StatefulSet 的價值在多副本:每個副本一份自己的 PVC(volumeClaimTemplates)、穩定的名稱與順序。
這個服務不會有第二個副本,所以刻意不用 —— 跟「生產不上 K8s」同一個判斷:不需要的東西不建。

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

## 📐 deployment.yaml 逐段解讀

> 讀完名詞對照之後看這段。**每個欄位都說明「不寫會怎樣」** ——
> 那才是理解一個設定存在理由的方式。

### 每個 K8s 物件都是同一個形狀

```yaml
apiVersion: apps/v1     # 這個物件歸哪個 API 群組/版本管
kind: Deployment        # 這是什麼東西
metadata:               # 它叫什麼、貼什麼標籤
  name: trading-web
spec:                   # 你要的【目標狀態】
  ...
```

⭐ **K8s 的核心心智模型:你描述「想要的樣子」,controller 持續把現實推向它。**
不是「執行這些步驟」,是「維持這個狀態」。pod 被殺掉會自己長回來,
因為你宣告的是 `replicas: 1`,不是「啟動一個容器」。

### Deployment 的 spec

```yaml
spec:
  replicas: 1
```
要幾份。**不寫預設是 1。** 這裡寫 1 是刻意的 —— 見上面「為什麼 replicas: 1」。

```yaml
  selector:
    matchLabels:
      app: trading-web
```
🔴 **這是整份檔案最容易誤解的地方。**
Deployment **不是**靠名字管理 pod,是靠**標籤比對**。它去找所有帶
`app: trading-web` 標籤的 pod,數一數夠不夠 `replicas`。

→ 所以下面 `template.metadata.labels` 必須包含這組標籤,**否則 Deployment
建出 pod 之後就認不得自己建的那些**,會無限建下去。
(K8s 會擋這種設定,但理由值得知道。)

```yaml
  strategy:
    type: Recreate
```
更新時怎麼換。`RollingUpdate`(預設)先起新的再殺舊的;`Recreate` 先殺乾淨再起。
**這裡選 Recreate 是因為新舊兩個 pod 會同時連交易所 websocket、同時寫同一份狀態。**
短暫斷線換取不重疊。

### Pod template

```yaml
  template:
    metadata:
      labels:
        app: trading-web      # ⭐ 必須對上上面的 selector
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534
```
**不寫的話容器以 root 跑。** scratch 映像沒有 `/etc/passwd`,所以只能用數字 UID
(寫 `runAsUser: nobody` 會解析失敗)。`runAsNonRoot: true` 是保險:
萬一映像裡設定成 root,kubelet 會拒絕啟動而不是默默以 root 跑。

### Container

```yaml
      containers:
        - name: web
          image: trading-web:scratch
          imagePullPolicy: IfNotPresent
```
🔴 `IfNotPresent` 在本機 minikube 是**必要的**。
預設的 `Always` 會去 registry 拉 —— 但 `trading-web:scratch` 只存在你本機,
registry 上沒有,結果是 `ErrImagePull`。

> 📌 冷知識:tag 是 `:latest` 時預設就是 `Always`,其他 tag 預設 `IfNotPresent`。
> 這也是「生產不要用 `:latest`」的其中一個理由 —— 你不知道拉到的是哪一版。

```yaml
          ports:
            - name: http
              containerPort: 8080
```
⚠️ **這一段幾乎是純文件用途。** 容器監聽什麼 port 是程式決定的,寫在這裡
**不會**讓它開 port、也不會擋掉沒宣告的 port。

**但 `name: http` 有實際作用** —— 下面的 probe 和 Service 用名字引用它。
以後改 port 只要改這一處。

```yaml
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: ["ALL"]
```
`readOnlyRootFilesystem: true` 之後,**任何要寫的路徑都必須明確掛出來**
(下面的 volumeMounts)。scratch 本來就幾乎是空的,順手鎖死。

### resources —— 兩個數字,兩種意義

```yaml
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
            limits:
              cpu: 500m
              memory: 256Mi
```

```
requests  排程器用它【找節點】。宣告「我至少需要這麼多」,
          節點的剩餘量不夠就不會把 pod 排上去。
limits    硬上限。
```

🔴 **CPU 和記憶體超限的行為完全不同:**

```
CPU 超過 limit    → cgroup throttle,程式變慢但活著
記憶體超過 limit  → kernel SIGKILL,沒有優雅關閉、沒有 log、defer 不執行
```

`100m` = 0.1 顆 CPU(m = millicore)。`Mi` 是 1024 進位,`M` 是 1000 進位 —— 混用會差 5%。

**不寫 requests/limits 會怎樣?** pod 變成 `BestEffort` QoS ——
節點記憶體吃緊時**第一個被殺**。而且排程器不知道你要多少,可能把你排到已經很擠的節點。

### 三種探針

```yaml
          startupProbe:
            httpGet: { path: /health, port: http }
            periodSeconds: 2
            failureThreshold: 30
```
`2 × 30 = 60 秒`,這是**允許的最長啟動時間**。
通過之前,liveness 和 readiness 都不會開始計算。

**不寫會怎樣?** 啟動慢的服務會在開機途中被 liveness 殺掉 → 無限重啟。
你的 app 啟動只要 2 秒,所以其實不需要 —— 但寫著是好習慣,
而且未來若加上大量資料載入就不用回頭補。

```yaml
          livenessProbe:
            httpGet: { path: /health, port: http }
            periodSeconds: 10
            timeoutSeconds: 3
            failureThreshold: 3
```
`10 × 3 = 約 30 秒`才會重啟。`failureThreshold: 3` 的意義是
**不要因為單次抖動就殺掉容器**。

```yaml
          readinessProbe:
            httpGet: { path: /readyz, port: http }
            periodSeconds: 5
            timeoutSeconds: 2
            failureThreshold: 2
```
⭐ **readiness 比 liveness 敏感是刻意的**(2 次 vs 3 次,5 秒 vs 10 秒)。

```
先停流量(可逆、無痛)  →  最後才重啟(破壞性)
```

這個不對稱是 probe 設計的核心直覺。

### volumes

```yaml
          volumeMounts:
            - name: state
              mountPath: /opt/trading
      volumes:
        - name: state
          emptyDir: {}
```
🔴 **`emptyDir` 隨 pod 消滅。** 這只適合展示。

真要保留狀態得用 `PersistentVolumeClaim`,而那又會把 Deployment 推向
`StatefulSet`(單一寫入者語意、穩定的網路識別)。

**「要不要持久化」和「能不能水平擴展」是同一個問題的兩面。**

```yaml
      terminationGracePeriodSeconds: 30
```
收到 `SIGTERM` 後給多久優雅關閉,逾時就 `SIGKILL`。
這個服務有 SSE 長連線和 gRPC 串流,需要時間讓它們收尾。

### Service

```yaml
apiVersion: v1
kind: Service
spec:
  type: ClusterIP
  selector:
    app: trading-web
  ports:
    - name: http
      port: 80
      targetPort: http
```

```
type: ClusterIP   只有叢集【內部】打得到。對外要 NodePort / LoadBalancer / Ingress
selector          ⭐ 又是標籤 —— Service 不知道 Deployment 存在,它只找標籤符合的 pod
port: 80          Service 對外的 port
targetPort: http  指向容器那個【具名】port(8080),不是寫死數字
```

⭐ **Service 和 Deployment 之間沒有直接關係。**
它們只是剛好都用 `app: trading-web` 這個標籤。Service 找到的是
**readiness 通過的 pod**,沒通過的會被放進 `notReadyAddresses`。

這就是實驗 A 看到的現象:readiness 一失敗,`ENDPOINTS` 欄位就空了。


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
