// Package plugincheck holds checks shared by the plugin validation commands.
package plugincheck

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var linkRE = regexp.MustCompile(`!?\[[^\]]*\]\(([^)]+)\)`)

// SkillFrontmatter matches a SKILL.md file split into frontmatter and body.
var SkillFrontmatter = regexp.MustCompile(`(?s)\A---\n(.*?)\n---\n(.*)\z`)

// Truthy mirrors Python truthiness for decoded JSON values.
func Truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case float64:
		return typed != 0
	case string:
		return typed != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	}
	return true
}

// NonBlank reports whether value is a string with non-whitespace content.
func NonBlank(value any) bool {
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

// Within resolves target (following symlinks) and reports whether it stays inside root.
func Within(root, target string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		resolved, err = filepath.Abs(target)
		if err != nil {
			return "", false
		}
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return resolved, false
	}
	return resolved, true
}

// Documents returns the README, the skill and every reference Markdown file.
func Documents(plugin, skill string) ([]string, error) {
	references, err := filepath.Glob(filepath.Join(filepath.Dir(skill), "references", "*.md"))
	if err != nil {
		return nil, err
	}
	return append([]string{filepath.Join(plugin, "README.md"), skill}, references...), nil
}

// CheckLinks fails on any relative Markdown link that does not point to a file inside plugin.
func CheckLinks(plugin string, documents []string, message string) error {
	for _, document := range documents {
		text, err := os.ReadFile(document)
		if err != nil {
			return err
		}
		for _, match := range linkRE.FindAllStringSubmatch(string(text), -1) {
			target := match[1]
			parsed, err := url.Parse(target)
			if err != nil {
				return fmt.Errorf("%s in %s: %s", message, document, target)
			}
			if parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" || parsed.Path == "" {
				continue
			}
			candidate := parsed.Path
			if !filepath.IsAbs(candidate) {
				candidate = filepath.Join(filepath.Dir(document), candidate)
			}
			resolved, inside := Within(plugin, candidate)
			info, statErr := os.Stat(resolved)
			if !inside || statErr != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("%s in %s: %s", message, document, target)
			}
		}
	}
	return nil
}

// PluginRoot returns the absolute, symlink-resolved plugin folder.
func PluginRoot(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(absolute)
}
