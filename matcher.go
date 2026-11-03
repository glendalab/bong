package main

import (
	"net/url"
	"slices"
	"strings"
	"unicode"
)

type FormParam struct {
	Key   string
	Value string
}

type Resolution struct {
	Key      string
	Bong     Bong
	Query    string
	Explicit bool
	Matched  bool
}

func (c *Config) Resolve(rawQuery string) Resolution {
	trimmed := strings.TrimSpace(rawQuery)
	firstToken, rest := trimmed, ""
	if i := strings.IndexFunc(trimmed, unicode.IsSpace); i != -1 {
		firstToken = trimmed[:i]
		rest = strings.TrimSpace(trimmed[i:])
	}
	if key, b, ok := c.matchToken(firstToken); ok {
		return Resolution{
			Key:      key,
			Bong:     b,
			Query:    rest,
			Explicit: true,
			Matched:  true,
		}
	}

	if c.Default != "" {
		return Resolution{
			Key:     c.Default,
			Bong:    c.Bongs[c.Default],
			Query:   trimmed,
			Matched: true,
		}
	}

	return Resolution{Query: trimmed}
}

func (c *Config) matchToken(token string) (string, Bong, bool) {
	rawKey, ok := cutPrefixFold(token, c.Prefix)
	if !ok {
		return "", Bong{}, false
	}
	key, ok := c.aliases[strings.ToLower(rawKey)]
	if !ok {
		return "", Bong{}, false
	}
	return key, c.Bongs[key], true
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

func (r Resolution) TargetURL(lang string) string {
	if r.Query == "" {
		return r.Bong.MainURL
	}
	return replacePlaceholder(r.Bong.SearchURL, encodeQuery(r.Query), lang)
}

func (r Resolution) UsePost() bool {
	return r.Bong.Post && r.Query != ""
}

func (r Resolution) PostFormParams(lang string) []FormParam {
	params := make([]FormParam, 0, len(r.Bong.PostParams))
	for k, v := range r.Bong.PostParams {
		params = append(params, FormParam{
			Key:   k,
			Value: replacePlaceholder(v, r.Query, lang),
		})
	}
	slices.SortFunc(params, func(a, b FormParam) int {
		return strings.Compare(a.Key, b.Key)
	})
	return params
}

func encodeQuery(q string) string {
	escaped := url.QueryEscape(q)
	return strings.ReplaceAll(escaped, "+", "%20")
}
