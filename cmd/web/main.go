// Command web is the Gin-based UI for the trading-bot bot. Renders an analyze
// dashboard with click-to-copy price buttons; future phases add journal CRUD,
// validate forms, live logs, and charts.
//
// Bind via WEB_BIND env var (e.g. "100.x.x.x:8080" for Tailscale-only
// access on the VPS). Defaults to ":8080" (all interfaces) for local dev.
package main

import (
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
	"myFirstGo/trading-bot/indicator"
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
	log.Printf("[ai] provider=%s dry_run=%v", providerName, aiProvider.IsDryRun())
	if !aiProvider.IsDryRun() && ai.APIKeyForProvider(aiProvider) == "" {
		switch providerName {
		case "anthropic":
			log.Printf("[ai] ANTHROPIC_API_KEY not set — /ai/analyze endpoints will return an error. Set the env var or enable ANTHROPIC_DRY_RUN=true.")
		default:
			log.Printf("[ai] GEMINI_API_KEY not set — /ai/analyze endpoints will return an error. Set the env var or enable GEMINI_DRY_RUN=true.")
		}
	}
	srv := &server{
		client:         bxClient,
		ai:             aiProvider,
		aiProviderName: providerName,
		aiCache:        make(map[int]aiCacheEntry),
		aiSymbolCache:  make(map[string]aiCacheEntry),
	}
	r.GET("/", srv.handleDashboard)
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
	r.GET("/validate", srv.handleValidateForm)
	r.POST("/validate", srv.handleValidatePost)
	r.GET("/ops", srv.handleOpsPage)
	r.GET("/ops/status", srv.handleOpsStatus)
	r.GET("/ops/logs", srv.handleOpsLogs)
	r.POST("/ops/start", srv.handleOpsStart)
	r.POST("/ops/stop", srv.handleOpsStop)
	r.POST("/ops/restart", srv.handleOpsRestart)
	r.POST("/ops/config", srv.handleOpsConfig)
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok %s\n", time.Now().Format(time.RFC3339))
	})

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
	}
}
