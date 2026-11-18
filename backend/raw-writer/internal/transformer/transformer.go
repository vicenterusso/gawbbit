// Package transformer maps a decoded event to a raw Parquet row and its partition key.
package transformer

import (
	"math"
	"time"

	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/partitioning"
	"github.com/mirlan-irokez/rawbbit/backend/raw-writer/internal/pyval"
)

// PartitionKey identifies one output file stream: app_id, event_date and hour.
type PartitionKey struct {
	AppID     string
	EventDate string
	Hour      string
}

// Row is one raw event. Nil string pointers are Parquet NULLs.
type Row struct {
	EventID            *string
	AppID              *string
	Environment        *string
	EventName          *string
	EventTimestamp     *string
	ReceivedAt         *string
	UserID             *string
	UserPseudoID       *string
	SessionID          *string
	Platform           *string
	AppVersion         *string
	OSVersion          *string
	DeviceModel        *string
	Locale             *string
	Timezone           *string
	EventParamsJSON    *string
	UserPropertiesJSON *string
	TrafficSourceJSON  *string
	GeoJSON            *string
	ConsentJSON        *string
	IngestRequestID    *string
	IngestUserAgent    *string
	IngestIPHash       *string
	NATSStream         *string
	NATSSequence       int64
}

// StringFields returns pointers to every string column in schema order.
func (r *Row) StringFields() []**string {
	return []**string{
		&r.EventID, &r.AppID, &r.Environment, &r.EventName, &r.EventTimestamp, &r.ReceivedAt,
		&r.UserID, &r.UserPseudoID, &r.SessionID, &r.Platform, &r.AppVersion, &r.OSVersion,
		&r.DeviceModel, &r.Locale, &r.Timezone, &r.EventParamsJSON, &r.UserPropertiesJSON,
		&r.TrafficSourceJSON, &r.GeoJSON, &r.ConsentJSON, &r.IngestRequestID,
		&r.IngestUserAgent, &r.IngestIPHash, &r.NATSStream,
	}
}

// Metadata is the JetStream delivery metadata stored with each row.
type Metadata struct {
	Stream         string
	StreamSequence uint64
}

// MetadataSource yields a message's JetStream metadata; nil means the message has none.
type MetadataSource interface {
	Metadata() (*Metadata, error)
}

func get(container any, key string) (any, error) {
	obj, ok := container.(*pyval.Object)
	if !ok {
		return nil, pyval.ErrAttribute
	}
	v, _ := obj.Get(key)
	return v, nil
}

func orEmpty(v any) any {
	if pyval.Truthy(v) {
		return v
	}
	return pyval.NewObject()
}

func stringOrNone(v any) *string {
	if v == nil {
		return nil
	}
	s := pyval.Str(v)
	return &s
}

func jsonString(v any) *string {
	if v == nil {
		v = pyval.NewObject()
	}
	s := pyval.Dumps(v)
	return &s
}

// EventToRow follows the original event_to_row step by step so that a malformed event
// fails with the same exception class. Missing nested objects count as empty; a truthy
// non-object user, device or ingest fails with AttributeError.
func EventToRow(event any, msg MetadataSource, now func() time.Time) (PartitionKey, *Row, error) {
	ingestRaw, err := get(event, "ingest")
	if err != nil {
		return PartitionKey{}, nil, err
	}
	ingest := orEmpty(ingestRaw)
	user := orEmpty(field(event, "user"))
	device := orEmpty(field(event, "device"))

	eventTimestamp := field(event, "event_timestamp")
	receivedAt := field(event, "received_at")
	if !pyval.Truthy(receivedAt) {
		if receivedAt, err = get(ingest, "received_at"); err != nil {
			return PartitionKey{}, nil, err
		}
	}
	eventDate, hour, err := partitioning.EventPartition(eventTimestamp, receivedAt, now)
	if err != nil {
		return PartitionKey{}, nil, err
	}

	appID := "unknown_app"
	if raw := field(event, "app_id"); pyval.Truthy(raw) {
		appID = pyval.Str(raw)
	}

	meta, err := msg.Metadata()
	if err != nil {
		return PartitionKey{}, nil, err
	}
	row := &Row{AppID: &appID}
	if meta != nil {
		row.NATSStream = &meta.Stream
		if meta.StreamSequence <= math.MaxInt64 {
			row.NATSSequence = int64(meta.StreamSequence)
		}
	}

	row.EventID = stringOrNone(field(event, "event_id"))
	row.Environment = stringOrNone(field(event, "environment"))
	row.EventName = stringOrNone(field(event, "event_name"))
	row.EventTimestamp = stringOrNone(eventTimestamp)
	row.ReceivedAt = stringOrNone(receivedAt)

	nested := []struct {
		from any
		key  string
		dst  **string
	}{
		{user, "user_id", &row.UserID},
		{user, "user_pseudo_id", &row.UserPseudoID},
		{user, "session_id", &row.SessionID},
		{device, "platform", &row.Platform},
		{device, "app_version", &row.AppVersion},
		{device, "os_version", &row.OSVersion},
		{device, "device_model", &row.DeviceModel},
		{device, "locale", &row.Locale},
		{device, "timezone", &row.Timezone},
	}
	for _, f := range nested {
		v, err := get(f.from, f.key)
		if err != nil {
			return PartitionKey{}, nil, err
		}
		*f.dst = stringOrNone(v)
	}

	for key, dst := range map[string]**string{
		"event_params":    &row.EventParamsJSON,
		"user_properties": &row.UserPropertiesJSON,
		"traffic_source":  &row.TrafficSourceJSON,
		"geo":             &row.GeoJSON,
		"consent":         &row.ConsentJSON,
	} {
		*dst = jsonString(field(event, key))
	}

	for _, f := range []struct {
		key string
		dst **string
	}{
		{"request_id", &row.IngestRequestID},
		{"user_agent", &row.IngestUserAgent},
		{"ip_hash", &row.IngestIPHash},
	} {
		v, err := get(ingest, f.key)
		if err != nil {
			return PartitionKey{}, nil, err
		}
		*f.dst = stringOrNone(v)
	}

	// Python accepted lone surrogates here and then failed the whole partition upload when
	// encoding to UTF-8; rejecting the single event keeps its neighbours deliverable.
	for _, field := range row.StringFields() {
		if *field != nil && pyval.HasSurrogate(**field) {
			return PartitionKey{}, nil, pyval.ErrUnicodeEncode
		}
	}
	return PartitionKey{AppID: appID, EventDate: eventDate, Hour: hour}, row, nil
}

// field reads a key of the event, already known to be a dict.
func field(event any, key string) any {
	v, _ := get(event, key)
	return v
}
