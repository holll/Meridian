package internal

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// OpenAccessLog opens (or creates) a log file for HTTP request logging.
// Returns a multi-writer that writes to both the file and stdout.
// The caller must close the returned file on shutdown.
func OpenAccessLog(path string) (*os.File, io.Writer, error) {
	if path == "" {
		return nil, os.Stdout, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) // #nosec G304 -- path comes from ACCESS_LOG env var, admin-controlled
	if err != nil {
		return nil, nil, fmt.Errorf("open access log %s: %w", path, err)
	}
	return f, io.MultiWriter(f, os.Stdout), nil
}

// skipAccessLog lists path prefixes that should not be logged (long-lived connections).
var skipAccessLog = []string{"/api/events"}

// RequestLogger returns a Gin middleware that writes one log line per request
// in a common format: timestamp, client IP, method, path, status, latency.
func RequestLogger(out io.Writer) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		for _, skip := range skipAccessLog {
			if strings.HasPrefix(path, skip) {
				c.Next()
				return
			}
		}

		start := time.Now()
		c.Next()
		latency := time.Since(start)

		fmt.Fprintf(out, "%s | %3d | %12s | %-15s | %-7s %s\n",
			start.Format("2006/01/02 15:04:05"),
			c.Writer.Status(),
			latency.Truncate(time.Microsecond),
			c.ClientIP(),
			c.Request.Method,
			path,
		)
	}
}
