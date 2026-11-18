package server

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/mirlan-irokez/rawbbit/mcp-server/internal/pyval"
)

type paramKind int

const (
	kindStr paramKind = iota
	kindOptStr
	kindInt
	kindBool
	kindStrList
)

// param describes one tool argument exactly like the Python signature.
type param struct {
	name     string
	kind     paramKind
	required bool
	def      pyval.Value // default (nil means None for optional strings)
}

func required(name string, kind paramKind) param {
	return param{name: name, kind: kind, required: true}
}
func optional(name string, kind paramKind, def pyval.Value) param {
	return param{name: name, kind: kind, def: def}
}

// schema renders the JSON Schema property FastMCP derives from the annotation.
func (p param) schema() string {
	var def string
	if !p.required {
		def = `"default":` + jsonLiteral(p.def) + ","
	}
	switch p.kind {
	case kindStr:
		return "{" + def + `"type":"string"}`
	case kindOptStr:
		return `{"anyOf":[{"type":"string"},{"type":"null"}],` + strings.TrimSuffix(def, ",") + "}"
	case kindInt:
		return "{" + def + `"type":"integer"}`
	case kindBool:
		return "{" + def + `"type":"boolean"}`
	default:
		return "{" + def + `"items":{"type":"string"},"type":"array"}`
	}
}

func jsonLiteral(v pyval.Value) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case string:
		b, _ := json.Marshal(t)
		return string(b)
	case bool:
		return strconv.FormatBool(t)
	}
	out, _ := pyval.EncodeJSON(v, pyval.NonFiniteNull)
	return string(out)
}

func inputSchema(params []param) json.RawMessage {
	var props, req []string
	for _, p := range params {
		props = append(props, strconv.Quote(p.name)+":"+p.schema())
		if p.required {
			req = append(req, strconv.Quote(p.name))
		}
	}
	s := `{"additionalProperties":false,"properties":{` + strings.Join(props, ",") + "}"
	if len(req) > 0 {
		s += `,"required":[` + strings.Join(req, ",") + "]"
	}
	return json.RawMessage(s + `,"type":"object"}`)
}

// validateArgs mirrors pydantic's validate_call for the tool signature,
// producing the same "N validation errors for call[tool]" messages.
func validateArgs(tool string, params []param, args *pyval.Dict) (map[string]pyval.Value, error) {
	out := make(map[string]pyval.Value, len(params))
	known := make(map[string]bool, len(params))
	var errs []pyval.LineError
	for _, p := range params {
		known[p.name] = true
		raw, ok := args.Get(p.name)
		if !ok {
			if p.required {
				errs = append(errs, pyval.LineError{Loc: []string{p.name}, Type: "missing_argument", Message: "Missing required argument", Input: args})
				continue
			}
			out[p.name] = p.def
			continue
		}
		value, lineErrs := coerce(p, raw)
		errs = append(errs, lineErrs...)
		out[p.name] = value
	}
	for i, k := range args.Keys {
		if name := k.(string); !known[name] {
			errs = append(errs, pyval.LineError{Loc: []string{name}, Type: "unexpected_keyword_argument", Message: "Unexpected keyword argument", Input: args.Values[i]})
		}
	}
	if len(errs) > 0 {
		return nil, &pyval.ValidationError{Title: "call[" + tool + "]", Errors: errs}
	}
	return out, nil
}

func stringTypeError(loc []string, v pyval.Value) pyval.LineError {
	return pyval.LineError{Loc: loc, Type: "string_type", Message: "Input should be a valid string", Input: v}
}

func coerce(p param, raw pyval.Value) (pyval.Value, []pyval.LineError) {
	loc := []string{p.name}
	switch p.kind {
	case kindStr:
		if s, ok := raw.(string); ok {
			return s, nil
		}
		return nil, []pyval.LineError{stringTypeError(loc, raw)}
	case kindOptStr:
		switch raw.(type) {
		case nil, string:
			return raw, nil
		}
		return nil, []pyval.LineError{stringTypeError(loc, raw)}
	case kindInt:
		n, le := pyval.ValidateLaxInt(raw)
		if le != nil {
			le.Loc = loc
			return nil, []pyval.LineError{*le}
		}
		return n, nil
	case kindBool:
		b, le := pyval.ValidateLaxBool(raw)
		if le != nil {
			le.Loc = loc
			return nil, []pyval.LineError{*le}
		}
		return b, nil
	}
	list, ok := raw.(pyval.List)
	if !ok {
		return nil, []pyval.LineError{{Loc: loc, Type: "list_type", Message: "Input should be a valid list", Input: raw}}
	}
	var errs []pyval.LineError
	items := make([]string, 0, len(list))
	for i, item := range list {
		s, ok := item.(string)
		if !ok {
			errs = append(errs, stringTypeError([]string{p.name, strconv.Itoa(i)}, item))
			continue
		}
		items = append(items, s)
	}
	return items, errs
}
