# SSH 救援紀錄(已解決)

> 2026-10-10 SSH 無法登入 → 2026-10-11 已解決。
> **真正的原因:金鑰有 passphrase,而 ssh-agent 是空的。伺服器端完全正常。**

---

## ✅ 結論

```
解法     ssh-add --apple-use-keychain ~/.ssh/oracle-trading.key
結果     立刻恢復。伺服器 uptime 19 週以上,從未異常。
```

---

## 🔴 我的誤判,與三個原因

我一路往「伺服器端壞掉」追,差點讓使用者去跑 GRUB single-user mode。錯在三處:

### ① 用錯方法判斷金鑰是否加密

```bash
grep -q "ENCRYPTED" "$KEY"      # ❌ 只對舊的 PEM 格式有效
```

新的 OpenSSH 格式(`-----BEGIN OPENSSH PRIVATE KEY-----`)把加密資訊放在
base64 內容裡。正確的判斷是直接試著讀:

```bash
ssh-keygen -y -P "" -f "$KEY" >/dev/null 2>&1 || echo "有 passphrase"
```

實際解碼出來是:`openssh-key-v1` + `aes256-ctr` + `bcrypt` —— 確實加密。

### ② 整串診斷都帶 `-o BatchMode=yes`

BatchMode **會把 passphrase 提示壓掉**,於是「金鑰解不開」看起來像「伺服器拒絕你」。

### ③ 把 debug 訊息讀反了

```
debug3: sign_and_send_pubkey: signing using rsa-sha2-512 ...   ← 嘗試【之前】印的
debug2: we did not send a packet, disable method               ← ⭐ 這行才是真相
debug1: No more authentication methods to try.
```

我看到第一行就認定「簽章成功 → 所以是伺服器拒絕」。**第二行明說根本沒送出封包。**

> ⭐ **教訓:`signing using` 不代表簽成功。要往下看有沒有 `we did not send a packet`。**

---

## ✅ 當時正確的觀察(保留備查)

這些推論本身沒錯,只是不足以定位:

```
port 22 有回應                      → 機器活著
主機金鑰與 known_hosts 相符          → 沒被重建、IP 沒被回收
伺服器送出 PK_OK                    → 金鑰確實在 authorized_keys 裡
```

⚠️ 但 **PK_OK 只代表「公鑰被接受」,不代表後續的簽章驗證會過** —— 簽不出來也會停在這裡。

---

## 📌 正確的事實(2026-10-11 由 myfirstgo-12 查證)

```
journal.csv   VPS 上在 /opt/trading/journal.csv
              ~/trading-bot-data/ 是 mcp-sync-journal 同步【回 Mac】的目的地
              (Makefile line 334)

ubuntu 帳號   ✅ 有密碼(passwd -S 回傳 P)· sudo 免密碼可用
              → serial console 本來就是可用的退路
              → 「兩個帳號都鎖 = lockout」的警告已過時

trading-bot   serve 於 2026-08-24 18:45 被手動正常停止(Result=success),之後未啟動
(serve)       改由 monitor 接手

備份          ~/vps-backup/20261011-225629/
              journal.csv 313 行 · sha256 471110bda411ef59(兩邊一致)
              opt-trading.tgz(整個 /opt/trading,含 .env,54 檔)
              systemd-units.txt
```

---

## 重跑備份

```bash
ORACLE_HOST=131.186.59.1 ./scripts/backup-vps.sh inventory   # 唯讀盤點
ORACLE_HOST=131.186.59.1 ./scripts/backup-vps.sh pull        # 打包 /opt/trading 到 ~/vps-backup/<時間>/
```

`pull` 只有在備份裡有 `journal.csv`、且 sha256 與 VPS 一致時才回報成功。
備份含 `.env`(交易所金鑰),絕對不要放進 git repo。

---

## 待辦 — 把這次的坑補起來

```
🔑 MFA bypass code                      ⬜ 還沒做
   OCI Console → 右上角帳號 → My Profile → Security
   → 產生 bypass code → 存進密碼管理器
   ← MFA 鎖死那件事的根因(Incident 261010-000270)

🔑 金鑰 passphrase 進 keychain          ✅ 已做
   ssh-add --apple-use-keychain ~/.ssh/oracle-trading.key
   ← SSH 這件事的根因

🔑 ssh 自動從 keychain 載入金鑰          ✅ 已做(2026-10-16)
   2026-10-16 又發生一次:agent 空了,make deploy-web 被拒。
   光把 passphrase 存進 keychain 不夠 —— agent 重開機後還是空的,
   要有 ~/.ssh/config 才會自動載入:

     Host 131.186.59.1 100.79.112.56 trading-bot
         User ubuntu
         IdentityFile ~/.ssh/oracle-trading.key
         UseKeychain yes
         AddKeysToAgent yes

   驗證方式:ssh-add -D 清空 agent → ssh -o BatchMode=yes 131.186.59.1
   仍能登入,且登入後 ssh-add -l 看得到金鑰。
   之後再遇到 Permission denied:先確認這個 config 還在。

🔑 第二把 SSH 金鑰                      ⬜ 還沒做
   ssh-keygen -t ed25519 -f ~/.ssh/vps-backup-key
   把 .pub 加進 authorized_keys(與主金鑰不同機器產)

🔑 ubuntu 密碼                          ✅ 本來就有,不用做

🔑 journal.csv 每日自動備份              ✅ 已做(2026-10-11)
   scripts/vps-data-backup.sh → private repo henry190927/trading-bot-data
   VPS cron 每天 04:15 TPE,有變動才 commit;失敗或 journal 少 >10% 會推 ntfy
   認證用只對該 repo 有寫入權的 deploy key(~/.ssh/trading-bot-data-deploy)
```

---

## 之後的完整強化計畫

`docs/architecture_hardening.md` —— 六項,其中 Docker 和 K8s 不需要 VPS。
