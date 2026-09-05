// Command web is the Gin-based UI for the trading-bot bot. Renders an analyze
// dashboard with click-to-copy price buttons; future phases add journal CRUD,
// validate forms, live logs, and charts.
//
// Bind via WEB_BIND env var (e.g. "100.x.x.x:8080" for Tailscale-only
// access on the VPS). Defaults to ":8080" (all interfaces) for local dev.
package main

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading-bot/ai"
	"myFirstGo/trading-bot/bingx"
	"myFirstGo/trading-bot/config"
	"myFirstGo/trading-bot/earnings"
	"myFirstGo/trading-bot/econcal"
	"myFirstGo/trading-bot/indicator"
	"myFirstGo/trading-bot/macro"
	"myFirstGo/trading-bot/onchain"
)

//go:embed templates/*.html static
var assets embed.FS

func main() {
	config.LoadDotEnv()

	bind := os.Getenv("WEB_BIND")
	if bind == "" {
		bind = ":8080"
	}

	// VOL_PROFILE_BODY_WEIGHT (e.g. "0.7") routes that fraction of each
	// candle's volume into its body range when building POC/HVN. Default
	// empty/0 = legacy uniform-over-HL. Surfaced as env var for live A/B
	// since the backtest can't measure it (POC/HVN don't drive trade
	// decisions, only the dashboard's diagnose row).
	if v := os.Getenv("VOL_PROFILE_BODY_WEIGHT"); v != "" {
		var bw float64
		if _, err := fmt.Sscanf(v, "%f", &bw); err == nil && bw > 0 && bw < 1 {
			indicator.BodyWeight = bw
			log.Printf("volume profile body-weighting enabled: %.2f", bw)
		}
	}

	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(requestLogger())
	// Static assets are embedded (go:embed) → zero modtime → http.FileServer
	// emits no Last-Modified/ETag, so browsers heuristic-cache style.css and
	// miss deploys. Force revalidation so CSS/JS changes always show up.
	r.Use(func(c *gin.Context) {
		p := c.Request.URL.Path
		if len(p) >= 8 && p[:8] == "/static/" {
			c.Header("Cache-Control", "no-cache")
		}
		c.Next()
	})

	// Embedded templates + static assets so the binary is self-contained.
	tmpl := template.Must(template.New("").Funcs(templateFuncs()).ParseFS(assets, "templates/*.html"))
	r.SetHTMLTemplate(tmpl)
	staticFS, err := fs.Sub(assets, "static")
	if err != nil {
		log.Fatalf("static fs: %v", err)
	}
	r.StaticFS("/static", http.FS(staticFS))

	bxClient := bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET"))
	if os.Getenv("BINGX_DRY_RUN") == "true" {
		bxClient.DryRun = true
		log.Printf("[BINGX_DRY_RUN=true] all write endpoints will log + return mock orderIds — no real orders sent")
	}
	aiProvider, providerName := ai.NewProvider()
	// If provider is Gemini, load any persisted model choice from disk.
	// The UI's /api/ai/model POST writes here after each admin change.
	const aiModelPath = "/opt/trading/ai_model.txt"
	if gc, ok := aiProvider.(*ai.GeminiClient); ok {
		gc.LoadPersistedModel(aiModelPath)
		log.Printf("[ai] gemini active model: %s", gc.CurrentModel())
	}
	log.Printf("[ai] provider=%s dry_run=%v", providerName, aiProvider.IsDryRun())
	if !aiProvider.IsDryRun() && ai.APIKeyForProvider(aiProvider) == "" {
		switch providerName {
		case "anthropic":
			log.Printf("[ai] ANTHROPIC_API_KEY not set — /ai/analyze endpoints will return an error. Set the env var or enable ANTHROPIC_DRY_RUN=true.")
		default:
			log.Printf("[ai] GEMINI_API_KEY not set — /ai/analyze endpoints will return an error. Set the env var or enable GEMINI_DRY_RUN=true.")
		}
	}
	onchainSvc := onchain.NewService()
	if count, _, source, _ := onchainSvc.CEX.Stats(); count > 0 {
		log.Printf("[onchain] CEX registry seeded from %s: %d addresses", source, count)
	}
	srv := &server{
		client:         bxClient,
		ai:             aiProvider,
		aiProviderName: providerName,
		aiModelPath:    aiModelPath,
		aiCache:        make(map[int]aiCacheEntry),
		aiSymbolCache:  make(map[string]aiCacheEntry),
		onchain:        onchainSvc,
	}
	// The hub's only dependency is "give me a snapshot" — the same builder
	// /api/tickers serves, so both transports carry identical numbers.
	srv.hub = newTickerHub(func(ctx context.Context) any { return srv.buildTickers(ctx) })
	r.GET("/", srv.handleTodayPage)      // home = Today (symbols overview)
	r.GET("/today", srv.handleTodayPage) // kept for existing links/bookmarks
	r.GET("/scan", srv.handleDashboard)  // Scan moved off "/"
	r.GET("/journal", srv.handleJournalList)
	r.GET("/journal/new", srv.handleJournalNew)
	r.POST("/journal/open", srv.handleJournalOpen)
	r.GET("/journal/:id/close", srv.handleJournalCloseForm)
	r.POST("/journal/:id/close", srv.handleJournalClosePost)
	r.GET("/journal/:id/edit", srv.handleJournalEditForm)
	r.POST("/journal/:id/edit", srv.handleJournalEditPost)
	r.POST("/journal/:id/delete", srv.handleJournalDelete)
	r.POST("/journal/:id/unwind", srv.handleJournalUnwind)
	r.POST("/ai/analyze/:id", srv.handleAIAnalyzeTrade)
	r.POST("/ai/analyze/symbol/:short/:tf", srv.handleAIAnalyzeSymbol)
	r.POST("/ai/analyze/validate", srv.handleAIAnalyzeValidate)
	r.GET("/api/ai/model", srv.handleAIModelGet)
	r.POST("/api/ai/model", srv.handleAIModelPost)
	r.GET("/validate", srv.handleValidateForm)
	r.POST("/validate", srv.handleValidatePost)
	// JSON APIs used by the /chart quick-action modal so validate +
	// record can happen inline without navigating away.
	r.POST("/api/chart/validate", srv.handleAPIChartValidate)
	r.POST("/api/chart/journal-open", srv.handleAPIChartJournalOpen)
	// Per-user layer/color preferences persisted server-side so a
	// fresh device (iPhone, incog window) inherits the same
	// most-used config instead of the DEFAULT_LAYERS baseline.
	r.GET("/api/chart/state", srv.handleAPIChartStateGet)
	r.POST("/api/chart/state", srv.handleAPIChartStatePost)
	r.POST("/api/chart/setup", srv.handleAPIChartSetupRecord)
	r.GET("/setups", srv.handleSetupsList)
	r.GET("/tips", srv.handleTipsPage)
	r.GET("/fundamentals", srv.handleFundamentalsPage)
	r.GET("/api/stock/news", srv.handleStockNews)
	r.GET("/tw", srv.handleTWStocksPage)
	r.GET("/calendar", srv.handleCalendarPage)
	r.POST("/api/setups/refresh", srv.handleAPISetupsRefresh)
	r.POST("/setups/:id/skip", srv.handleSetupSkip)
	r.POST("/setups/:id/delete", srv.handleSetupDelete)
	r.GET("/onchain", srv.handleOnchainPage)
	r.GET("/api/onchain/lookup", srv.handleOnchainLookup)
	r.GET("/chart", srv.handleChartPage)
	r.GET("/api/chart/data", srv.handleChartData)
	r.GET("/api/chart/bias", srv.handleChartBias)
	r.GET("/api/tickers", srv.handleTickers)
	r.GET("/api/stream/tickers", srv.handleStreamTickers)
	r.GET("/ops", srv.handleOpsPage)
	r.GET("/ops/scan", srv.handleScanSetups)
	r.POST("/scan/record", srv.handleScanRecord)
	r.GET("/ops/verify", srv.handleVerifyExchange)
	// B2: attach a reduce-only stop/tp to a live position without SSH.
	// POST-only and two-step (preview unless confirm=1) — see protect_handler.go.
	r.POST("/ops/protect", srv.handleOpsProtect)
	r.GET("/ops/autotrade", srv.handleOpsAutotrade)
	r.GET("/ops/status", srv.handleOpsStatus)
	r.GET("/ops/services", srv.handleOpsServices)
	r.POST("/ops/ntfy", srv.handleOpsNtfy)
	r.GET("/ops/zones", srv.handleOpsZones)
	r.POST("/ops/zone-config", srv.handleOpsZoneConfig)
	r.GET("/ops/zone-logs", srv.handleOpsZoneLogs)
	r.GET("/ops/logs", srv.handleOpsLogs)
	r.POST("/ops/start", srv.handleOpsStart)
	r.POST("/ops/stop", srv.handleOpsStop)
	r.POST("/ops/restart", srv.handleOpsRestart)
	r.POST("/ops/config", srv.handleOpsConfig)
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok %s\n", time.Now().Format(time.RFC3339))
	})

	// Load the earnings-blackout calendar (F3) and hot-reload it every 10min so
	// the daily cron-written earnings.json is picked up without a restart. A
	// missing file just leaves the gate a no-op — never fatal.
	earnings.LoadDefaultAndWatch(context.Background(), earnings.Path(), 10*time.Minute, log.Printf)
	// Ad-hoc blackouts (a Fed speech announced days ahead) without a rebuild.
	// 1 minute, not 10 like earnings: an earnings date is known weeks out, but
	// the overlay exists precisely for something you are adding minutes before
	// it matters.
	macro.LoadOverlayAndWatch(context.Background(), macro.OverlayPath(), time.Minute, log.Printf)

	// Economic-data-release calendar (ForexFactory feed) — display layer for
	// /calendar, refreshed hourly. Never fatal: feed down = calendar minus this row.
	econcal.LoadAndWatch(context.Background(), time.Hour, log.Printf)

	log.Printf("trading-bot-web listening on %s (Asia/Taipei)", bind)
	if err := r.Run(bind); err != nil {
		log.Fatalf("server: %v", err)
	}
}

// requestLogger logs each request in a compact format compatible with
// journalctl filtering. Skips /static/* to keep the log readable.
func requestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		if len(c.Request.URL.Path) >= 8 && c.Request.URL.Path[:8] == "/static/" {
			return
		}
		log.Printf("%s %s %d %s", c.Request.Method, c.Request.URL.Path,
			c.Writer.Status(), time.Since(start))
		// Surface handler/render errors (gin appends template-execution failures to
		// c.Errors) — otherwise a template error silently truncates the page with a
		// 200 and no log line.
		for _, e := range c.Errors {
			log.Printf("  ⚠ %s %s: %v", c.Request.Method, c.Request.URL.Path, e.Err)
		}
	}
}
