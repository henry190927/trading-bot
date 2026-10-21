"""Gate every unarmed (symbol x strategy) cell of the live-setups grid.

Stage 1: 60/90/120-day windows through the strategy's own A/B harness, judged
by cmd/gate on absolute grounds (a new rule has no baseline).
Stage 2: cells that PASS get four DISJOINT 30-day buckets (end-offset 0/30/60/90)
and are gated again with --buckets, which downgrades a one-month wonder.

Usage: build the harnesses into one directory, then point BIN at it:
    mkdir -p /tmp/grid/bin && for c in sweepbt rangebt snrbt backtest gate; do
        go build -o /tmp/grid/bin/$c ./cmd/$c; done
    BIN=/tmp/grid/bin python3 scripts/gate_grid.py
CONFIGURED must mirror autotrade.json's rules (enabled or not) at run time.
Results land in $BIN/../grid_results.json. See docs/open-and-rejected.md for
the 2026-10-21 run.
"""
import json, re, subprocess, sys, os
B = os.environ.get("BIN") or os.path.dirname(os.path.abspath(__file__)) + "/bin"
CODES = {"BTC":"BTC-USDT","ETH":"ETH-USDT","XAU":"NCCOGOLD2USD-USDT","XAG":"NCCOXAG2USD-USDT","SNDK":"NCSKSNDK2USD-USDT",
 "NVDA":"NCSKNVDA2USD-USDT","SPCX":"NCSKSPCX2USD-USDT","MSTR":"NCSKMSTR2USD-USDT","APP":"NCSKAPP2USD-USDT","SOL":"SOL-USDT",
 "LINK":"LINK-USDT","SUI":"SUI-USDT","HYPE":"HYPE-USDT","NEAR":"NEAR-USDT","XRP":"XRP-USDT","AKE":"AKE-USDT","UNI":"UNI-USDT"}
CONFIGURED = {("BTC","range-edge"),("ETH","range-edge"),("HYPE","engine"),("BTC","engine"),("ETH","engine"),("SOL","engine"),
 ("SOL","sweep-reject"),("ETH","sweep-reject"),("XAG","engine"),("SUI","sweep-reject"),("NEAR","engine"),("ETH","htf-snr"),
 ("SNDK","engine"),("NVDA","engine"),("SPCX","engine"),("MSTR","engine"),("APP","sweep-reject")}
STRATS = ["range-edge","engine","sweep-reject","htf-snr"]

def run(strat, sym, days, off=0):
    # A transient API failure (rate limit, timeout) reads as no data; retry
    # twice before believing it.
    import time
    for attempt in range(3):
        r=_run(strat, sym, days, off)
        if r is not None:
            return r
        time.sleep(5)
    return None

def _run(strat, sym, days, off=0):
    if strat == "sweep-reject":
        cmd=[B+"/sweepbt","-days",str(days),"-end-offset",str(off),"-symbols",sym]
    elif strat == "htf-snr":
        cmd=[B+"/snrbt","-days",str(days),"-end-offset",str(off),"-symbols",sym]
    elif strat == "range-edge":
        cmd=[B+"/rangebt","-days",str(days),"-end-offset",str(off),"-symbols",sym]
    else:
        cmd=[B+"/backtest","-days",str(days),"-end-offset",str(off),"-symbol",CODES[sym]]
    out = subprocess.run(cmd, capture_output=True, text=True, timeout=900).stdout
    if strat in ("sweep-reject","htf-snr"):
        for ln in out.splitlines():
            f=ln.split()
            if f and f[0]==sym and len(f)>=7:
                return float(f[6]), int(f[1])
    elif strat == "range-edge":
        for ln in out.splitlines():
            f=ln.split()
            if len(f)>=8 and f[0]==sym and f[1]=="V0-baseline":
                return float(f[-1]), int(f[2])
    else:
        m=re.search(r"(\d+) filled .*?netR=([+-]?[\d.]+)", out)
        if m: return float(m.group(2)), int(m.group(1))
    return None

def gate(name, arm, buckets=None):
    a=", ".join("%d %.2f %d"%(d,r,n) for d,r,n in arm)
    cmd=[B+"/gate","--arm",a,"--name",name]
    if buckets: cmd+=["--buckets",", ".join("%d %.2f %d"%(o,r,n) for o,r,n in buckets)]
    out=subprocess.run(cmd,capture_output=True,text=True).stdout
    # The verdict is the first word of the first line; the body mentions
    # PASS/FAIL elsewhere too, so searching the text misreads it.
    first=(out.split() or ["?"])[0]
    return first, out.strip()

res=[]
cells=[(s,st) for s in CODES for st in STRATS if (s,st) not in CONFIGURED]
print("cells:",len(cells),flush=True)
for s,st in cells:
    arm=[]
    for d in (60,90,120):
        try: r=run(st,s,d)
        except Exception as e: r=None; print("ERR",s,st,d,e,flush=True)
        if r is None: break
        arm.append((d,r[0],r[1]))
    if len(arm)<3:
        res.append({"sym":s,"strat":st,"verdict":"NO-DATA","arm":arm}); print(s,st,"NO-DATA",flush=True); continue
    v,out=gate("%s %s"%(s,st),arm)
    rec={"sym":s,"strat":st,"verdict":v,"arm":arm,"gate":out}
    if v=="PASS":
        bk=[]
        for off in (0,30,60,90):
            r=run(st,s,30,off)
            if r: bk.append((off,r[0],r[1]))
        v2,out2=gate("%s %s"%(s,st),arm,bk)
        rec.update({"buckets":bk,"verdict2":v2,"gate2":out2})
    res.append(rec)
    print(s,st,v,rec.get("verdict2",""),arm,flush=True)
    json.dump(res,open(os.path.dirname(B)+"/grid_results.json","w"),indent=1)
print("DONE",flush=True)
