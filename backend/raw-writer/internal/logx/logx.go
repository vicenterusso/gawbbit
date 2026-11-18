// Package logx writes log lines in the format the Python service used:
//
//	2026-01-01 10:00:00,123 INFO service=raw-writer event=<message>
//
// to stderr, filtered by a Python logging level name.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Level is a Python logging level number.
type Level int

// Python logging levels.
const (
	NotSet   Level = 0
	Debug    Level = 10
	Info     Level = 20
	Warning  Level = 30
	Error    Level = 40
	Critical Level = 50
)

var levelNames = map[Level]string{Debug: "DEBUG", Info: "INFO", Warning: "WARNING", Error: "ERROR", Critical: "CRITICAL"}

// ParseLevel maps LOG_LEVEL (upper-cased) like logging.basicConfig(level=...).
func ParseLevel(name string) (Level, error) {
	switch strings.ToUpper(name) {
	case "CRITICAL", "FATAL":
		return Critical, nil
	case "ERROR":
		return Error, nil
	case "WARNING", "WARN":
		return Warning, nil
	case "INFO":
		return Info, nil
	case "DEBUG":
		return Debug, nil
	case "NOTSET":
		return NotSet, nil
	}
	return 0, fmt.Errorf("Unknown level: '%s'", strings.ToUpper(name))
}

// Logger is safe for concurrent use.
type Logger struct {
	mu    sync.Mutex
	out   io.Writer
	level Level
	now   func() time.Time
}

// New returns a logger writing records at or above level to out.
func New(out io.Writer, level Level) *Logger {
	return &Logger{out: out, level: level, now: time.Now}
}

// Std is the process logger; main configures its level.
var Std = New(os.Stderr, Info)

// SetLevel changes the minimum level.
func (l *Logger) SetLevel(level Level) {
	l.mu.Lock()
	l.level = level
	l.mu.Unlock()
}

// Enabled reports whether records at level are written.
func (l *Logger) Enabled(level Level) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return level >= l.level
}

// Log writes one record; format and args follow fmt, and values should already be
// rendered Python-style (see Py).
func (l *Logger) Log(level Level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if level < l.level {
		return
	}
	ts := l.now().Format("2006-01-02 15:04:05.000")
	ts = ts[:19] + "," + ts[20:]
	fmt.Fprintf(l.out, "%s %s service=raw-writer event=%s\n", ts, levelNames[level], fmt.Sprintf(format, args...))
}

// Debugf logs at DEBUG.
func (l *Logger) Debugf(format string, args ...any) { l.Log(Debug, format, args...) }

// Infof logs at INFO.
func (l *Logger) Infof(format string, args ...any) { l.Log(Info, format, args...) }

// Warningf logs at WARNING.
func (l *Logger) Warningf(format string, args ...any) { l.Log(Warning, format, args...) }

// Errorf logs at ERROR.
func (l *Logger) Errorf(format string, args ...any) { l.Log(Error, format, args...) }

// Py renders a value the way Python's %s does for the types logged here: nil and nil
// pointers as None, booleans as True/False.
func Py(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case *int:
		if x == nil {
			return "None"
		}
		return fmt.Sprint(*x)
	case *uint64:
		if x == nil {
			return "None"
		}
		return fmt.Sprint(*x)
	}
	return fmt.Sprint(v)
}
