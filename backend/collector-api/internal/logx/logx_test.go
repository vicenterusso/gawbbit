package logx

import (
	"bytes"
	"testing"
	"time"
)

func TestFormatMatchesPythonLogging(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, LevelInfo)
	l.now = func() time.Time { return time.Date(2026, 3, 20, 18, 42, 15, 123456789, time.Local) }
	l.Info("startup env=%s", "local")
	l.Warn("request_rejected reason=%s", "x")
	l.Error("publish_failed")
	want := "2026-03-20 18:42:15,123 INFO service=collector-api event=startup env=local\n" +
		"2026-03-20 18:42:15,123 WARNING service=collector-api event=request_rejected reason=x\n" +
		"2026-03-20 18:42:15,123 ERROR service=collector-api event=publish_failed\n"
	if buf.String() != want {
		t.Fatalf("%q", buf.String())
	}
	buf.Reset()
	New(&buf, LevelWarning).Info("hidden")
	if buf.Len() != 0 {
		t.Fatal("INFO written at WARNING level")
	}
}

func TestParseLevel(t *testing.T) {
	for name, want := range map[string]int{"info": 20, "Warn": 30, "WARNING": 30, "debug": 10, "fatal": 50, "notset": 0, "error": 40} {
		if got, err := ParseLevel(name); err != nil || got != want {
			t.Errorf("%s: %d %v", name, got, err)
		}
	}
	if _, err := ParseLevel("loud"); err == nil || err.Error() != "Unknown level: 'LOUD'" {
		t.Errorf("%v", err)
	}
}
