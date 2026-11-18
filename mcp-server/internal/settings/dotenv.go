package settings

import (
	"os"
	"regexp"
	"strings"
)

var (
	dotenvLine  = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_.]*)\s*=\s*(.*)$`)
	dotenvVar   = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)
	doubleQuote = strings.NewReplacer(`\\`, `\`, `\"`, `"`, `\n`, "\n", `\r`, "\r", `\t`, "\t", `\'`, `'`)
	singleQuote = strings.NewReplacer(`\\`, `\`, `\'`, `'`)
)

// parseDotenv implements the subset of python-dotenv syntax used for local
// development: comments, optional "export", quoted values and ${VAR}
// interpolation (environment first, then earlier file entries).
func parseDotenv(data string) map[string]string {
	out := map[string]string{}
	lines := strings.Split(strings.ReplaceAll(data, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		m := dotenvLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, raw := m[1], m[2]
		var value string
		interpolate := true
		switch {
		case strings.HasPrefix(raw, "'") || strings.HasPrefix(raw, `"`):
			quote := raw[:1]
			body := raw[1:]
			// Quoted values may span lines.
			for !hasClosingQuote(body, quote) && i+1 < len(lines) {
				i++
				body += "\n" + lines[i]
			}
			end := closingQuote(body, quote)
			if end < 0 {
				end = len(body)
			}
			body = body[:end]
			if quote == "'" {
				value = singleQuote.Replace(body)
				interpolate = false
			} else {
				value = doubleQuote.Replace(body)
			}
		default:
			if idx := strings.Index(raw, " #"); idx >= 0 {
				raw = raw[:idx]
			}
			value = strings.TrimSpace(raw)
		}
		if interpolate {
			value = dotenvVar.ReplaceAllStringFunc(value, func(ref string) string {
				sm := dotenvVar.FindStringSubmatch(ref)
				if v, ok := os.LookupEnv(sm[1]); ok {
					return v
				}
				if v, ok := out[sm[1]]; ok {
					return v
				}
				return sm[2]
			})
		}
		out[key] = value
	}
	return out
}

func closingQuote(body, quote string) int {
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '\\':
			i++
		case quote[0]:
			return i
		}
	}
	return -1
}

func hasClosingQuote(body, quote string) bool { return closingQuote(body, quote) >= 0 }
