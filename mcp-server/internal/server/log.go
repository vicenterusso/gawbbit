package server

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Logger writes Python-logging-style lines:
// "2026-11-17 22:45:10,366 INFO service=rawbbit-mcp event=<message>".
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level int
}

var levels = map[string]int{
	"NOTSET": 0, "DEBUG": 10, "INFO": 20, "WARN": 30, "WARNING": 30,
	"ERROR": 40, "FATAL": 50, "CRITICAL": 50,
}

// NewLogger validates LOG_LEVEL like logging.basicConfig(level=...).
func NewLogger(out io.Writer, level string) (*Logger, error) {
	n, ok := levels[strings.ToUpper(level)]
	if !ok {
		return nil, fmt.Errorf("Unknown level: '%s'", strings.ToUpper(level))
	}
	return &Logger{out: out, level: n}, nil
}

func (l *Logger) write(level int, name, format string, args ...any) {
	if level < l.level {
		return
	}
	now := time.Now()
	line := fmt.Sprintf("%s,%03d %s service=rawbbit-mcp event=%s\n",
		now.Format("2006-01-02 15:04:05"), now.Nanosecond()/1e6, name, fmt.Sprintf(format, args...))
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.out, line)
}

// Info logs at INFO.
func (l *Logger) Info(format string, args ...any) { l.write(20, "INFO", format, args...) }

// Warning logs at WARNING.
func (l *Logger) Warning(format string, args ...any) { l.write(30, "WARNING", format, args...) }

// Error logs at ERROR.
func (l *Logger) Error(format string, args ...any) { l.write(40, "ERROR", format, args...) }

// Server writes uvicorn-style lines ("INFO:     ...") that are not subject
// to LOG_LEVEL, like uvicorn's own loggers.
func (l *Logger) Server(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = fmt.Fprintf(l.out, "INFO:     "+format+"\n", args...)
}
