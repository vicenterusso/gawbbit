package schema

import "github.com/mirlan-irokez/rawbbit/backend/collector-api/internal/pyjson"

// Dump renders the event like EventEnvelope.model_dump(mode="json"): every
// field in declaration order, nested models as objects or null.
func (e *Event) Dump() *pyjson.ObjectBuilder {
	o := pyjson.NewObject(14)
	o.Set("event_id", pyjson.StringValue(e.EventID))
	o.Set("app_id", pyjson.StringValue(e.AppID))
	o.Set("environment", pyjson.OptString(e.Environment))
	o.Set("event_name", pyjson.StringValue(e.EventName))
	o.Set("event_timestamp", pyjson.StringValue(e.EventTimestamp.String()))
	o.Set("received_at", optDatetime(e.ReceivedAt))
	o.Set("user", dumpModel(e.User, func(m *UserContext) []field {
		return []field{{"user_id", m.UserID}, {"user_pseudo_id", m.UserPseudoID}, {"session_id", m.SessionID}}
	}))
	o.Set("device", dumpModel(e.Device, func(m *DeviceContext) []field {
		return []field{
			{"platform", m.Platform}, {"app_version", m.AppVersion}, {"os_version", m.OSVersion},
			{"device_model", m.DeviceModel}, {"locale", m.Locale}, {"timezone", m.Timezone},
		}
	}))
	o.Set("traffic_source", dumpModel(e.TrafficSource, func(m *TrafficSource) []field {
		return []field{{"source", m.Source}, {"medium", m.Medium}, {"campaign", m.Campaign}}
	}))
	o.Set("geo", dumpModel(e.Geo, func(m *GeoContext) []field {
		return []field{
			{"country", m.Country}, {"country_code", m.CountryCode}, {"city", m.City},
			{"region", m.Region}, {"timezone", m.Timezone},
		}
	}))
	o.Set("event_params", e.EventParams)
	o.Set("user_properties", e.UserProperties)
	if e.Consent == nil {
		o.Set("consent", pyjson.NullValue())
	} else {
		c := pyjson.NewObject(3)
		c.Set("analytics_storage", optBool(e.Consent.AnalyticsStorage))
		c.Set("ads_storage", optBool(e.Consent.AdsStorage))
		c.Set("personalization", optBool(e.Consent.Personalization))
		o.Set("consent", c.Value())
	}
	if e.Ingest == nil {
		o.Set("ingest", pyjson.NullValue())
	} else {
		in := pyjson.NewObject(5)
		in.Set("user_agent", pyjson.OptString(e.Ingest.UserAgent))
		in.Set("ip_hash", pyjson.OptString(e.Ingest.IPHash))
		in.Set("ip", pyjson.OptString(e.Ingest.IP))
		in.Set("received_at", optDatetime(e.Ingest.ReceivedAt))
		in.Set("request_id", pyjson.OptString(e.Ingest.RequestID))
		o.Set("ingest", in.Value())
	}
	return o
}

type field struct {
	name  string
	value *string
}

func dumpModel[T any](m *T, fields func(*T) []field) pyjson.Value {
	if m == nil {
		return pyjson.NullValue()
	}
	fs := fields(m)
	o := pyjson.NewObject(len(fs))
	for _, f := range fs {
		o.Set(f.name, pyjson.OptString(f.value))
	}
	return o.Value()
}

func optDatetime(d *DateTime) pyjson.Value {
	if d == nil {
		return pyjson.NullValue()
	}
	return pyjson.StringValue(d.String())
}

func optBool(b *bool) pyjson.Value {
	if b == nil {
		return pyjson.NullValue()
	}
	return pyjson.Value{Kind: pyjson.Bool, Bool: *b}
}
