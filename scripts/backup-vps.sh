#!/usr/bin/env bash
# VPS 備份 —— 唯讀,不會在 VPS 上寫入任何東西、不會重啟任何服務。
#
#   ./scripts/backup-vps.sh inventory   先看有什麼(建議先跑這個)
#   ./scripts/backup-vps.sh pull        真的拉下來
#
# 需要 ORACLE_HOST(與 Makefile 同一個變數)。
set -uo pipefail

HOST="${ORACLE_HOST:-}"
USER_="${ORACLE_USER:-ubuntu}"
KEY="${ORACLE_KEY:-$HOME/.ssh/oracle-trading.key}"
# All state lives here — the systemd units and the Makefile j* targets use it.
# ~/trading-bot-data/ is only the Mac-side sync target, not a VPS path.
DATA="${ORACLE_DATA:-/opt/trading}"
MODE="${1:-inventory}"

if [ -z "$HOST" ]; then
  echo "✗ 沒有 ORACLE_HOST。用法:"
  echo "    ORACLE_HOST=1.2.3.4 ./scripts/backup-vps.sh inventory"
  exit 1
fi
[ -f "$KEY" ] || { echo "✗ 找不到金鑰 $KEY(可用 ORACLE_KEY=... 指定)"; exit 1; }

SSH="ssh -i $KEY -o ConnectTimeout=10 -o BatchMode=yes $USER_@$HOST"

echo "═══ 0. 連線測試 ═══"
if ! $SSH 'echo ok' >/dev/null 2>&1; then
  echo "🔴 SSH 進不去。先排除最常見的原因:"
  echo
  # ⚠️ 新式 OpenSSH 金鑰的加密資訊在 base64 內容裡,不在標頭 ——
  #    grep ENCRYPTED 對它無效。要直接試著讀私鑰才準。
  if ! ssh-keygen -y -P "" -f "$KEY" >/dev/null 2>&1; then
    echo "   ⭐ 金鑰有 passphrase,而 ssh-agent 裡沒有它。"
    echo "      本腳本用 BatchMode=yes,會把密碼提示壓掉 → 看起來像伺服器壞掉。"
    echo
    echo "      先跑這行再重試:"
    echo "        ssh-add --apple-use-keychain $KEY"
    echo
  else
    echo "   金鑰可直接讀取(無 passphrase),所以不是這個原因。"
    echo "   再檢查:IP 有沒有變 · 安全群組 · 伺服器端的 /home/\$USER 權限"
  fi
  exit 1
fi
echo "✅ SSH 正常"

echo
echo "═══ 1. 退路檢查(你自己警告過的)═══"
$SSH '
  echo -n "  sudo 免密碼:  "; sudo -n true 2>/dev/null && echo "✅ 可用" || echo "⚠️  不可用 —— 沒有 console 的話風險高"
  echo -n "  ubuntu 有密碼:"; sudo -n passwd -S ubuntu 2>/dev/null | awk "{print \$2}" | grep -q "^P" && echo "✅ 有" || echo "⚠️  無/查不到"
  echo -n "  authorized_keys 行數: "; wc -l < ~/.ssh/authorized_keys 2>/dev/null || echo "?"
  echo -n "  uptime: "; uptime -p 2>/dev/null
'

echo
echo "═══ 2. 服務狀態 ═══"
$SSH 'for u in trading-bot trading-web trading-monitor; do
        printf "  %-18s %s\n" "$u" "$(systemctl is-active $u 2>/dev/null) / $(systemctl is-enabled $u 2>/dev/null)"
      done'

echo
echo "═══ 3. 資料盤點 ($DATA) ═══"
$SSH "
  echo '  狀態檔(不含可重編的 trading-* binary):'
  find $DATA -maxdepth 1 -type f ! -name 'trading-*' -printf '    %M %u %9s  %TY-%Tm-%Td %TH:%TM  %f\n' 2>/dev/null | sort -k7
  echo
  echo -n '  journal.csv 行數: '; wc -l < $DATA/journal.csv 2>/dev/null || echo '(不存在!)'
  echo -n '  ubuntu 讀不到的檔案數: '; find $DATA -maxdepth 1 -type f ! -readable 2>/dev/null | wc -l
  echo
  echo '  crontab:'
  crontab -l 2>/dev/null | sed 's/^/    /' || echo '    (無)'
"

if [ "$MODE" != "pull" ]; then
  echo
  echo "────────────────────────────────────────"
  echo "盤點完成。確認上面的清單之後,執行:"
  echo "    ORACLE_HOST=$HOST ./scripts/backup-vps.sh pull"
  exit 0
fi

STAMP=$(date +%Y%m%d-%H%M%S)
OUT="$HOME/vps-backup/$STAMP"
mkdir -p "$OUT"/{data,config,systemd}
chmod 700 "$HOME/vps-backup" "$OUT"

echo
echo "═══ 4. 拉取到 $OUT ═══"
# One tar stream: read-only on the VPS, nothing is written there. Binaries are
# left out (rebuildable from git); .env is IN and holds live exchange keys.
if $SSH "tar czf - -C $DATA --exclude='./trading-*' ." > "$OUT/data/opt-trading.tgz" 2> "$OUT/data/tar.err"; then
  echo "  ✅ $DATA → data/opt-trading.tgz  ⚠️ 含 .env(交易所金鑰),絕對不要進版控"
else
  echo "  ⚠️  tar 有錯誤(常見:有檔案 ubuntu 讀不到),見 data/tar.err"
fi
# Never report success on a backup that lacks the one irreplaceable file.
if tar tzf "$OUT/data/opt-trading.tgz" 2>/dev/null | grep -qx './journal.csv'; then
  REMOTE_SUM=$($SSH "sha256sum $DATA/journal.csv" 2>/dev/null | cut -d' ' -f1)
  LOCAL_SUM=$(tar xzOf "$OUT/data/opt-trading.tgz" ./journal.csv | shasum -a 256 | cut -d' ' -f1)
  [ -n "$REMOTE_SUM" ] && [ "$REMOTE_SUM" = "$LOCAL_SUM" ] \
    && echo "  ✅ journal.csv 在備份裡,SHA-256 與 VPS 一致" \
    || echo "  🔴 journal.csv 的 SHA-256 與 VPS 不一致 —— 不要當作已備份"
else
  echo "  🔴 備份裡沒有 journal.csv —— 不要當作已備份"
fi
$SSH 'sudo -n cat /etc/systemd/system/trading-{bot,web,monitor}.service 2>/dev/null' > "$OUT/systemd/units.txt" 2>/dev/null \
  && [ -s "$OUT/systemd/units.txt" ] && echo "  ✅ systemd units" || echo "  ⚠️  systemd units 拉不到(sudo 不可用?手動補)"
$SSH 'crontab -l' > "$OUT/config/crontab.txt" 2>/dev/null
$SSH "ls -la $DATA; echo; systemctl list-units 'trading-*' --no-pager" > "$OUT/inventory.txt" 2>/dev/null
chmod -R go-rwx "$OUT"

echo
echo "═══ 5. 結果 ═══"
du -sh "$OUT"
find "$OUT" -type f | sed "s|$OUT/|  |" | head -30
echo
echo "🔴 還要手動做兩件事:"
echo "  1. 備份 SSH 私鑰 $KEY 到密碼管理器或外接碟"
echo "  2. 確認 $OUT 不在任何 git repo 裡(它在 ~/vps-backup,應該安全)"
