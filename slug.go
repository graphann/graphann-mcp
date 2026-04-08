package main

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
)

// nonSlugRe matches any run of characters that are not safe for a GraphANN
// tenant/index identifier. Anything matching is collapsed to a single '-'.
var nonSlugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify converts an arbitrary string to a GraphANN-safe identifier:
// lowercase, alphanumerics with '-' separators, trimmed, capped at 64 chars.
// Returns "default" for empty input so callers can pass it through safely.
func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 64 {
		s = strings.TrimRight(s[:64], "-")
	}
	if s == "" {
		return "default"
	}
	return s
}

// resolveTenant returns the slugified tenant name. Override precedence:
//  1. explicit GRAPHANN_TENANT env var
//  2. $USER / os/user.Current() + "-claude" suffix
//  3. "default-claude"
func resolveTenant() string {
	if v := strings.TrimSpace(os.Getenv("GRAPHANN_TENANT")); v != "" {
		return slugify(v)
	}
	name := os.Getenv("USER")
	if name == "" {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	if name == "" {
		return "default-claude"
	}
	return slugify(name + "-claude")
}

// resolveIndex returns the slugified per-project index name. Precedence:
//  1. GRAPHANN_INDEX env var
//  2. basename of CLAUDE_PROJECT_DIR
//  3. basename of PWD
//  4. basename of os.Getwd()
//  5. "default"
func resolveIndex() string {
	if v := strings.TrimSpace(os.Getenv("GRAPHANN_INDEX")); v != "" {
		return slugify(v)
	}
	for _, env := range []string{"CLAUDE_PROJECT_DIR", "PWD"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return slugify(filepath.Base(v))
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		return slugify(filepath.Base(cwd))
	}
	return "default"
}

// encodeMemoryText wraps the user-supplied text with a compact tag prefix so
// that kind/tags/source are searchable by the embedder. GraphANN's typed
// metadata schema silently drops arbitrary keys on /documents POST, so we
// inline them into the text itself. The format is machine-reversible:
//
//	[kind:preference tags:go,infra source:chat] <text>
//
// Each segment is only emitted when the corresponding field is non-empty. If
// all three are empty the original text is returned unchanged.
func encodeMemoryText(kind string, tags []string, source, text string) string {
	var prefix strings.Builder
	segs := make([]string, 0, 3)
	if k := strings.TrimSpace(kind); k != "" {
		segs = append(segs, fmt.Sprintf("kind:%s", slugify(k)))
	}
	if len(tags) > 0 {
		cleaned := make([]string, 0, len(tags))
		for _, t := range tags {
			if s := slugify(t); s != "" && s != "default" {
				cleaned = append(cleaned, s)
			}
		}
		if len(cleaned) > 0 {
			segs = append(segs, "tags:"+strings.Join(cleaned, ","))
		}
	}
	if s := strings.TrimSpace(source); s != "" {
		segs = append(segs, fmt.Sprintf("source:%s", s))
	}
	if len(segs) == 0 {
		return text
	}
	prefix.WriteString("[")
	prefix.WriteString(strings.Join(segs, " "))
	prefix.WriteString("] ")
	prefix.WriteString(text)
	return prefix.String()
}

// tagPrefixRe matches the leading `[...]` tag block produced by
// encodeMemoryText so recall output can strip it for display.
var tagPrefixRe = regexp.MustCompile(`^\[[^\]]*\]\s*`)

// stripTagPrefix removes the encoded tag prefix from stored text for
// human-friendly display. The full text including prefix is still retained
// elsewhere for debug purposes.
func stripTagPrefix(s string) string {
	return tagPrefixRe.ReplaceAllString(s, "")
}
