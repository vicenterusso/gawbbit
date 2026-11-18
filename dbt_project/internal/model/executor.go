package model

import (
	"context"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/clickhouse"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/dbt_project/internal/storage"
)

// Profile timeouts (formerly profiles.yml connect_timeout/send_receive_timeout).
const (
	ConnectTimeout = 10 * time.Second
	ReceiveTimeout = 600 * time.Second
)

// HTTPExecutor runs loader statements over ClickHouse HTTP. Every request
// carries a fresh query ID and the attempt tag (log_comment). When
// RequestDir is set, each request is journaled durably BEFORE dispatch
// (status "dispatched", then its non-secret transport settings) and marked
// "confirmed" only after an acknowledged response. Requests are never
// replayed after a transport failure: a broken connection is not proof that
// ClickHouse did not accept a write.
type HTTPExecutor struct {
	Client     *clickhouse.Client
	Tag        string
	MaxThreads int64
	RequestDir string
}

// NewHTTPExecutor builds the loader executor for one attempt.
func NewHTTPExecutor(ep clickhouse.Endpoint, tag string, maxThreads int64, requestDir string) *HTTPExecutor {
	return &HTTPExecutor{Client: clickhouse.NewClient(ep, ConnectTimeout, ReceiveTimeout),
		Tag: tag, MaxThreads: maxThreads, RequestDir: requestDir}
}

func (h *HTTPExecutor) params(queryID string) url.Values {
	p := url.Values{}
	p.Set("query_id", queryID)
	p.Set("log_comment", h.Tag)
	p.Set("max_threads", strconv.FormatInt(h.MaxThreads, 10))
	p.Set("max_execution_time", "600")
	p.Set("mutations_sync", "2")
	p.Set("lightweight_deletes_sync", "2")
	p.Set("log_queries", "1")
	p.Set("log_query_settings", "1")
	p.Set("allow_nondeterministic_mutations", "1")
	p.Set("date_time_input_format", "best_effort")
	p.Set("distributed_foreground_insert", "1")
	p.Set("cancel_http_readonly_queries_on_client_close", "1")
	p.Set("wait_end_of_query", "1")
	return p
}

func (h *HTTPExecutor) send(ctx context.Context, sql string) ([]byte, error) {
	queryID := clickhouse.UUID4()
	params := h.params(queryID)
	var path string
	if h.RequestDir != "" {
		path = filepath.Join(h.RequestDir, queryID+".json")
		record := pyjson.NewObject("query_id", queryID, "status", "dispatched")
		if err := storage.Save(path, record); err != nil {
			return nil, err
		}
		// Only fixed nonsecret ownership/synchronization settings.
		record.Set("transport_settings", map[string]any{
			"log_comment":              params.Get("log_comment"),
			"mutations_sync":           params.Get("mutations_sync"),
			"lightweight_deletes_sync": params.Get("lightweight_deletes_sync"),
		})
		if err := storage.Save(path, record); err != nil {
			return nil, err
		}
	}
	body, err := h.Client.Do(ctx, sql, params, 16*1024*1024)
	if err != nil {
		return nil, err
	}
	if path != "" {
		record, err := storage.LoadObject(path)
		if err != nil {
			return nil, err
		}
		record.Set("status", "confirmed")
		if err := storage.Save(path, record); err != nil {
			return nil, err
		}
	}
	return body, nil
}

// Exec runs a statement that returns no rows.
func (h *HTTPExecutor) Exec(ctx context.Context, sql string) error {
	_, err := h.send(ctx, sql)
	return err
}

// Query runs a SELECT-like statement and decodes FORMAT JSON rows.
func (h *HTTPExecutor) Query(ctx context.Context, sql string) ([]map[string]any, error) {
	body, err := h.send(ctx, sql+" FORMAT JSON")
	if err != nil {
		return nil, err
	}
	return clickhouse.Rows(body)
}
