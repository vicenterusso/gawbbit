// Package logx writes log lines in the format the Python service used:
//
//	2026-03-20 18:42:15,123 INFO service=collector-api event=<message>
package logx

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Python logging level numbers.
const (
	LevelDebug    = 10
	LevelInfo     = 20
	LevelWarning  = 30
	LevelError    = 40
	LevelCritical = 50
)

// ParseLevel accepts the names Python's logging.basicConfig(level=...) does
// after str.upper().
func ParseLevel(name string) (int, error) {
	switch strings.ToUpper(name) {
	case "CRITICAL", "FATAL":
		return LevelCritical, nil
	case "ERROR":
		return LevelError, nil
	case "WARNING", "WARN":
		return LevelWarning, nil
	case "INFO":
		return LevelInfo, nil
	case "DEBUG":
		return LevelDebug, nil
	case "NOTSET":
		return 0, nil
	}
	return 0, fmt.Errorf("Unknown level: '%s'", strings.ToUpper(name))
}

// Logger is safe for concurrent use.
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level int
	now   func() time.Time
}

// New returns a logger writing records at or above level to out.
func New(out io.Writer, level int) *Logger {
	return &Logger{out: out, level: level, now: time.Now}
}

func (l *Logger) log(level int, name, msg string) {
	if level < l.level {
		return
	}
	t := l.now()
	line := fmt.Sprintf("%s,%03d %s service=collector-api event=%s\n",
		t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1_000_000, name, msg)
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.out, line)
}

// Debug logs at DEBUG.
func (l *Logger) Debug(format string, args ...any) {
	l.log(LevelDebug, "DEBUG", fmt.Sprintf(format, args...))
}

// Info logs at INFO.
func (l *Logger) Info(format string, args ...any) {
	l.log(LevelInfo, "INFO", fmt.Sprintf(format, args...))
}

// Warn logs at WARNING.
func (l *Logger) Warn(format string, args ...any) {
	l.log(LevelWarning, "WARNING", fmt.Sprintf(format, args...))
}

// Error logs at ERROR.
func (l *Logger) Error(format string, args ...any) {
	l.log(LevelError, "ERROR", fmt.Sprintf(format, args...))
}
