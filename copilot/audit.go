package copilot

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// AuditLog appends one JSON line per tool call (design §7): what the model
// asked for, with which arguments, how long it took, how much came back.
type AuditLog struct {
	path string
	mu   sync.Mutex
	now  func() time.Time
}

// NewAuditLog appends to path. An empty path disables the log.
func NewAuditLog(path string) *AuditLog {
	return &AuditLog{path: path, now: time.Now}
}

type auditLine struct {
	Time time.Time `json:"time"`
	ToolTrace
}

// Write records one tool call. Best effort: a failed write is logged and
// never blocks the chat.
func (l *AuditLog) Write(tr ToolTrace) {
	if l == nil || l.path == "" {
		return
	}
	b, err := json.Marshal(auditLine{Time: l.now().UTC(), ToolTrace: tr})
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o664)
	if err != nil {
		log.Printf("copilot: audit open: %v", err)
		return
	}
	_, werr := f.Write(append(b, '\n'))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		log.Printf("copilot: audit write: %v", werr)
	}
}
