package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	suggestTimeout  = 2500 * time.Millisecond
	maxSuggestBytes = 1 << 20
)

type Suggestor struct {
	Config *Config
	Client *http.Client
}

func NewSuggestor(cfg *Config) *Suggestor {
	return &Suggestor{
		Config: cfg,
		Client: &http.Client{
			Timeout: suggestTimeout,
			// http.Client sets Referer to the full previous URL on redirect
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return errors.New("stopped after 10 redirects")
				}
				req.Header.Del("Referer")
				return nil
			},
		},
	}
}

func (s *Suggestor) Suggest(ctx context.Context, rawQuery, lang string) ([]string, []string, error) {
	trimmed := strings.TrimSpace(rawQuery)
	if trimmed == "" {
		return nil, nil, nil
	}

	if !strings.ContainsFunc(strings.TrimLeftFunc(rawQuery, unicode.IsSpace), unicode.IsSpace) {
		if rest, ok := cutPrefixFold(trimmed, s.Config.Prefix); ok {
			items, descs := s.suggestShortcuts(rest)
			return items, descs, nil
		}
	}

	res := s.Config.Resolve(rawQuery)
	if !res.Matched || res.Query == "" {
		return nil, nil, nil
	}

	suggestTmpl, suggestPath := s.Config.SuggestURL, ""
	if res.Explicit && res.Bong.SuggestURL != "" {
		suggestTmpl, suggestPath = res.Bong.SuggestURL, res.Bong.SuggestPath
	}
	if suggestTmpl == "" {
		return nil, nil, nil
	}

	targetURL := replacePlaceholder(suggestTmpl, encodeQuery(res.Query), lang)
	items, descs, err := s.fetchUpstream(ctx, targetURL, suggestPath)
	if err != nil {
		return nil, nil, err
	}

	if res.Explicit {
		tag := s.Config.Prefix + res.Key + " "
		for i, item := range items {
			items[i] = tag + item
		}
	}

	return items, descs, nil
}

type shortcutMatch struct {
	entry string
	title string
}

func (s *Suggestor) suggestShortcuts(rest string) ([]string, []string) {
	partial := strings.ToLower(rest)
	n := utf8.RuneCountInString(partial)
	var exact, prefix, word, jamoWord, substring []shortcutMatch

	for k, b := range s.Config.Bongs {
		m := shortcutMatch{entry: s.Config.Prefix + k, title: b.Title}
		kLower := strings.ToLower(k)
		literal, jamo := matchesBongWords(b, partial)
		switch {
		case s.Config.aliases[partial] == k:
			exact = append(exact, m)
		case strings.HasPrefix(kLower, partial) || hasPrefixAlias(b.Aliases, partial):
			prefix = append(prefix, m)
		case literal:
			word = append(word, m)
		case jamo:
			jamoWord = append(jamoWord, m)
		case n >= 2 && matchesSubstring(kLower, b, partial):
			substring = append(substring, m)
		}
	}

	byEntry := func(a, b shortcutMatch) int { return strings.Compare(a.entry, b.entry) }
	for _, ms := range [][]shortcutMatch{exact, prefix, word, jamoWord, substring} {
		slices.SortFunc(ms, byEntry)
	}

	combined := append(exact, prefix...)
	if n >= 2 || len(combined) == 0 {
		combined = slices.Concat(combined, word, jamoWord, substring)
	}

	if len(combined) == 0 && n >= 2 {
		var typo, typoWord []shortcutMatch
		for k, b := range s.Config.Bongs {
			m := shortcutMatch{entry: s.Config.Prefix + k, title: b.Title}
			switch typoMatchRank(strings.ToLower(k), b, partial) {
			case 1:
				typo = append(typo, m)
			case 2:
				typoWord = append(typoWord, m)
			}
		}
		slices.SortFunc(typo, byEntry)
		slices.SortFunc(typoWord, byEntry)
		combined = slices.Concat(typo, typoWord)
	}

	out := make([]string, len(combined))
	descs := make([]string, len(combined))
	for i, m := range combined {
		out[i] = m.entry
		descs[i] = m.title
	}
	return out, descs
}

func hasPrefixAlias(aliases []string, partial string) bool {
	for _, a := range aliases {
		if strings.HasPrefix(strings.ToLower(a), partial) {
			return true
		}
	}
	return false
}

func matchesSubstring(kLower string, b Bong, partial string) bool {
	for _, s := range slices.Concat([]string{kLower, b.Title}, b.Aliases, b.Keywords) {
		if strings.Contains(strings.ToLower(s), partial) {
			return true
		}
	}
	return false
}

var secondLevelDomainLabels = map[string]bool{
	"co": true, "com": true, "org": true, "net": true,
	"ac": true, "gov": true, "edu": true, "or": true, "ne": true, "go": true,
}

func hostWords(hostname string) []string {
	host := strings.TrimPrefix(strings.ToLower(hostname), "www.")
	labels := strings.Split(host, ".")
	if len(labels) >= 2 {
		tld := labels[len(labels)-1]
		labels = labels[:len(labels)-1]
		if len(tld) == 2 && len(labels) >= 2 && secondLevelDomainLabels[labels[len(labels)-1]] {
			labels = labels[:len(labels)-1]
		}
	}
	return splitWords(strings.Join(labels, "."))
}

func matchesBongWords(b Bong, partial string) (literal, jamo bool) {
	for _, text := range append([]string{b.Title}, b.Keywords...) {
		l, j := matchesTextWords(text, partial)
		literal, jamo = literal || l, jamo || j
	}
	for _, w := range hostWords(b.host) {
		literal = literal || strings.HasPrefix(w, partial)
	}
	return literal, jamo
}

func matchesTextWords(text, partial string) (literal, jamo bool) {
	partialJamo := hangulJamo(partial)
	words := splitWords(text)
	if len(words) > 1 {
		words = append(words, strings.Join(words, ""))
	}
	for _, w := range words {
		ch := hangulChosung(w)
		literal = literal || strings.HasPrefix(w, partial) || ch != "" && strings.HasPrefix(ch, partial)
		jamo = jamo || strings.HasPrefix(hangulJamo(w), partialJamo)
	}
	return literal, jamo
}

var (
	hangulInitials = [...]string{
		"ㄱ", "ㄲ", "ㄴ", "ㄷ", "ㄸ", "ㄹ", "ㅁ", "ㅂ", "ㅃ", "ㅅ",
		"ㅆ", "ㅇ", "ㅈ", "ㅉ", "ㅊ", "ㅋ", "ㅌ", "ㅍ", "ㅎ",
	}
	hangulMedials = [...]string{
		"ㅏ", "ㅐ", "ㅑ", "ㅒ", "ㅓ", "ㅔ", "ㅕ", "ㅖ", "ㅗ", "ㅗㅏ", "ㅗㅐ",
		"ㅗㅣ", "ㅛ", "ㅜ", "ㅜㅓ", "ㅜㅔ", "ㅜㅣ", "ㅠ", "ㅡ", "ㅡㅣ", "ㅣ",
	}
	hangulFinals = [...]string{
		"", "ㄱ", "ㄲ", "ㄱㅅ", "ㄴ", "ㄴㅈ", "ㄴㅎ", "ㄷ", "ㄹ", "ㄹㄱ",
		"ㄹㅁ", "ㄹㅂ", "ㄹㅅ", "ㄹㅌ", "ㄹㅍ", "ㄹㅎ", "ㅁ", "ㅂ", "ㅂㅅ", "ㅅ",
		"ㅆ", "ㅇ", "ㅈ", "ㅊ", "ㅋ", "ㅌ", "ㅍ", "ㅎ",
	}
)

func hangulJamo(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < '가' || r > '힣' {
			b.WriteRune(r)
			continue
		}
		i := r - '가'
		b.WriteString(hangulInitials[i/588])
		b.WriteString(hangulMedials[i%588/28])
		b.WriteString(hangulFinals[i%28])
	}
	return b.String()
}

func hangulChosung(word string) string {
	var b strings.Builder
	hasHangul := false
	for _, r := range word {
		if r >= '가' && r <= '힣' {
			b.WriteString(hangulInitials[(r-'가')/588])
			hasHangul = true
		} else {
			b.WriteRune(r)
		}
	}
	if !hasHangul {
		return ""
	}
	return b.String()
}

func typoMatchRank(kLower string, b Bong, partial string) int {
	for _, name := range append([]string{kLower}, b.Aliases...) {
		if withinOneEdit(strings.ToLower(name), partial) {
			return 1
		}
	}
	for _, w := range slices.Concat(splitWords(b.Title), splitWords(strings.Join(b.Keywords, " "))) {
		if prefixWithinOneEdit(w, partial) {
			return 2
		}
	}
	return 0
}

func prefixWithinOneEdit(word, partial string) bool {
	wRunes := []rune(word)
	pRunes := []rune(partial)
	if len(pRunes) < 3 || wRunes[0] != pRunes[0] {
		return false
	}
	for l := range len(wRunes) + 1 {
		if withinOneEditRunes(wRunes[:l], pRunes) {
			return true
		}
	}
	return false
}

func withinOneEdit(a, b string) bool {
	ar, br := []rune(a), []rune(b)
	return ar[0] == br[0] && withinOneEditRunes(ar, br)
}

func withinOneEditRunes(a, b []rune) bool {
	la, lb := len(a), len(b)
	diff := la - lb
	if diff < -1 || diff > 1 {
		return false
	}
	if la == lb {
		mismatches := 0
		lastIdx := -1
		for i := range la {
			if a[i] != b[i] {
				mismatches++
				if mismatches == 2 {
					if lastIdx != i-1 || a[lastIdx] != b[i] || a[i] != b[lastIdx] {
						return false
					}
				} else if mismatches > 2 {
					return false
				}
				lastIdx = i
			}
		}
		return true
	}
	if la > lb {
		a, b = b, a
		la, lb = lb, la
	}
	i, j, edits := 0, 0, 0
	for i < la && j < lb {
		if a[i] == b[j] {
			i++
			j++
		} else {
			edits++
			if edits > 1 {
				return false
			}
			j++
		}
	}
	return true
}

func splitWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

func (s *Suggestor) fetchUpstream(ctx context.Context, targetURL, suggestPath string) ([]string, []string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Bong/"+version+")")
	req.Header.Set("Accept", "application/x-suggestions+json, application/json")

	resp, err := s.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("suggest upstream %s: %w", req.URL.Host, errors.Unwrap(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("suggest upstream %s: %s", req.URL.Host, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSuggestBytes))
	if err != nil {
		return nil, nil, err
	}

	if suggestPath != "" {
		items, err := parseJSONPathSuggestions(body, suggestPath)
		return items, nil, err
	}
	return parseOpenSearchSuggestions(body)
}

func parseOpenSearchSuggestions(data []byte) ([]string, []string, error) {
	var payload []json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return nil, nil, err
	}
	if len(payload) < 2 {
		return nil, nil, fmt.Errorf("opensearch suggestions: got %d elements, want at least 2", len(payload))
	}

	var suggestions []string
	if err := json.Unmarshal(payload[1], &suggestions); err != nil {
		return nil, nil, err
	}

	var descriptions []string
	if len(payload) >= 3 {
		var rawDescs []string
		if err := json.Unmarshal(payload[2], &rawDescs); err == nil && len(rawDescs) == len(suggestions) {
			for _, d := range rawDescs {
				if strings.TrimSpace(d) != "" {
					descriptions = rawDescs
					break
				}
			}
		}
	}

	return suggestions, descriptions, nil
}

func parseJSONPathSuggestions(data []byte, path string) ([]string, error) {
	var root any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, err
	}
	segments := strings.Split(path, ".")
	var out []string
	walkJSONPath(root, segments, &out)
	return out, nil
}

func walkJSONPath(curr any, segments []string, out *[]string) {
	switch v := curr.(type) {
	case string:
		if len(segments) == 0 && v != "" {
			*out = append(*out, v)
		}
	case []any:
		if len(segments) > 0 {
			if i, err := strconv.Atoi(segments[0]); err == nil {
				if i >= 0 && i < len(v) {
					walkJSONPath(v[i], segments[1:], out)
				}
				return
			}
			if segments[0] == "*" {
				segments = segments[1:]
			}
		}
		for _, elem := range v {
			walkJSONPath(elem, segments, out)
		}
	case map[string]any:
		if len(segments) == 0 {
			return
		}
		if next, ok := v[segments[0]]; ok {
			walkJSONPath(next, segments[1:], out)
		}
	}
}
