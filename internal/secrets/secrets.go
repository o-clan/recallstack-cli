// Package secrets strips anything that looks like a credential before a string
// leaves this computer (contract section 12). The server strips again.
package secrets

import (
	"math"
	"regexp"
	"strings"
)

const Marker = "[secret]"

var (
	privateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(-----END [A-Z0-9 ]*PRIVATE KEY-----|\z)`)
	knownTokens     = []*regexp.Regexp{
		regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{10,}`),
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{16,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bxox[abpr]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}`),
		regexp.MustCompile(`\brsk_[A-Za-z0-9_-]{8,}`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`),
	}
	keyWords = `[A-Za-z0-9_.-]*(?i:secret|token|password|passwd|api[_-]?key|private[_-]?key|auth)[A-Za-z0-9_.-]*`
	// KEY=value, KEY="value", KEY='value'. The key is kept, the value replaced.
	assignment = regexp.MustCompile(`\b(` + keyWords + `)(\s*=\s*)("[^"\n]*"|'[^'\n]*'|[^\s"',;]+)`)
	// "key": "value" (JSON and similar).
	jsonPair = regexp.MustCompile(`("` + keyWords + `"\s*:\s*)"[^"\n]*"`)
	// Candidate high-entropy runs.
	entropyRun  = regexp.MustCompile(`[A-Za-z0-9+/=_-]{21,}`)
	pathSegment = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// Strip replaces every secret-looking part of value with [secret] and reports
// how many replacements it made.
func Strip(value string) (string, int) {
	if value == "" {
		return value, 0
	}
	count := 0
	replace := func(pattern *regexp.Regexp, input string) string {
		return pattern.ReplaceAllStringFunc(input, func(string) string {
			count++
			return Marker
		})
	}
	value = replace(privateKeyBlock, value)
	for _, pattern := range knownTokens {
		value = replace(pattern, value)
	}
	value = assignment.ReplaceAllStringFunc(value, func(match string) string {
		parts := assignment.FindStringSubmatch(match)
		if len(parts) < 4 || parts[3] == Marker || strings.Trim(parts[3], `"'`) == "" {
			return match
		}
		count++
		return parts[1] + parts[2] + Marker
	})
	value = jsonPair.ReplaceAllStringFunc(value, func(match string) string {
		parts := jsonPair.FindStringSubmatch(match)
		if len(parts) < 2 || strings.HasSuffix(match, `"`+Marker+`"`) || strings.HasSuffix(match, `""`) {
			return match
		}
		count++
		return parts[1] + `"` + Marker + `"`
	})
	value = entropyRun.ReplaceAllStringFunc(value, func(run string) string {
		if !highEntropy(run) {
			return run
		}
		count++
		return Marker
	})
	return value, count
}

// StripString is Strip without the count.
func StripString(value string) string {
	out, _ := Strip(value)
	return out
}

// Contains reports whether value has anything Strip would replace.
func Contains(value string) bool {
	_, count := Strip(value)
	return count > 0
}

func highEntropy(run string) bool {
	if strings.Contains(run, Marker) {
		return false
	}
	trimmed := strings.Trim(run, "=-_/")
	if len(trimmed) < 21 {
		return false
	}
	// Plain words and identifiers (no digits at all) aren't secrets.
	if !strings.ContainsAny(trimmed, "0123456789") && !strings.ContainsAny(trimmed, "+/=") {
		return false
	}
	// Paths and URL paths: every segment is a short name (the same rule as the
	// server). Segments like "e2e", "v2", "123" or "2026-10-18" are normal in
	// paths and URLs, so digits don't disqualify a segment; length does.
	if strings.Contains(trimmed, "/") {
		pathLike := true
		for _, segment := range strings.Split(trimmed, "/") {
			if segment == "" {
				continue
			}
			if len(segment) > 24 || !pathSegment.MatchString(segment) {
				pathLike = false
				break
			}
		}
		if pathLike {
			return false
		}
	}
	return shannon(trimmed) >= 3.5
}

func shannon(value string) float64 {
	counts := map[rune]int{}
	total := 0
	for _, r := range value {
		counts[r]++
		total++
	}
	entropy := 0.0
	for _, count := range counts {
		p := float64(count) / float64(total)
		entropy -= p * math.Log2(p)
	}
	return entropy
}
