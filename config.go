package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Title      string          `yaml:"title"`
	Listen     string          `yaml:"listen"`
	BaseURL    string          `yaml:"base_url"`
	Prefix     string          `yaml:"prefix"`
	Default    string          `yaml:"default"`
	SuggestURL string          `yaml:"suggest_url"`
	Include    []string        `yaml:"include"`
	Bongs      map[string]Bong `yaml:"bongs"`

	aliases map[string]string
}

type Bong struct {
	Title       string            `yaml:"title"`
	Aliases     []string          `yaml:"aliases"`
	Keywords    []string          `yaml:"keywords"`
	MainURL     string            `yaml:"main_url"`
	SearchURL   string            `yaml:"search_url"`
	SuggestURL  string            `yaml:"suggest_url"`
	SuggestPath string            `yaml:"suggest_path"`
	Post        bool              `yaml:"post"`
	PostKey     string            `yaml:"post_key"`
	PostParams  map[string]string `yaml:"post_params"`

	host string
}

// func-style unmarshal keeps the decoder's KnownFields
func (b *Bong) UnmarshalYAML(unmarshal func(any) error) error {
	var raw any
	if err := unmarshal(&raw); err != nil {
		return err
	}
	if s, ok := raw.(string); ok {
		b.SearchURL = s
		return nil
	}
	type bong Bong
	return unmarshal((*bong)(b))
}

type includeFile struct {
	Include []string        `yaml:"include"`
	Bongs   map[string]Bong `yaml:"bongs"`
}

func loadRawConfigFile(path string) (*Config, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve config path %q: %w", path, err)
	}
	cfg, err := decodeFile[Config](absPath)
	if err != nil {
		return nil, err
	}
	included, err := loadIncludes(absPath, cfg.Include, map[string]bool{absPath: true})
	if err != nil {
		return nil, err
	}
	for k, b := range included {
		if _, inRoot := cfg.Bongs[k]; !inRoot && b.SearchURL == "" {
			delete(included, k)
		}
	}
	mergeBongs(included, cfg.Bongs)
	cfg.Bongs = included
	return cfg, nil
}

func loadIncludes(fromPath string, includes []string, visited map[string]bool) (map[string]Bong, error) {
	included := make(map[string]Bong)
	for _, inc := range includes {
		incPath := filepath.Clean(inc)
		if !filepath.IsAbs(incPath) {
			incPath = filepath.Join(filepath.Dir(fromPath), incPath)
		}
		if visited[incPath] {
			return nil, fmt.Errorf("include cycle at %q", incPath)
		}
		f, err := decodeFile[includeFile](incPath)
		if err != nil {
			return nil, err
		}
		visited[incPath] = true
		nested, err := loadIncludes(incPath, f.Include, visited)
		delete(visited, incPath)
		if err != nil {
			return nil, err
		}
		mergeBongs(nested, f.Bongs)
		mergeBongs(included, nested)
	}
	return included, nil
}

func decodeFile[T any](path string) (*T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	v, err := decodeYAML[T](f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return v, nil
}

func mergeBongs(dst, src map[string]Bong) {
	for k, b := range src {
		if existing, ok := dst[k]; ok {
			b = mergeBong(existing, b)
		}
		dst[k] = b
	}
}

func mergeBong(base, over Bong) Bong {
	if over.Title != "" {
		base.Title = over.Title
	}
	base.Aliases = appendUnique(base.Aliases, over.Aliases)
	base.Keywords = appendUnique(base.Keywords, over.Keywords)
	if over.SearchURL != "" && strings.TrimSpace(over.SearchURL) != strings.TrimSpace(base.SearchURL) {
		if base.SearchURL != "" {
			base.MainURL, base.SuggestURL, base.SuggestPath = "", "", ""
			base.Post, base.PostKey, base.PostParams = false, "", nil
		}
		base.SearchURL = over.SearchURL
	}
	base.Post = base.Post || over.Post
	if over.PostKey != "" || len(over.PostParams) > 0 {
		base.PostKey, base.PostParams = over.PostKey, over.PostParams
	}
	if over.MainURL != "" {
		base.MainURL = over.MainURL
	}
	if over.SuggestURL != "" {
		base.SuggestURL, base.SuggestPath = over.SuggestURL, over.SuggestPath
	} else if over.SuggestPath != "" {
		base.SuggestPath = over.SuggestPath
	}
	return base
}

func appendUnique(dst, src []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, s := range dst {
		seen[s] = true
	}
	for _, s := range src {
		if !seen[s] {
			seen[s] = true
			dst = append(dst, s)
		}
	}
	return dst
}

func decodeYAML[T any](r io.Reader) (*T, error) {
	var v T
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&v); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config file is empty")
		}
		return nil, fmt.Errorf("parse config yaml: %w", err)
	}
	return &v, nil
}

func (c *Config) NormalizeAndValidate() error {
	c.Title = strings.TrimSpace(c.Title)
	if c.Title == "" {
		c.Title = "Bong"
	}

	c.Listen = strings.TrimSpace(c.Listen)
	if c.Listen == "" {
		c.Listen = ":8080"
	}

	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if c.BaseURL != "" {
		u, err := url.Parse(c.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || strings.ContainsAny(c.BaseURL, "?#") {
			return fmt.Errorf("base_url %q must be an http or https URL without query or fragment", c.BaseURL)
		}
	}

	c.Prefix = strings.TrimSpace(c.Prefix)
	if c.Prefix == "" {
		c.Prefix = "."
	}
	if strings.ContainsFunc(c.Prefix, unicode.IsSpace) {
		return fmt.Errorf("prefix %q must not contain whitespace", c.Prefix)
	}

	c.SuggestURL = strings.TrimSpace(c.SuggestURL)
	if c.SuggestURL != "" {
		if err := validateTemplateURL(c.SuggestURL); err != nil {
			return fmt.Errorf("invalid top-level suggest_url: %w", err)
		}
	}

	if len(c.Bongs) == 0 {
		return errors.New("no bongs defined")
	}

	primaryKeys := make([]string, 0, len(c.Bongs))
	for key, b := range c.Bongs {
		if key == "" || strings.ContainsFunc(key, unicode.IsSpace) {
			return fmt.Errorf("bong key %q must be non-empty and contain no whitespace", key)
		}

		b.Title = strings.TrimSpace(b.Title)
		if b.Title == "" {
			b.Title = key
		}

		b.SearchURL = strings.TrimSpace(b.SearchURL)
		if b.SearchURL == "" {
			return fmt.Errorf("bong %q: search_url is required", key)
		}

		parsedSearch, err := parseHTTPURL(b.SearchURL)
		if err != nil {
			return fmt.Errorf("bong %q: invalid search_url: %w", key, err)
		}

		mainURL := parsedSearch
		b.MainURL = strings.TrimSpace(b.MainURL)
		if b.MainURL == "" {
			b.MainURL = parsedSearch.Scheme + "://" + parsedSearch.Host
		} else if hasAnyPlaceholder(b.MainURL) {
			return fmt.Errorf("bong %q: main_url %q takes no placeholder", key, b.MainURL)
		} else if mainURL, err = parseHTTPURL(b.MainURL); err != nil {
			return fmt.Errorf("bong %q: invalid main_url: %w", key, err)
		}
		b.host = mainURL.Hostname()

		b.SuggestURL = strings.TrimSpace(b.SuggestURL)
		b.SuggestPath = strings.TrimSpace(b.SuggestPath)
		if b.SuggestURL != "" {
			if err := validateTemplateURL(b.SuggestURL); err != nil {
				return fmt.Errorf("bong %q: invalid suggest_url: %w", key, err)
			}
		} else if b.SuggestPath != "" {
			return fmt.Errorf("bong %q: suggest_path requires suggest_url", key)
		}

		b.PostKey = strings.TrimSpace(b.PostKey)
		if b.PostKey != "" || len(b.PostParams) > 0 {
			b.Post = true
		}

		if b.Post {
			if err := normalizePostBong(key, &b); err != nil {
				return err
			}
		} else if !hasPlaceholder(b.SearchURL) {
			return fmt.Errorf("bong %q: search_url %q needs a %%s, {q} or {searchTerms} placeholder, or post: true", key, b.SearchURL)
		}

		cleanAliases := make([]string, 0, len(b.Aliases))
		for _, rawAlias := range b.Aliases {
			alias := strings.TrimSpace(rawAlias)
			if alias == "" || strings.ContainsFunc(alias, unicode.IsSpace) {
				return fmt.Errorf("bong %q: alias %q must be non-empty and contain no whitespace", key, rawAlias)
			}
			cleanAliases = append(cleanAliases, alias)
		}
		cleanKeywords := make([]string, 0, len(b.Keywords))
		for _, rawKw := range b.Keywords {
			if kw := strings.TrimSpace(rawKw); kw != "" {
				cleanKeywords = append(cleanKeywords, kw)
			}
		}
		b.Aliases = cleanAliases
		b.Keywords = cleanKeywords
		c.Bongs[key] = b
		primaryKeys = append(primaryKeys, key)
	}

	slices.Sort(primaryKeys)

	c.aliases = make(map[string]string)
	for _, key := range primaryKeys {
		lower := strings.ToLower(key)
		if existing, exists := c.aliases[lower]; exists {
			return fmt.Errorf("bong keys %q and %q differ only in case", existing, key)
		}
		c.aliases[lower] = key
	}
	for _, key := range primaryKeys {
		for _, alias := range c.Bongs[key].Aliases {
			lower := strings.ToLower(alias)
			if existing, exists := c.aliases[lower]; exists && existing != key {
				return fmt.Errorf("bong %q: alias %q already names bong %q", key, alias, existing)
			}
			c.aliases[lower] = key
		}
	}

	c.Default = strings.TrimSpace(c.Default)
	if c.Default != "" {
		key, ok := c.aliases[strings.ToLower(c.Default)]
		if !ok {
			return fmt.Errorf("default bong %q does not exist", c.Default)
		}
		c.Default = key
	}

	return nil
}

func normalizePostBong(key string, b *Bong) error {
	if b.PostKey != "" && len(b.PostParams) > 0 {
		return fmt.Errorf("bong %q: set post_key or post_params, not both", key)
	}
	if (b.PostKey != "" || len(b.PostParams) > 0) && hasPlaceholder(b.SearchURL) {
		return fmt.Errorf("bong %q: with post_key or post_params, search_url %q takes no query placeholder", key, b.SearchURL)
	}
	if len(b.PostParams) == 0 {
		if b.PostKey != "" {
			b.PostParams = map[string]string{b.PostKey: "%s"}
		} else if baseURL, rawQuery, ok := strings.Cut(b.SearchURL, "?"); ok && rawQuery != "" {
			values, err := url.ParseQuery(strings.ReplaceAll(rawQuery, "%s", "%25s"))
			if err != nil {
				return fmt.Errorf("bong %q: parse search_url query: %w", key, err)
			}
			b.PostParams = make(map[string]string, len(values))
			for k, v := range values {
				b.PostParams[k] = v[len(v)-1]
			}
			b.SearchURL = baseURL
		} else {
			return fmt.Errorf("bong %q: post needs post_key, post_params or query parameters in search_url", key)
		}
	}

	for _, val := range b.PostParams {
		if hasPlaceholder(val) {
			return nil
		}
	}
	return fmt.Errorf("bong %q: post_params need a %%s, {q} or {searchTerms} placeholder", key)
}

func hasPlaceholder(s string) bool {
	return strings.Contains(s, "%s") ||
		strings.Contains(s, "{q}") ||
		strings.Contains(s, "{searchTerms}")
}

func hasAnyPlaceholder(s string) bool {
	return hasPlaceholder(s) || strings.Contains(s, "{language}")
}

func replacePlaceholder(tmpl, val, lang string) string {
	return strings.NewReplacer("%s", val, "{q}", val, "{searchTerms}", val, "{language}", lang).Replace(tmpl)
}

func parseHTTPURL(raw string) (*url.URL, error) {
	_, authority, _ := strings.Cut(raw, "://")
	if end := strings.IndexAny(authority, "/?#"); end >= 0 {
		authority = authority[:end]
	}
	if hasAnyPlaceholder(authority) {
		return nil, fmt.Errorf("URL %q has a placeholder in its host", raw)
	}
	u, err := url.Parse(replacePlaceholder(raw, "q", "en"))
	if err != nil {
		return nil, fmt.Errorf("URL %q: %w", raw, errors.Unwrap(err))
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("URL %q must have http or https scheme and a host", raw)
	}
	return u, nil
}

func validateTemplateURL(raw string) error {
	if _, err := parseHTTPURL(raw); err != nil {
		return err
	}
	if !hasPlaceholder(raw) {
		return fmt.Errorf("URL %q needs a %%s, {q} or {searchTerms} placeholder", raw)
	}
	return nil
}
