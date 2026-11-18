package config

import (
	"bufio"
	"os"
	"regexp"
	"strings"
)

// readDotenv parses a .env file with the python-dotenv rules pydantic-settings
// relied on: optional "export", quoted or unquoted values, inline comments
// after whitespace, and ${NAME} / ${NAME:-default} expansion (earlier file
// values first, then the process environment) outside single quotes.
// A missing file yields no values.
func readDotenv(path string, environ map[string]string) map[string]string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue // a bare key has no value
		}
		key := strings.Trim(strings.TrimSpace(line[:eq]), `'"`)
		raw := strings.TrimLeft(line[eq+1:], " \t")
		var value string
		expand := true
		switch {
		case strings.HasPrefix(raw, "'") || strings.HasPrefix(raw, `"`):
			quote := raw[0]
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
			value = unescapeQuoted(body[:end], quote)
			expand = quote == '"'
		default:
			if j := strings.Index(raw, " #"); j >= 0 {
				raw = raw[:j]
			}
			if j := strings.Index(raw, "\t#"); j >= 0 {
				raw = raw[:j]
			}
			value = strings.TrimSpace(raw)
		}
		if expand {
			value = expandVars(value, values, environ)
		}
		values[key] = value
	}
	return values
}

func closingQuote(s string, quote byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' {
			i++
			continue
		}
		if s[i] == quote {
			return i
		}
	}
	return -1
}

func hasClosingQuote(s string, quote byte) bool { return closingQuote(s, quote) >= 0 }

func unescapeQuoted(s string, quote byte) string {
	if quote == '\'' {
		return strings.NewReplacer(`\\`, `\`, `\'`, `'`).Replace(s)
	}
	return strings.NewReplacer(
		`\\`, `\`, `\'`, `'`, `\"`, `"`, `\a`, "\a", `\b`, "\b",
		`\f`, "\f", `\n`, "\n", `\r`, "\r", `\t`, "\t", `\v`, "\v",
	).Replace(s)
}

var dotenvVar = regexp.MustCompile(`\$\{([^}:]+)(?::-([^}]*))?\}`)

func expandVars(value string, file, environ map[string]string) string {
	return dotenvVar.ReplaceAllStringFunc(value, func(m string) string {
		parts := dotenvVar.FindStringSubmatch(m)
		if v, ok := file[parts[1]]; ok {
			return v
		}
		if v, ok := environ[parts[1]]; ok {
			return v
		}
		return parts[2]
	})
}
