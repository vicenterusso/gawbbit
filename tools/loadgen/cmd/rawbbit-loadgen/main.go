// rawbbit-loadgen sends synthetic game events to a Rawbbit collector and
// verifies, through ClickHouse or the MCP server, that every accepted event
// was stored exactly once and that rejected requests stored nothing.
//
//	rawbbit-loadgen send   -collector URL -api-key KEY -app-id APP [-events N ...]
//	rawbbit-loadgen verify -manifest FILE (-mcp-url URL -mcp-token T | -clickhouse-url URL ...)
//	rawbbit-loadgen run    (send, then verify)
//
// Every flag can also be set with an environment variable: -api-key is
// LOADGEN_API_KEY, -mcp-token is LOADGEN_MCP_TOKEN, and so on.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mirlan-irokez/rawbbit/tools/loadgen/internal/loadgen"
)

const usage = `usage: rawbbit-loadgen <send|verify|run> [flags]

  send    send events (and the rejection checks) and write a manifest
  verify  wait until ClickHouse holds exactly the events of a manifest
  run     send, then verify

Run "rawbbit-loadgen <command> -h" for the flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "send":
		err = cmdSend(ctx, os.Args[2:])
	case "verify":
		err = cmdVerify(ctx, os.Args[2:])
	case "run":
		err = cmdRun(ctx, os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "loadgen_failed:", err)
		os.Exit(1)
	}
}

// envName maps a flag name to its environment variable.
func envName(flagName string) string {
	return "LOADGEN_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(flagName))
}

// applyEnv sets every flag not given on the command line from LOADGEN_*.
func applyEnv(fs *flag.FlagSet) error {
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if given[f.Name] || err != nil {
			return
		}
		if v, ok := os.LookupEnv(envName(f.Name)); ok {
			if e := fs.Set(f.Name, v); e != nil {
				err = fmt.Errorf("%s: %w", envName(f.Name), e)
			}
		}
	})
	return err
}

type sendFlags struct {
	collector, host, apiKey, appID, environment, runID, manifest string
	events, batch, concurrency, players, maxEvents, retries      int
	rate, duplicates                                             float64
	negative, insecure                                           bool
	timeout                                                      time.Duration
	seed                                                         uint64
}

func (s *sendFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&s.collector, "collector", "http://127.0.0.1:8080", "collector base URL")
	fs.StringVar(&s.host, "collector-host", "", "Host header for the collector (reach Caddy by IP)")
	fs.StringVar(&s.apiKey, "api-key", "", "collector API key (X-API-Key)")
	fs.StringVar(&s.appID, "app-id", "", "app_id the API key is allowed to send")
	fs.StringVar(&s.environment, "environment", "loadgen", "environment of the generated events")
	fs.StringVar(&s.runID, "run-id", "", "run tag stored in event_params.loadgen_run (default random)")
	fs.StringVar(&s.manifest, "manifest", "loadgen-manifest.json", "where to write the manifest")
	fs.IntVar(&s.events, "events", 1000, "distinct events to send")
	fs.IntVar(&s.batch, "batch", 50, "events per request")
	fs.IntVar(&s.concurrency, "concurrency", 4, "parallel requests")
	fs.IntVar(&s.players, "players", 50, "synthetic players (user_pseudo_id)")
	fs.IntVar(&s.maxEvents, "max-events-per-request", 500, "collector MAX_EVENTS_PER_REQUEST, for the 413 check (0 skips it)")
	fs.IntVar(&s.retries, "retries", 5, "retries per batch on 5xx or network errors")
	fs.Float64Var(&s.rate, "rate", 200, "events per second (0 = as fast as possible)")
	fs.Float64Var(&s.duplicates, "duplicates", 0.05, "fraction of batches sent twice (deduplication check)")
	fs.BoolVar(&s.negative, "negative", true, "also send requests the collector must reject")
	fs.BoolVar(&s.insecure, "insecure", false, "skip TLS verification")
	fs.DurationVar(&s.timeout, "timeout", 30*time.Second, "per-request timeout")
	fs.Uint64Var(&s.seed, "seed", 0, "random seed (0 = random)")
}

type verifyFlags struct {
	manifest, mcpURL, mcpToken, mcpHost, chURL, chUser, chPassword, chHost, table, loadCmd string
	timeout, interval                                                                      time.Duration
	insecure                                                                               bool
}

func (v *verifyFlags) register(fs *flag.FlagSet, withManifest bool) {
	if withManifest {
		fs.StringVar(&v.manifest, "manifest", "loadgen-manifest.json", "manifest written by send")
	}
	fs.StringVar(&v.mcpURL, "mcp-url", "", "MCP endpoint (verifies through run_readonly_sql)")
	fs.StringVar(&v.mcpToken, "mcp-token", "", "MCP bearer token")
	fs.StringVar(&v.mcpHost, "mcp-host", "", "Host header for the MCP server")
	fs.StringVar(&v.chURL, "clickhouse-url", "", "ClickHouse HTTP URL (alternative to -mcp-url)")
	fs.StringVar(&v.chUser, "clickhouse-user", "", "ClickHouse user")
	fs.StringVar(&v.chPassword, "clickhouse-password", "", "ClickHouse password")
	fs.StringVar(&v.chHost, "clickhouse-host", "", "Host header for ClickHouse")
	fs.StringVar(&v.table, "table", "analytics.events", "events table")
	fs.StringVar(&v.loadCmd, "load-cmd", "", "shell command run before each check, e.g. a runner backfill; "+
		"$LOADGEN_WINDOW_START and $LOADGEN_WINDOW_END hold the window")
	fs.DurationVar(&v.timeout, "verify-timeout", 10*time.Minute, "give up after this long")
	fs.DurationVar(&v.interval, "verify-interval", 20*time.Second, "time between checks")
	if withManifest {
		fs.BoolVar(&v.insecure, "insecure", false, "skip TLS verification")
	}
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return applyEnv(fs)
}

func printJSON(label string, v any) {
	b, _ := json.Marshal(v)
	fmt.Printf("%s %s\n", label, b)
}

func cmdSend(ctx context.Context, args []string) error {
	var s sendFlags
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	s.register(fs)
	if err := parse(fs, args); err != nil {
		return err
	}
	_, err := doSend(ctx, s)
	return err
}

func doSend(ctx context.Context, s sendFlags) (loadgen.Manifest, error) {
	if s.apiKey == "" || s.appID == "" {
		return loadgen.Manifest{}, errors.New("-api-key and -app-id are required")
	}
	if s.runID == "" {
		s.runID = loadgen.NewRunID()
	}
	client := loadgen.NewClient(s.collector, s.host, s.apiKey, s.timeout, s.insecure)
	hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	for {
		err := client.Health(hctx)
		if err == nil {
			break
		}
		if hctx.Err() != nil {
			return loadgen.Manifest{}, fmt.Errorf("collector not healthy: %w", err)
		}
		time.Sleep(2 * time.Second)
	}
	fmt.Printf("loadgen_send_started run_id=%s events=%d batch=%d rate=%g concurrency=%d\n",
		s.runID, s.events, s.batch, s.rate, s.concurrency)

	var failures []string
	if s.negative {
		cases := loadgen.RunNegative(ctx, client, s.apiKey, s.runID, s.appID, s.environment, s.maxEvents)
		for _, c := range cases {
			status := "ok"
			if !c.OK() {
				status = "FAILED"
				failures = append(failures, fmt.Sprintf("%s: want %d, got %d %s", c.Name, c.WantStatus, c.Got, c.Err))
			}
			fmt.Printf("loadgen_negative %-28q want=%d got=%d %s\n", c.Name, c.WantStatus, c.Got, status)
		}
	}

	res, err := loadgen.Send(ctx, loadgen.SendConfig{
		CollectorURL: s.collector, HostHeader: s.host, APIKey: s.apiKey, AppID: s.appID,
		Environment: s.environment, RunID: s.runID, Events: s.events, BatchSize: s.batch,
		Rate: s.rate, Concurrency: s.concurrency, Players: s.players, Duplicates: s.duplicates,
		Seed: s.seed, MaxRetries: s.retries, Timeout: s.timeout, Insecure: s.insecure,
	})
	if err != nil {
		return res.Manifest, err
	}
	if err := loadgen.WriteManifest(s.manifest, res.Manifest); err != nil {
		return res.Manifest, err
	}
	rate := 0.0
	if res.Elapsed > 0 {
		rate = float64(res.Manifest.Total) / res.Elapsed.Seconds()
	}
	printJSON("loadgen_send_summary", map[string]any{
		"run_id": s.runID, "accepted_events": res.Manifest.Total, "failed_events": res.Failed,
		"requests": res.Requests, "retries": res.Retries, "duplicate_batches": res.DuplicateSends,
		"elapsed_s": round(res.Elapsed.Seconds()), "events_per_s": round(rate),
		"latency_ms": map[string]float64{
			"p50": ms(res.LatencyP50), "p95": ms(res.LatencyP95), "p99": ms(res.LatencyP99), "max": ms(res.LatencyMax),
		},
		"window_start": res.Manifest.WindowStart, "window_end": res.Manifest.WindowEnd,
		"expected": res.Manifest.Expected, "manifest": s.manifest,
	})
	if res.Failed > 0 {
		failures = append(failures, fmt.Sprintf("%d events were never accepted", res.Failed))
	}
	if len(failures) > 0 {
		return res.Manifest, errors.New(strings.Join(failures, "; "))
	}
	return res.Manifest, nil
}

func cmdVerify(ctx context.Context, args []string) error {
	var v verifyFlags
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	v.register(fs, true)
	if err := parse(fs, args); err != nil {
		return err
	}
	m, err := loadgen.ReadManifest(v.manifest)
	if err != nil {
		return err
	}
	return doVerify(ctx, v, m)
}

func doVerify(ctx context.Context, v verifyFlags, m loadgen.Manifest) error {
	var q loadgen.Querier
	switch {
	case v.mcpURL != "":
		q = loadgen.NewMCP(loadgen.MCPURL(v.mcpURL, "/mcp"), v.mcpToken, v.mcpHost, v.insecure)
	case v.chURL != "":
		q = loadgen.NewClickHouse(v.chURL, v.chUser, v.chPassword, v.chHost, v.insecure)
	default:
		return errors.New("set -mcp-url or -clickhouse-url")
	}
	sql, err := loadgen.VerifySQL(v.table, m)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(v.timeout)
	var last []string
	for attempt := 1; ; attempt++ {
		if v.loadCmd != "" {
			cmd := exec.CommandContext(ctx, "sh", "-c", v.loadCmd)
			cmd.Env = append(os.Environ(),
				"LOADGEN_WINDOW_START="+m.WindowStart.UTC().Format(time.RFC3339),
				"LOADGEN_WINDOW_END="+m.WindowEnd.UTC().Format(time.RFC3339))
			out, err := cmd.CombinedOutput()
			if err != nil {
				fmt.Printf("loadgen_load_cmd_failed attempt=%d err=%v\n%s\n", attempt, err, tail(string(out), 20))
			}
		}
		rows, qerr := q.Query(ctx, sql)
		var got map[string]loadgen.Count
		if qerr == nil {
			got, qerr = loadgen.ParseCounts(rows)
		}
		if qerr != nil {
			last = []string{qerr.Error()}
		} else {
			last = loadgen.Diff(m.Expected, got)
			stored := 0
			for _, c := range got {
				stored += c.Rows
			}
			fmt.Printf("loadgen_verify attempt=%d expected=%d stored_rows=%d problems=%d\n", attempt, m.Total, stored, len(last))
			if len(last) == 0 {
				printJSON("loadgen_verify_ok", map[string]any{"run_id": m.RunID, "events": m.Total, "by_event_name": sortedCounts(got)})
				return nil
			}
		}
		if time.Now().Add(v.interval).After(deadline) {
			return fmt.Errorf("verification failed after %d attempts: %s", attempt, strings.Join(last, "; "))
		}
		select {
		case <-time.After(v.interval):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func cmdRun(ctx context.Context, args []string) error {
	var s sendFlags
	var v verifyFlags
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	s.register(fs)
	v.register(fs, false)
	if err := parse(fs, args); err != nil {
		return err
	}
	v.insecure = s.insecure
	m, err := doSend(ctx, s)
	if err != nil {
		return err
	}
	return doVerify(ctx, v, m)
}

func sortedCounts(got map[string]loadgen.Count) map[string]int {
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make(map[string]int, len(names))
	for _, n := range names {
		out[n] = got[n].Rows
	}
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func ms(d time.Duration) float64 { return round(float64(d.Microseconds()) / 1000) }
func round(f float64) float64    { return float64(int64(f*10+0.5)) / 10 }
