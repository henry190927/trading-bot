// Command web is the Gin-based UI for the trading bot. Renders an analyze
// dashboard with click-to-copy price buttons; future phases add journal CRUD,
// validate forms, live logs, and charts.
//
// Bind via WEB_BIND env var (e.g. "100.x.x.x:8080" for Tailscale-only
// access on the VPS). Defaults to ":8080" (all interfaces) for local dev.
package main

import (
	"embed"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"

	"myFirstGo/trading/bingx"
	"myFirstGo/trading/config"
)

//go:embed templates/*.html static/*
var assets embed.FS

func main() {
	config.LoadDotEnv()

	bind := os.Getenv("WEB_BIND")
	if bind == "" {
		bind = ":8080"
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

	srv := &server{
		client: bingx.New(os.Getenv("BINGX_API_KEY"), os.Getenv("BINGX_API_SECRET")),
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
	r.GET("/validate", srv.handleValidateForm)
	r.POST("/validate", srv.handleValidatePost)
	r.GET("/health", func(c *gin.Context) {
		c.String(http.StatusOK, "ok %s\n", time.Now().Format(time.RFC3339))
	})

	log.Printf("trading-web listening on %s (Asia/Taipei)", bind)
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
