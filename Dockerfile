# syntax=docker/dockerfile:1

# 一份 Dockerfile,兩個映像:
#   docker build --build-arg CMD=web     -t trading-web .
#   docker build --build-arg CMD=monitor -t trading-monitor .
ARG CMD=web

# ═══════════════════════ stage 1:build ═══════════════════════
# --platform=$BUILDPLATFORM:編譯器用「建置機器本身」的架構跑(Apple Silicon 的
# Mac 是 arm64、GitHub 的 runner 是 amd64),再交叉編譯成 TARGETARCH。
# 少了這行,在 Mac 上建 amd64 映像時,整套 Go 工具鏈會在 QEMU 模擬器裡跑,慢好幾倍。
FROM --platform=$BUILDPLATFORM golang:1.25 AS builder
ARG CMD
ARG TARGETOS TARGETARCH

WORKDIR /src

# go.mod / go.sum 先複製,依賴沒變時這一層有快取 —— 改 code 不用重抓模組
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0   靜態連結。scratch 裡沒有 libc,動態連結的 binary 會起不來
# -trimpath       移除本機絕對路徑 → build 可重現,也不洩漏目錄結構
# -ldflags="-s -w" 拿掉符號表與 DWARF 除錯資訊 → binary 小一截
# -tags timetzdata 把 IANA 時區資料庫嵌進 binary(約 450 KB)。
#   scratch 沒有 /usr/share/zoneinfo,而有兩條路徑需要它:
#   session.go 載入 America/New_York(美股開盤、含夏令時間切換),
#   journal 用 time.Local(TZ=Asia/Taipei)解析沒帶時區的時間。
#   少了它兩邊都會【安靜地】退回 UTC —— New York 那條包在 `if err == nil` 裡,
#   不會報錯。跟 CA 憑證是同一類問題:壞得很安靜。
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build \
      -tags timetzdata -trimpath -ldflags="-s -w" \
      -o /out/app ./cmd/$CMD

# ═══════════════════════ stage 2:runtime ═════════════════════
# scratch = 完全空的映像。沒有 shell、沒有 libc、沒有 /etc、沒有任何檔案。
FROM scratch

# CA 憑證:scratch 沒有 /etc/ssl/certs,少這行所有 HTTPS 外呼都會
#   x509: certificate signed by unknown authority
# 而且 /health 照樣回 200 —— 實驗紀錄見 k8s/README.md。
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# 模板與靜態檔已經 //go:embed 進 binary,不用複製。
COPY --from=builder /out/app /app

# 非 root。scratch 沒有 /etc/passwd,只能用數字 UID;65534 是 nobody。
# compose 會改成擁有 /opt/trading 的那個 UID,這裡只是安全的預設值。
USER 65534:65534

# 綁定位址(WEB_BIND)、時區(TZ)、記憶體上限(GOMEMLIMIT)由執行環境給
# (compose.yaml / k8s/deployment.yaml),不寫死在映像裡 —— 同一個映像要能
# 在本機、VPS、K8s 三個地方跑。
ENTRYPOINT ["/app"]
