package deploy

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Usage and Help reproduce the original argparse output for this program
// name (80-column formatting, as used non-interactively by Ansible).
const Usage = `usage: rawbbit-deploy-dbt-runner [-h] --install-dir INSTALL_DIR
                                 --stage-dir STAGE_DIR
                                 [--runtime-dir RUNTIME_DIR]
                                 --compose-file COMPOSE_FILE
                                 [--drain-timeout DRAIN_TIMEOUT]
                                 [--bootstrap-stopped] [--recover-gate]
                                 [--pre-routing-rollback]
                                 [--remove-dataset-overlay] [--validate-only |
                                 --inspect-mcp-env | --check-quiescence]
`

// Help is printed for -h/--help.
const Help = Usage + `
Fail-closed host cutover. All supported ingestion enters through dbt-job. No
signals are sent to jobs. An ungated running runner is NEVER stopped here. The
same pipeline.lock inode is held from drain through publication/verification.
Docker-exec of raw dbt, bypassing dbt-job, is not a supported maintenance
workflow.

options:
  -h, --help            show this help message and exit
  --install-dir INSTALL_DIR
  --stage-dir STAGE_DIR
  --runtime-dir RUNTIME_DIR
  --compose-file COMPOSE_FILE
  --drain-timeout DRAIN_TIMEOUT
  --bootstrap-stopped
  --recover-gate
  --pre-routing-rollback
  --remove-dataset-overlay
  --validate-only
  --inspect-mcp-env
  --check-quiescence    Read-only server check for an operator-held bootstrap
                        lock; does not stop jobs
`

// Args are the parsed command-line options.
type Args struct {
	InstallDir           string
	StageDir             string
	RuntimeDir           string
	ComposeFiles         []string
	DrainTimeout         time.Duration
	drainPositive        bool
	BootstrapStopped     bool
	RecoverGate          bool
	PreRoutingRollback   bool
	RemoveDatasetOverlay bool
	ValidateOnly         bool
	InspectMCPEnv        bool
	CheckQuiescence      bool
}

// UsageError is an argparse error (exit code 2).
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }

// ErrHelp requests the help text (exit code 0).
var ErrHelp = &UsageError{Msg: "help"}

var options = []string{"--help", "--install-dir", "--stage-dir", "--runtime-dir", "--compose-file",
	"--drain-timeout", "--bootstrap-stopped", "--recover-gate", "--pre-routing-rollback",
	"--remove-dataset-overlay", "--validate-only", "--inspect-mcp-env", "--check-quiescence"}

var takesValue = map[string]bool{"--install-dir": true, "--stage-dir": true, "--runtime-dir": true,
	"--compose-file": true, "--drain-timeout": true}

var exclusive = []string{"--validate-only", "--inspect-mcp-env", "--check-quiescence"}

var negativeNumber = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)

func isOption(arg string) bool {
	return strings.HasPrefix(arg, "-") && arg != "-" && !negativeNumber.MatchString(arg)
}

type token struct {
	option   string // resolved option name, "" for positional/unknown
	explicit *string
	raw      string
	unknown  bool
}

// classify resolves option names (including unique prefixes) before any
// value is consumed, so ambiguity errors come first, like argparse.
func classify(args []string) ([]token, error) {
	tokens := make([]token, 0, len(args))
	rest := false
	for _, arg := range args {
		if rest || !isOption(arg) {
			tokens = append(tokens, token{raw: arg})
			continue
		}
		if arg == "--" {
			rest = true
			continue
		}
		if arg == "-h" {
			tokens = append(tokens, token{option: "--help", raw: arg})
			continue
		}
		name, value, hasValue := strings.Cut(arg, "=")
		t := token{raw: arg}
		if hasValue {
			t.explicit = &value
		}
		if !strings.HasPrefix(name, "--") {
			t.unknown = true
			tokens = append(tokens, t)
			continue
		}
		var matches []string
		for _, option := range options {
			if option == name {
				matches = []string{option}
				break
			}
			if strings.HasPrefix(option, name) {
				matches = append(matches, option)
			}
		}
		switch len(matches) {
		case 0:
			t.unknown = true
		case 1:
			t.option = matches[0]
		default:
			return nil, &UsageError{Msg: fmt.Sprintf("ambiguous option: %s could match %s",
				name, strings.Join(matches, ", "))}
		}
		tokens = append(tokens, t)
	}
	return tokens, nil
}

// pyInt mirrors Python int(str): surrounding whitespace, a sign, and single
// underscores between digits are accepted.
func pyInt(s string) (*big.Int, bool) {
	s = strings.TrimFunc(s, unicode.IsSpace)
	body := strings.TrimLeft(s, "+-")
	if len(s)-len(body) > 1 || body == "" || body[0] == '_' || body[len(body)-1] == '_' ||
		strings.Contains(body, "__") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(s, "_", ""), 10)
	return n, ok
}

// ParseArgs parses argv like the original argparse configuration.
func ParseArgs(argv []string) (*Args, error) {
	tokens, err := classify(argv)
	if err != nil {
		return nil, err
	}
	args := &Args{RuntimeDir: "/srv/rawbbit-two/dbt", DrainTimeout: 600 * time.Second, drainPositive: true}
	seen := map[string]bool{}
	var group string
	var extras []string
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if t.option == "" {
			extras = append(extras, t.raw)
			continue
		}
		if t.option == "--help" {
			return nil, ErrHelp
		}
		for _, option := range exclusive {
			if option == t.option {
				if group != "" && group != option {
					return nil, &UsageError{Msg: fmt.Sprintf("argument %s: not allowed with argument %s", option, group)}
				}
				group = option
			}
		}
		var value string
		if takesValue[t.option] {
			switch {
			case t.explicit != nil:
				value = *t.explicit
			case i+1 < len(tokens) && tokens[i+1].option == "" && !tokens[i+1].unknown:
				i++
				value = tokens[i].raw
			default:
				return nil, &UsageError{Msg: "argument " + t.option + ": expected one argument"}
			}
		} else if t.explicit != nil {
			return nil, &UsageError{Msg: fmt.Sprintf("argument %s: ignored explicit argument '%s'", t.option, *t.explicit)}
		}
		seen[t.option] = true
		switch t.option {
		case "--install-dir":
			args.InstallDir = value
		case "--stage-dir":
			args.StageDir = value
		case "--runtime-dir":
			args.RuntimeDir = value
		case "--compose-file":
			args.ComposeFiles = append(args.ComposeFiles, value)
		case "--drain-timeout":
			n, ok := pyInt(value)
			if !ok {
				return nil, &UsageError{Msg: fmt.Sprintf("argument --drain-timeout: invalid int value: %s", pyRepr(value))}
			}
			args.drainPositive = n.Sign() > 0
			args.DrainTimeout = time.Duration(1<<62 - 1)
			if n.IsInt64() && n.Int64() < int64(args.DrainTimeout/time.Second) {
				args.DrainTimeout = time.Duration(n.Int64()) * time.Second
			}
		case "--bootstrap-stopped":
			args.BootstrapStopped = true
		case "--recover-gate":
			args.RecoverGate = true
		case "--pre-routing-rollback":
			args.PreRoutingRollback = true
		case "--remove-dataset-overlay":
			args.RemoveDatasetOverlay = true
		case "--validate-only":
			args.ValidateOnly = true
		case "--inspect-mcp-env":
			args.InspectMCPEnv = true
		case "--check-quiescence":
			args.CheckQuiescence = true
		}
	}
	var missing []string
	for _, required := range []string{"--install-dir", "--stage-dir", "--compose-file"} {
		if !seen[required] {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return nil, &UsageError{Msg: "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	if len(extras) > 0 {
		return nil, &UsageError{Msg: "unrecognized arguments: " + strings.Join(extras, " ")}
	}
	return args, nil
}

func pyRepr(s string) string {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), "'", `\'`) + "'"
}
