package server

import (
	"strconv"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/schema"
)

const maxValidationErrors = 20

// safeLocationFields are the location parts that may be echoed back; any
// other key is reported as "<field>" so that request content never leaks.
var safeLocationFields = map[string]bool{
	"events": true, "event_id": true, "app_id": true, "environment": true, "event_name": true,
	"event_timestamp": true, "received_at": true, "user": true, "device": true, "traffic_source": true,
	"geo": true, "event_params": true, "user_properties": true, "consent": true, "ingest": true,
	"user_id": true, "user_pseudo_id": true, "session_id": true, "platform": true, "app_version": true,
	"os_version": true, "device_model": true, "locale": true, "timezone": true, "source": true,
	"medium": true, "campaign": true, "country": true, "country_code": true, "city": true,
	"region": true, "analytics_storage": true, "ads_storage": true, "personalization": true,
	"user_agent": true, "ip_hash": true, "ip": true, "request_id": true,
}

// safeMessages are fixed messages per error type; values are never echoed.
var safeMessages = map[string]string{
	"json_invalid":     "Invalid JSON",
	"missing":          "Field required",
	"string_type":      "Input should be a valid string",
	"datetime_type":    "Input should be a valid datetime",
	"datetime_parsing": "Input should be a valid datetime",
	"model_type":       "Input should be a valid object",
	"list_type":        "Input should be a valid list",
	"dict_type":        "Input should be a valid dictionary",
	"bool_type":        "Input should be a valid boolean",
	"int_type":         "Input should be a valid integer",
	"float_type":       "Input should be a valid number",
}

func safeType(t string) string {
	if t == "" {
		return "validation_error"
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '_' {
			return "validation_error"
		}
	}
	return t
}

func sanitizedError(errType string, loc []any, dst []byte) []byte {
	dst = append(dst, `{"type":`...)
	dst = pyjson.AppendString(dst, errType)
	dst = append(dst, `,"loc":[`...)
	for i, part := range loc {
		if i > 0 {
			dst = append(dst, ',')
		}
		switch p := part.(type) {
		case int:
			if p >= 0 {
				dst = strconv.AppendInt(dst, int64(p), 10)
			} else {
				dst = pyjson.AppendString(dst, "<field>")
			}
		case string:
			if !safeLocationFields[p] {
				p = "<field>"
			}
			dst = pyjson.AppendString(dst, p)
		default:
			dst = pyjson.AppendString(dst, "<field>")
		}
	}
	msg, ok := safeMessages[errType]
	if !ok {
		msg = "Input is invalid"
	}
	dst = append(dst, `],"msg":`...)
	dst = pyjson.AppendString(dst, msg)
	return append(dst, '}')
}

// ValidationErrorBody renders the 422 body: {"detail": [...]} with only
// type, loc and msg per error, at most 20 entries, the 20th replaced by a
// fixed truncation marker when there are more.
func ValidationErrorBody(errs []schema.ValidationError) []byte {
	out := []byte(`{"detail":[`)
	limit := len(errs)
	truncated := limit > maxValidationErrors
	if truncated {
		limit = maxValidationErrors - 1
	}
	for i := 0; i < limit; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = sanitizedError(safeType(errs[i].Type), errs[i].Loc, out)
	}
	if truncated {
		out = append(out, `,{"type":"errors_truncated","loc":[],"msg":"Additional validation errors omitted"}`...)
	}
	return append(out, "]}"...)
}
