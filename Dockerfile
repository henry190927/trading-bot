# syntax=docker/dockerfile:1

# ═══════════════════════ stage 1:build ═══════════════════════
FROM golang:1.25 AS builder

WORKDIR /src

# go.mod / go.sum 先複製,依賴沒變時這一層有快取 —— 改 code 不用重抓模組
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0   靜態連結。scratch 裡沒有 libc,動態連結的 binary 會起不來
# -trimpath       移除本機絕對路徑 → build 可重現,也不洩漏目錄結構
# -ldflags="-s -w" 拿掉符號表與 DWARF 除錯資訊 → binary 小一截
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/trading-web ./cmd/web

# ═══════════════════════ stage 2:runtime ═════════════════════
# scratch = 完全空的映像。沒有 shell、沒有 libc、沒有 /etc、沒有任何檔案。
# Go 靜態 binary 不需要那些 —— 但有三個東西要自己補。
FROM scratch

# 🔴 補 1:CA 憑證
# scratch 沒有 /etc/ssl/certs,少這行所有 HTTPS 外呼都會:
#   x509: certificate signed by unknown authority
# 這個 app 會打 BingX / coingecko / etherscan / anthropic...,一定要。
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# 🔶 補 2:時區 —— 本專案【不需要】
# 程式用 time.FixedZone("Asia/Taipei", 8*3600),純 Go 計算,不讀 tzdata。
# 如果改用 time.LoadLocation("Asia/Taipei"),就要解開下面這行:
# COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# 模板與靜態檔已經 //go:embed 進 binary,不用複製。
COPY --from=builder /out/trading-web /trading-web

# 🔶 補 3:非 root
# scratch 沒有 /etc/passwd,所以只能用數字 UID(不能寫 USER nobody)。
# 65534 是 nobody 的慣例 UID。
USER 65534:65534

ENV WEB_BIND=:8080
EXPOSE 8080

ENTRYPOINT ["/trading-web"]
