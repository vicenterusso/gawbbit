package main

import (
	"flag"
	"testing"
)

func TestApplyEnv(t *testing.T) {
	var s sendFlags
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	s.register(fs)
	t.Setenv("LOADGEN_API_KEY", "from-env")
	t.Setenv("LOADGEN_EVENTS", "7")
	t.Setenv("LOADGEN_APP_ID", "env-app")
	if err := parse(fs, []string{"-app-id", "flag-app"}); err != nil {
		t.Fatal(err)
	}
	if s.apiKey != "from-env" || s.events != 7 || s.appID != "flag-app" {
		t.Fatalf("got key=%q events=%d app=%q", s.apiKey, s.events, s.appID)
	}
	t.Setenv("LOADGEN_EVENTS", "many")
	fs2 := flag.NewFlagSet("t", flag.ContinueOnError)
	var s2 sendFlags
	s2.register(fs2)
	if err := parse(fs2, nil); err == nil {
		t.Fatal("expected an error for a bad LOADGEN_EVENTS")
	}
}

func TestEnvName(t *testing.T) {
	if got := envName("clickhouse-url"); got != "LOADGEN_CLICKHOUSE_URL" {
		t.Fatal(got)
	}
}
