// Package cli reproduces the argparse command line of the original
// provision_datasets.py: same options, abbreviations, messages and exit codes.
package cli

import (
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Args are the parsed command-line arguments.
type Args struct {
	Config        string
	RuntimeConfig string
	Schema        string
	Journal       string
	PreflightOnly bool
}

// ExitError carries an argparse exit status (0 for --help, 2 for usage errors).
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return "exit " + strconv.Itoa(e.Code) }

const description = "Provision optional Rawbbit ClickHouse datasets safely"

type option struct {
	long    string
	dest    string
	flag    bool
	metavar string
}

var options = []option{
	{long: "--config", dest: "config", metavar: "CONFIG"},
	{long: "--runtime-config", dest: "runtime_config", metavar: "RUNTIME_CONFIG"},
	{long: "--schema", dest: "schema", metavar: "SCHEMA"},
	{long: "--journal", dest: "journal", metavar: "JOURNAL"},
	{long: "--preflight-only", dest: "preflight_only", flag: true},
}

var negativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

// width mirrors argparse's HelpFormatter (terminal columns minus 2).
func width() int {
	if n, err := strconv.Atoi(os.Getenv("COLUMNS")); err == nil && n > 0 {
		return n - 2
	}
	return 78
}

func usage(prog string) string {
	parts := []string{"[-h]"}
	for _, o := range options {
		if o.flag {
			parts = append(parts, "["+o.long+"]")
		} else {
			parts = append(parts, o.long+" "+o.metavar)
		}
	}
	const prefix = "usage: "
	textWidth := width()
	full := prog + " " + strings.Join(parts, " ")
	if len(prefix)+len(full) <= textWidth {
		return prefix + full
	}
	getLines := func(parts []string, indent string, withPrefix bool) []string {
		var lines, line []string
		lineLen := len(indent) - 1
		if withPrefix {
			lineLen = len(prefix) - 1
		}
		for _, part := range parts {
			if lineLen+1+len(part) > textWidth && len(line) > 0 {
				lines = append(lines, indent+strings.Join(line, " "))
				line = nil
				lineLen = len(indent) - 1
			}
			line = append(line, part)
			lineLen += len(part) + 1
		}
		if len(line) > 0 {
			lines = append(lines, indent+strings.Join(line, " "))
		}
		if withPrefix {
			lines[0] = lines[0][len(indent):]
		}
		return lines
	}
	var lines []string
	if float64(len(prefix)+len(prog)) <= 0.75*float64(textWidth) {
		indent := strings.Repeat(" ", len(prefix)+len(prog)+1)
		lines = getLines(append([]string{prog}, parts...), indent, true)
	} else {
		indent := strings.Repeat(" ", len(prefix))
		lines = append([]string{prog}, getLines(parts, indent, false)...)
	}
	return prefix + strings.Join(lines, "\n")
}

// wrap is textwrap.wrap for plain single-spaced text.
func wrap(text string, width int) []string {
	var lines []string
	line := ""
	for _, word := range strings.Fields(text) {
		switch {
		case line == "":
			line = word
		case len(line)+1+len(word) <= width:
			line += " " + word
		default:
			lines = append(lines, line)
			line = word
		}
	}
	if line != "" {
		lines = append(lines, line)
	}
	return lines
}

// help mirrors argparse.HelpFormatter for this parser.
func help(prog string) string {
	textWidth := width()
	invocations := []string{"-h, --help"}
	for _, o := range options {
		if o.flag {
			invocations = append(invocations, o.long)
		} else {
			invocations = append(invocations, o.long+" "+o.metavar)
		}
	}
	maxLen := 0
	for _, inv := range invocations {
		maxLen = max(maxLen, len(inv)+2)
	}
	helpPosition := min(maxLen+2, min(24, max(textWidth-20, 4)))
	helpWidth := max(textWidth-helpPosition, 11)
	var b strings.Builder
	b.WriteString(usage(prog) + "\n\n")
	b.WriteString(strings.Join(wrap(description, textWidth), "\n") + "\n\noptions:\n")
	for i, inv := range invocations {
		if i > 0 {
			b.WriteString("  " + inv + "\n")
			continue
		}
		helpLines := wrap("show this help message and exit", helpWidth)
		if actionWidth := helpPosition - 4; len(inv) <= actionWidth {
			b.WriteString(fmt.Sprintf("  %-*s  %s\n", actionWidth, inv, helpLines[0]))
		} else {
			b.WriteString("  " + inv + "\n" + strings.Repeat(" ", helpPosition) + helpLines[0] + "\n")
		}
		for _, l := range helpLines[1:] {
			b.WriteString(strings.Repeat(" ", helpPosition) + l + "\n")
		}
	}
	return b.String()
}

func fail(stderr io.Writer, prog, msg string) error {
	fmt.Fprintf(stderr, "%s\n%s: error: %s\n", usage(prog), prog, msg)
	return &ExitError{Code: 2}
}

// match resolves an option token: exact name, "--name=value" or a unique
// "--prefix" abbreviation.
func match(arg string) (opt *option, explicit *string, isHelp, ambiguous bool, matches []string) {
	name := arg
	if i := strings.IndexByte(arg, '='); i >= 0 && strings.HasPrefix(arg, "--") {
		name = arg[:i]
		v := arg[i+1:]
		explicit = &v
	}
	all := append([]string{"-h", "--help"}, func() []string {
		var out []string
		for _, o := range options {
			out = append(out, o.long)
		}
		return out
	}()...)
	for _, candidate := range all {
		if candidate == name {
			return lookup(candidate), explicit, candidate == "-h" || candidate == "--help", false, nil
		}
	}
	if !strings.HasPrefix(name, "--") {
		return nil, nil, false, false, nil
	}
	for _, candidate := range all {
		if strings.HasPrefix(candidate, name) && strings.HasPrefix(candidate, "--") {
			matches = append(matches, candidate)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil, false, false, nil
	case 1:
		return lookup(matches[0]), explicit, matches[0] == "--help", false, nil
	}
	return nil, nil, false, true, matches
}

func lookup(name string) *option {
	for i := range options {
		if options[i].long == name {
			return &options[i]
		}
	}
	return nil
}

// isOptionLike mirrors argparse's classification of "O" arguments.
func isOptionLike(arg string) bool {
	if arg == "" || arg[0] != '-' || arg == "-" || arg == "--" {
		return false
	}
	if opt, _, isHelp, ambiguous, _ := match(arg); opt != nil || isHelp || ambiguous {
		return true
	}
	if negativeNumber.MatchString(arg) || strings.Contains(arg, " ") {
		return false
	}
	return true
}

// Parse parses argv (without the program name). On --help or a usage error
// it writes to stdout/stderr and returns an *ExitError.
func Parse(prog string, argv []string, stdout, stderr io.Writer) (*Args, error) {
	values := map[string]string{}
	seen := map[string]bool{}
	var extras []string
	positionalOnly := false
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" && !positionalOnly {
			positionalOnly = true
			continue
		}
		if positionalOnly || !isOptionLike(arg) {
			extras = append(extras, arg)
			continue
		}
		opt, explicit, isHelp, ambiguous, matches := match(arg)
		switch {
		case ambiguous:
			return nil, fail(stderr, prog, fmt.Sprintf("ambiguous option: %s could match %s", arg, strings.Join(matches, ", ")))
		case isHelp:
			if explicit != nil {
				return nil, fail(stderr, prog, fmt.Sprintf("argument -h/--help: ignored explicit argument '%s'", *explicit))
			}
			_, _ = io.WriteString(stdout, help(prog))
			return nil, &ExitError{Code: 0}
		case opt == nil:
			extras = append(extras, arg)
		case opt.flag:
			if explicit != nil {
				return nil, fail(stderr, prog, fmt.Sprintf("argument %s: ignored explicit argument '%s'", opt.long, *explicit))
			}
			values[opt.dest] = "1"
			seen[opt.dest] = true
		case explicit != nil:
			values[opt.dest] = *explicit
			seen[opt.dest] = true
		default:
			if i+1 >= len(argv) || isOptionLike(argv[i+1]) || (argv[i+1] == "--" && !positionalOnly) {
				return nil, fail(stderr, prog, fmt.Sprintf("argument %s: expected one argument", opt.long))
			}
			i++
			values[opt.dest] = argv[i]
			seen[opt.dest] = true
		}
	}
	var missing []string
	for _, o := range options {
		if !o.flag && !seen[o.dest] {
			missing = append(missing, o.long)
		}
	}
	if len(missing) > 0 {
		return nil, fail(stderr, prog, "the following arguments are required: "+strings.Join(missing, ", "))
	}
	if len(extras) > 0 {
		return nil, fail(stderr, prog, "unrecognized arguments: "+strings.Join(extras, " "))
	}
	return &Args{
		Config:        values["config"],
		RuntimeConfig: values["runtime_config"],
		Schema:        values["schema"],
		Journal:       values["journal"],
		PreflightOnly: seen["preflight_only"],
	}, nil
}
