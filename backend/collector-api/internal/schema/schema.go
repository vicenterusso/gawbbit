// Package schema validates the collector's request body with the same lax
// JSON-mode semantics as the original pydantic models (EventBatchRequest,
// EventEnvelope and their nested contexts) and renders validated events the
// way model_dump(mode="json") did.
package schema

import (
	"math"

	"github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"
)

// Error types produced by validation (pydantic error type names).
const (
	ErrJSONInvalid             = "json_invalid"
	ErrMissing                 = "missing"
	ErrStringType              = "string_type"
	ErrDatetimeType            = "datetime_type"
	ErrDatetimeParsing         = "datetime_parsing"
	ErrDatetimeFromDateParsing = "datetime_from_date_parsing"
	ErrModelType               = "model_type"
	ErrListType                = "list_type"
	ErrDictType                = "dict_type"
	ErrBoolType                = "bool_type"
	ErrBoolParsing             = "bool_parsing"
)

// ValidationError is one pydantic line error. Loc elements are string or int.
type ValidationError struct {
	Type string
	Loc  []any
}

// UserContext and the other context types mirror the pydantic models.
type UserContext struct {
	UserID, UserPseudoID, SessionID *string
}

type DeviceContext struct {
	Platform, AppVersion, OSVersion, DeviceModel, Locale, Timezone *string
}

type TrafficSource struct {
	Source, Medium, Campaign *string
}

type GeoContext struct {
	Country, CountryCode, City, Region, Timezone *string
}

type ConsentContext struct {
	AnalyticsStorage, AdsStorage, Personalization *bool
}

type IngestContext struct {
	UserAgent, IPHash, IP *string
	ReceivedAt            *DateTime
	RequestID             *string
}

// Event is a validated EventEnvelope.
type Event struct {
	EventID        string
	AppID          string
	Environment    *string
	EventName      string
	EventTimestamp DateTime
	ReceivedAt     *DateTime
	User           *UserContext
	Device         *DeviceContext
	TrafficSource  *TrafficSource
	Geo            *GeoContext
	EventParams    pyjson.Value // object, Python dict semantics applied
	UserProperties pyjson.Value
	Consent        *ConsentContext
	Ingest         *IngestContext
}

// ParseBatch validates a request body. On failure it returns every
// validation error in pydantic's order.
func ParseBatch(body []byte) ([]Event, []ValidationError) {
	root, err := pyjson.Parse(body)
	if err != nil {
		return nil, []ValidationError{{Type: ErrJSONInvalid, Loc: []any{}}}
	}
	v := &validator{}
	if root.Kind != pyjson.Object {
		v.fail(ErrModelType)
		return nil, v.errors
	}
	var events []Event
	if raw, ok := root.Get("events"); ok {
		v.push("events")
		if raw.Kind != pyjson.Array {
			v.fail(ErrListType)
		} else {
			events = make([]Event, 0, len(raw.Array))
			for i := range raw.Array {
				v.push(i)
				if ev, ok := v.event(&raw.Array[i]); ok {
					events = append(events, ev)
				}
				v.pop()
			}
		}
		v.pop()
	}
	if len(v.errors) > 0 {
		return nil, v.errors
	}
	if events == nil {
		events = []Event{}
	}
	return events, nil
}

type validator struct {
	loc    []any
	errors []ValidationError
}

func (v *validator) push(part any) { v.loc = append(v.loc, part) }
func (v *validator) pop()          { v.loc = v.loc[:len(v.loc)-1] }

func (v *validator) fail(errType string) {
	loc := make([]any, len(v.loc))
	copy(loc, v.loc)
	v.errors = append(v.errors, ValidationError{Type: errType, Loc: loc})
}

// field runs check on obj[name] (if present) with the location extended.
// It returns false when the field is missing.
func (v *validator) field(obj *pyjson.Value, name string, check func(*pyjson.Value)) bool {
	raw, ok := obj.Get(name)
	if !ok {
		return false
	}
	v.push(name)
	check(raw)
	v.pop()
	return true
}

func (v *validator) requiredString(obj *pyjson.Value, name string, dst *string) {
	if !v.field(obj, name, func(raw *pyjson.Value) {
		if raw.Kind != pyjson.String {
			v.fail(ErrStringType)
			return
		}
		*dst = raw.Str
	}) {
		v.push(name)
		v.fail(ErrMissing)
		v.pop()
	}
}

func (v *validator) optString(obj *pyjson.Value, name string, dst **string) {
	v.field(obj, name, func(raw *pyjson.Value) {
		switch raw.Kind {
		case pyjson.Null:
		case pyjson.String:
			s := raw.Str
			*dst = &s
		default:
			v.fail(ErrStringType)
		}
	})
}

func (v *validator) optBool(obj *pyjson.Value, name string, dst **bool) {
	v.field(obj, name, func(raw *pyjson.Value) {
		if raw.Kind == pyjson.Null {
			return
		}
		b, errType := laxBool(raw)
		if errType != "" {
			v.fail(errType)
			return
		}
		*dst = &b
	})
}

// laxBool is pydantic-core's JSON bool validation in lax mode.
func laxBool(raw *pyjson.Value) (bool, string) {
	switch raw.Kind {
	case pyjson.Bool:
		return raw.Bool, ""
	case pyjson.String:
		switch lowerASCII(raw.Str) {
		case "0", "f", "n", "no", "off", "false":
			return false, ""
		case "1", "t", "y", "on", "yes", "true":
			return true, ""
		}
		return false, ErrBoolParsing
	case pyjson.Int:
		switch raw.Int {
		case 0:
			return false, ""
		case 1:
			return true, ""
		}
		return false, ErrBoolParsing
	case pyjson.Float:
		f := raw.Float
		if math.IsNaN(f) || math.IsInf(f, 0) || math.Mod(f, 1) != 0 ||
			f <= math.MinInt64 || f >= math.MaxInt64 {
			return false, ErrBoolType
		}
		switch f {
		case 0:
			return false, ""
		case 1:
			return true, ""
		}
		return false, ErrBoolParsing
	}
	return false, ErrBoolType
}

// lowerASCII lowercases ASCII letters only, like Rust's eq_ignore_ascii_case.
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// laxDatetime is the pydantic `datetime` validator in lax mode on JSON input.
func laxDatetime(raw *pyjson.Value) (DateTime, string) {
	var dt DateTime
	var err error
	switch raw.Kind {
	case pyjson.String:
		dt, err = parseDateTimeString([]byte(raw.Str))
		if err != nil {
			// Lax mode retries as a date and uses midnight (naive).
			d, derr := parseDateString([]byte(raw.Str))
			if derr != nil {
				return DateTime{}, ErrDatetimeFromDateParsing
			}
			dt = DateTime{Year: d.year, Month: d.month, Day: d.day}
		}
	case pyjson.Int:
		if dt, err = fromTimestamp(raw.Int, 0); err != nil {
			return DateTime{}, ErrDatetimeParsing
		}
	case pyjson.Float:
		if math.IsNaN(raw.Float) {
			return DateTime{}, ErrDatetimeParsing
		}
		if dt, err = fromFloat(raw.Float); err != nil {
			return DateTime{}, ErrDatetimeParsing
		}
	default:
		return DateTime{}, ErrDatetimeType
	}
	if dt.Year == 0 {
		// Python's datetime cannot represent year 0.
		return DateTime{}, ErrDatetimeParsing
	}
	return dt, ""
}

func (v *validator) optDatetime(obj *pyjson.Value, name string, dst **DateTime) {
	v.field(obj, name, func(raw *pyjson.Value) {
		if raw.Kind == pyjson.Null {
			return
		}
		dt, errType := laxDatetime(raw)
		if errType != "" {
			v.fail(errType)
			return
		}
		*dst = &dt
	})
}

func (v *validator) dict(obj *pyjson.Value, name string, dst *pyjson.Value) {
	*dst = pyjson.NewObject(0).Value()
	v.field(obj, name, func(raw *pyjson.Value) {
		if raw.Kind != pyjson.Object {
			v.fail(ErrDictType)
			return
		}
		*dst = raw.Dict()
	})
}

// optModel validates a nullable nested model; build runs on object input.
func optModel[T any](v *validator, obj *pyjson.Value, name string, dst **T, build func(*pyjson.Value, *T)) {
	v.field(obj, name, func(raw *pyjson.Value) {
		switch raw.Kind {
		case pyjson.Null:
		case pyjson.Object:
			before := len(v.errors)
			var m T
			build(raw, &m)
			if len(v.errors) == before {
				*dst = &m
			}
		default:
			v.fail(ErrModelType)
		}
	})
}

func (v *validator) event(raw *pyjson.Value) (Event, bool) {
	if raw.Kind != pyjson.Object {
		v.fail(ErrModelType)
		return Event{}, false
	}
	before := len(v.errors)
	var ev Event
	v.requiredString(raw, "event_id", &ev.EventID)
	v.requiredString(raw, "app_id", &ev.AppID)
	v.optString(raw, "environment", &ev.Environment)
	v.requiredString(raw, "event_name", &ev.EventName)
	if !v.field(raw, "event_timestamp", func(ts *pyjson.Value) {
		dt, errType := laxDatetime(ts)
		if errType != "" {
			v.fail(errType)
			return
		}
		ev.EventTimestamp = dt
	}) {
		v.push("event_timestamp")
		v.fail(ErrMissing)
		v.pop()
	}
	v.optDatetime(raw, "received_at", &ev.ReceivedAt)
	optModel(v, raw, "user", &ev.User, func(o *pyjson.Value, m *UserContext) {
		v.optString(o, "user_id", &m.UserID)
		v.optString(o, "user_pseudo_id", &m.UserPseudoID)
		v.optString(o, "session_id", &m.SessionID)
	})
	optModel(v, raw, "device", &ev.Device, func(o *pyjson.Value, m *DeviceContext) {
		v.optString(o, "platform", &m.Platform)
		v.optString(o, "app_version", &m.AppVersion)
		v.optString(o, "os_version", &m.OSVersion)
		v.optString(o, "device_model", &m.DeviceModel)
		v.optString(o, "locale", &m.Locale)
		v.optString(o, "timezone", &m.Timezone)
	})
	optModel(v, raw, "traffic_source", &ev.TrafficSource, func(o *pyjson.Value, m *TrafficSource) {
		v.optString(o, "source", &m.Source)
		v.optString(o, "medium", &m.Medium)
		v.optString(o, "campaign", &m.Campaign)
	})
	optModel(v, raw, "geo", &ev.Geo, func(o *pyjson.Value, m *GeoContext) {
		v.optString(o, "country", &m.Country)
		v.optString(o, "country_code", &m.CountryCode)
		v.optString(o, "city", &m.City)
		v.optString(o, "region", &m.Region)
		v.optString(o, "timezone", &m.Timezone)
	})
	v.dict(raw, "event_params", &ev.EventParams)
	v.dict(raw, "user_properties", &ev.UserProperties)
	optModel(v, raw, "consent", &ev.Consent, func(o *pyjson.Value, m *ConsentContext) {
		v.optBool(o, "analytics_storage", &m.AnalyticsStorage)
		v.optBool(o, "ads_storage", &m.AdsStorage)
		v.optBool(o, "personalization", &m.Personalization)
	})
	optModel(v, raw, "ingest", &ev.Ingest, func(o *pyjson.Value, m *IngestContext) {
		v.optString(o, "user_agent", &m.UserAgent)
		v.optString(o, "ip_hash", &m.IPHash)
		v.optString(o, "ip", &m.IP)
		v.optDatetime(o, "received_at", &m.ReceivedAt)
		v.optString(o, "request_id", &m.RequestID)
	})
	return ev, len(v.errors) == before
}
