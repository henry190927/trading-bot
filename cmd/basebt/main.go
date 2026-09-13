// Command basebt A/B-tests the "base-hold early entry" vs "wait-for-confirmation"
// entry on the SAME event: a dump into a key open (daily/weekly), price HOLDS
// above it (close back above), then continues up. EARLY = enter at the hold bar;
// LATE = enter only after a further +confirmMargin% close. Same SL below the
// open, TP = 2R. If EARLY netR > LATE robustly, entering the base beats waiting.
package main
import(
 "context";"flag";"fmt";"math";"os";"time"
 "github.com/henry190927/trading-bot/autotrade";"github.com/henry190927/trading-bot/bingx"
 "github.com/henry190927/trading-bot/config";"github.com/henry190927/trading-bot/indicator"
 "github.com/henry190927/trading-bot/market";"github.com/henry190927/trading-bot/signal"
)
func alignRight(a []float64,n int)[]float64{if len(a)==n{return a};o:=make([]float64,n);d:=n-len(a);for i:=range o{if i<d{if len(a)>0{o[i]=a[0]}}else{o[i]=a[i-d]}};return o}
type buk struct{n,tp,st int;r float64}
func(b *buk)add(o autotrade.Outcome){switch o.Status{case autotrade.OutTP:b.tp++;b.n++;b.r+=o.NetR;case autotrade.OutStop:b.st++;b.n++;b.r+=o.NetR}}
func(b buk)s(name string)string{w:=0.0;if b.n>0{w=float64(b.tp)/float64(b.n)*100};return fmt.Sprintf("  %-6s n=%-3d win=%3.0f%% netR %+7.2f",name,b.n,w,b.r)}
func main(){
 config.LoadDotEnv()
 days:=flag.Int("days",90,"window");flag.Parse()
 c:=bingx.New(os.Getenv("BINGX_API_KEY"),os.Getenv("BINGX_API_SECRET"))
 end:=time.Now().UTC();start:=end.AddDate(0,0,-*days)
 const(dropPct=0.008;dropLook=12;tolNear=0.003;confirmMargin=0.005;confirmWin=6;slATR=0.25;rMult=2.0)
 syms:=[]struct{n string;s market.Symbol}{{"BTC",market.BTCUSDT},{"ETH",market.ETHUSDT},{"SOL",market.SOLUSDT},{"SUI",market.SUIUSDT},{"NEAR",market.NEARUSDT},{"LINK",market.LINKUSDT}}
 fmt.Printf("=== base-hold EARLY vs LATE(confirm) · %dd · 1h ===\n",*days)
 var agE,agL buk
 for _,sm:=range syms{
  cs,err:=c.KlinesRange(context.Background(),sm.s,market.Timeframe("1h"),start,end)
  if err!=nil||len(cs)<200{continue}
  atr:=alignRight(indicator.ATR(cs,14),len(cs))
  var early,late []autotrade.PaperFire
  for i:=dropLook+2;i<len(cs)-1;i++{
   bar:=cs[i];a:=atr[i]
   op:=signal.ComputeOpens(cs,bar.CloseTime)
   // nearest of daily/weekly open below-ish the bar
   for _,O:=range []float64{op.Daily,op.Weekly}{
    if O<=0{continue}
    // near the open + held above it (close>O)
    if math.Abs(bar.Close-O)/O>tolNear || bar.Close<=O{continue}
    // recent dump INTO it: a high in last dropLook bars >= close*(1+dropPct)
    hi:=0.0;for j:=i-dropLook;j<i;j++{if cs[j].High>hi{hi=cs[j].High}}
    if hi<bar.Close*(1+dropPct){continue}
    stop:=O-slATR*a;risk:=bar.Close-stop
    if risk<=0{continue}
    // EARLY: enter at this hold bar close
    early=append(early,autotrade.PaperFire{Time:bar.CloseTime,Symbol:sm.n,TF:"1h",Strategy:"base-early",Side:"long",Market:true,Entry:bar.Close,Stop:stop,TP:bar.Close+rMult*risk})
    // LATE: first bar within confirmWin closing >= close*(1+confirmMargin)
    for k:=i+1;k<=i+confirmWin && k<len(cs);k++{
     if cs[k].Close>=bar.Close*(1+confirmMargin){
      e:=cs[k].Close;r2:=e-stop;if r2>0{late=append(late,autotrade.PaperFire{Time:cs[k].CloseTime,Symbol:sm.n,TF:"1h",Strategy:"base-late",Side:"long",Market:true,Entry:e,Stop:stop,TP:e+rMult*r2})}
      break
     }
    }
    break
   }
  }
  scoreE:=autotrade.DedupFires(early,6,6,time.Hour,func(f autotrade.PaperFire)autotrade.Outcome{return autotrade.EvaluateFire(f,cs,6)})
  scoreL:=autotrade.DedupFires(late,6,6,time.Hour,func(f autotrade.PaperFire)autotrade.Outcome{return autotrade.EvaluateFire(f,cs,6)})
  var e,l buk
  for _,p:=range scoreE{e.add(p.Outcome);agE.add(p.Outcome)}
  for _,p:=range scoreL{l.add(p.Outcome);agL.add(p.Outcome)}
  fmt.Printf("%s:\n%s\n%s\n",sm.n,e.s("EARLY"),l.s("LATE"))
 }
 fmt.Printf("--- AGG ---\n%s\n%s\n",agE.s("EARLY"),agL.s("LATE"))
}
