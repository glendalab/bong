package main

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testYAML = `
title: "Test & <Bong>"
prefix: "."
default: g
bongs:
  g:
    title: Google
    aliases: [goog]
    main_url: "https://www.google.com"
    search_url: "https://www.google.com/search?q=%s&ie=UTF-8"
  w:
    title: Wikipedia
    main_url: "https://en.wikipedia.org"
    search_url: "https://en.wikipedia.org/w/index.php?search={q}"
  p:
    title: PostEngine
    main_url: "https://search.example.com"
    search_url: "https://search.example.com/search"
    post_key: "q"
  p2:
    search_url: "https://search.example.com/search?q=%s&cat=web"
    post: true
  r: "https://www.reddit.com/r/%s"
`

func validated(cfg *Config, err error) (*Config, error) {
	if err != nil {
		return nil, err
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func mustParseConfig(t *testing.T, yaml string) *Config {
	t.Helper()
	cfg, err := validated(decodeYAML[Config](strings.NewReader(yaml)))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestConfigLoadAndNormalization(t *testing.T) {
	cfg := mustParseConfig(t, testYAML)

	rBong, ok := cfg.Bongs["r"]
	if !ok {
		t.Fatal("missing 'r' bong")
	}
	if rBong.MainURL != "https://www.reddit.com" {
		t.Errorf("r.MainURL = %q, want https://www.reddit.com", rBong.MainURL)
	}
	if rBong.Title != "r" {
		t.Errorf("r.Title = %q, want r", rBong.Title)
	}

	p2 := cfg.Bongs["p2"]
	if !p2.Post {
		t.Error("p2.Post = false, want true")
	}
	if p2.SearchURL != "https://search.example.com/search" {
		t.Errorf("p2.SearchURL = %q, want https://search.example.com/search", p2.SearchURL)
	}
	if p2.PostParams["q"] != "%s" || p2.PostParams["cat"] != "web" {
		t.Errorf("unexpected p2.PostParams: %+v", p2.PostParams)
	}
}

func TestConfigValidationErrors(t *testing.T) {
	const g = "bongs:\n  g: https://google.com/search?q=%s\n"
	cases := []struct {
		name, yaml, want string
	}{
		{"empty config", "", "config file is empty"},
		{"no bongs", "title: Bong\nbongs: {}\n", "no bongs defined"},
		{"missing placeholder in GET search_url", "bongs:\n  g: https://google.com/search\n", "placeholder"},
		{"only {language} in GET search_url", "bongs:\n  g: https://google.com/{language}/search\n", "placeholder"},
		{"only {language} in post_params", "bongs:\n  p:\n    search_url: https://search.example.com/search\n    post_params:\n      hl: \"{language}\"\n", "post_params need a"},
		{"missing default bong", "default: missing\n" + g, `default bong "missing" does not exist`},
		{"unknown field", "unknown_key: 123\n" + g, "field unknown_key not found"},
		{"post without params", "bongs:\n  p:\n    search_url: https://search.example.com/search\n    post: true\n", "post needs post_key"},
		{"missing search_url with aliases", "bongs:\n  g:\n    aliases: [goog]\n", "search_url is required"},
		{"padded key", "bongs:\n  \" g \": https://google.com/search?q=%s\n", "contain no whitespace"},
		{"suggest_path without suggest_url", "bongs:\n  g:\n    search_url: https://google.com/search?q=%s\n    suggest_path: items.name\n", "suggest_path requires suggest_url"},
		{"base_url without scheme", "base_url: bong.example.org\n" + g, "base_url"},
		{"base_url with query", "base_url: https://bong.example.org/?x=1\n" + g, "base_url"},
		{"misspelled bong field", "bongs:\n  g:\n    search_url: https://google.com/search?q=%s\n    sugest_url: https://google.com/complete?q=%s\n", "field sugest_url not found"},
		{"alias naming another bong", "bongs:\n  g: https://google.com/search?q=%s\n  gh:\n    search_url: https://github.com/search?q=%s\n    aliases: [g]\n", `alias "g" already names bong "g"`},
		{"keys differing only in case", "bongs:\n  G: https://google.com/search?q=%s\n  g: https://google.com/search?q=%s\n", `bong keys "G" and "g" differ only in case`},
		{"%s in search_url host", "bongs:\n  g: https://%s.example.com/\n", "placeholder in its host"},
		{"{language} in search_url host", "bongs:\n  g: https://{language}.wikipedia.org/w/index.php?search=%s\n", "placeholder in its host"},
		{"post_key and post_params", "bongs:\n  p:\n    search_url: https://p.example/s\n    post_key: q\n    post_params: {query: \"%s\"}\n", "not both"},
		{"query placeholder in POST target", "bongs:\n  p:\n    search_url: https://p.example/s?q=%s\n    post_key: q\n", "takes no query placeholder"},
		{"placeholder in main_url", "bongs:\n  g:\n    search_url: https://google.com/search?q=%s\n    main_url: https://google.com/%s\n", "main_url"},
		{"{language} in main_url", "bongs:\n  g:\n    search_url: https://google.com/search?q=%s\n    main_url: https://google.com/{language}\n", "main_url"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validated(decodeYAML[Config](strings.NewReader(tc.yaml)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestResolveAndTargetURL(t *testing.T) {
	cfg := mustParseConfig(t, testYAML)

	cases := []struct {
		query        string
		wantKey      string
		wantQuery    string
		wantExplicit bool
		wantTarget   string
		wantPost     bool
	}{
		{
			query:        ".w seoul subway",
			wantKey:      "w",
			wantQuery:    "seoul subway",
			wantExplicit: true,
			wantTarget:   "https://en.wikipedia.org/w/index.php?search=seoul%20subway",
		},
		{
			query:        ".W seoul subway",
			wantKey:      "w",
			wantQuery:    "seoul subway",
			wantExplicit: true,
			wantTarget:   "https://en.wikipedia.org/w/index.php?search=seoul%20subway",
		},
		{
			query:        ".goog golang",
			wantKey:      "g",
			wantQuery:    "golang",
			wantExplicit: true,
			wantTarget:   "https://www.google.com/search?q=golang&ie=UTF-8",
		},
		{
			query:        ".w",
			wantKey:      "w",
			wantQuery:    "",
			wantExplicit: true,
			wantTarget:   "https://en.wikipedia.org",
		},
		{
			query:        "how to open .r",
			wantKey:      "g",
			wantQuery:    "how to open .r",
			wantExplicit: false,
			wantTarget:   "https://www.google.com/search?q=how%20to%20open%20.r&ie=UTF-8",
		},
		{
			query:        ".gitignore template",
			wantKey:      "g",
			wantQuery:    ".gitignore template",
			wantExplicit: false,
			wantTarget:   "https://www.google.com/search?q=.gitignore%20template&ie=UTF-8",
		},
		{
			query:        "100% pure golang",
			wantKey:      "g",
			wantQuery:    "100% pure golang",
			wantExplicit: false,
			wantTarget:   "https://www.google.com/search?q=100%25%20pure%20golang&ie=UTF-8",
		},
		{
			query:        ".p post query",
			wantKey:      "p",
			wantQuery:    "post query",
			wantExplicit: true,
			wantPost:     true,
		},
		{
			query:        ".p",
			wantKey:      "p",
			wantQuery:    "",
			wantExplicit: true,
			wantTarget:   "https://search.example.com",
		},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			res := cfg.Resolve(tc.query)
			if !res.Matched {
				t.Fatalf("Resolve(%q) did not match", tc.query)
			}
			if res.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", res.Key, tc.wantKey)
			}
			if res.Query != tc.wantQuery {
				t.Errorf("Query = %q, want %q", res.Query, tc.wantQuery)
			}
			if res.Explicit != tc.wantExplicit {
				t.Errorf("Explicit = %v, want %v", res.Explicit, tc.wantExplicit)
			}
			if res.UsePost() != tc.wantPost {
				t.Errorf("UsePost() = %v, want %v", res.UsePost(), tc.wantPost)
			}
			if got := res.TargetURL("en"); !tc.wantPost && got != tc.wantTarget {
				t.Errorf("TargetURL() = %q, want %q", got, tc.wantTarget)
			}
		})
	}
}

func TestPrefixCaseInsensitive(t *testing.T) {
	cfg := mustParseConfig(t, `
prefix: "b:"
default: d
bongs:
  d: "https://default.example/?q=%s"
  g: "https://www.google.com/search?q=%s"
`)
	res := cfg.Resolve("B:g foo")
	if res.Key != "g" || !res.Explicit || res.Query != "foo" {
		t.Errorf("Resolve(B:g foo) = key %q explicit %v query %q, want g true foo", res.Key, res.Explicit, res.Query)
	}
}

func TestAcceptLanguage(t *testing.T) {
	cases := map[string]string{
		"":                           "en",
		"*":                          "en",
		"ko-KR,ko;q=0.9,en-US;q=0.8": "ko",
		"en;q=0.5, DE-at":            "de",
		"zh-Hant-TW;q=0.7, ja;q=0.7": "zh",
		"fr;q=0, it;q=0.1":           "it",
		"fr;q=0":                     "en",
		"en;q=0.5, de;Q=0.9":         "de",
		"en;q=0.5, ja ; q=0.600":     "ja",
		"es;q=bogus, nl;q=0.2":       "nl",
		"en;q=0.5, fr;q=1.5":         "en",
		"x-foo, 12":                  "en",
	}
	for header, want := range cases {
		if got := acceptLanguage([]string{header}); got != want {
			t.Errorf("acceptLanguage(%q) = %q, want %q", header, got, want)
		}
	}
	if got := acceptLanguage([]string{"en;q=0.1", "ko"}); got != "ko" {
		t.Errorf("acceptLanguage(two headers) = %q, want ko", got)
	}
}

func TestLanguagePlaceholder(t *testing.T) {
	cfg := mustParseConfig(t, `
bongs:
  dl: "https://www.deepl.com/translator?targetLanguage={language}&sourceText=%s"
  pl:
    search_url: "https://post.example/{language}/search"
    post_params:
      q: "%s"
      hl: "{language}"
`)
	srv := NewServer(cfg, discardLogger())
	search := func(q, acceptLang string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/s?q="+url.QueryEscape(q), nil)
		if acceptLang != "" {
			req.Header.Set("Accept-Language", acceptLang)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	for acceptLang, want := range map[string]string{
		"ko-KR,ko;q=0.9,en-US;q=0.8": "https://www.deepl.com/translator?targetLanguage=ko&sourceText=a%2Fb",
		"":                           "https://www.deepl.com/translator?targetLanguage=en&sourceText=a%2Fb",
	} {
		if got := search(".dl a/b", acceptLang).Header().Get("Location"); got != want {
			t.Errorf("Accept-Language %q: Location = %q, want %q", acceptLang, got, want)
		}
	}

	body := search(".pl {language} docs", "de-DE").Body.String()
	if !strings.Contains(body, `action="https://post.example/de/search"`) ||
		!strings.Contains(body, `name="hl" value="de"`) ||
		!strings.Contains(body, `name="q" value="{language} docs"`) {
		t.Errorf("unexpected POST form HTML: %s", body)
	}
}

func TestHostWords(t *testing.T) {
	cases := map[string][]string{
		"pkg.go.dev":       {"pkg", "go"},
		"www.bbc.co.uk":    {"bbc"},
		"search.naver.com": {"search", "naver"},
	}
	for host, want := range cases {
		if got := hostWords(host); !slices.Equal(got, want) {
			t.Errorf("hostWords(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestDrawerListsDefaultFirst(t *testing.T) {
	cfg := mustParseConfig(t, testYAML)
	cfg.Default = "w"
	var keys []string
	for _, item := range buildBongItems(cfg) {
		keys = append(keys, item.Key)
	}
	if want := []string{"w", "g", "p", "p2", "r"}; !slices.Equal(keys, want) {
		t.Errorf("drawer order = %v, want %v", keys, want)
	}
}

func TestTypos(t *testing.T) {
	for typed, want := range map[string]bool{
		"google": true, "gogle": true, "googles": true, "goggle": true, "googel": true,
		"ogogle": false, "gogel": false, "ggoole": false, "goolke": false, "gookge": false,
	} {
		if got := withinOneEdit("google", typed); got != want {
			t.Errorf("withinOneEdit(google, %s) = %v, want %v", typed, got, want)
		}
	}
	for typed, want := range map[string]bool{
		"wkip": true, "wkii": true,
		"wk": false, "wipe": false, "xiki": false,
	} {
		if got := prefixWithinOneEdit("wikipedia", typed); got != want {
			t.Errorf("prefixWithinOneEdit(wikipedia, %s) = %v, want %v", typed, got, want)
		}
	}
}

func TestServerHandlers(t *testing.T) {
	cfg := mustParseConfig(t, testYAML)
	srv := NewServer(cfg, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET / status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "<title>Test &amp; &lt;Bong&gt;</title>") ||
		!strings.Contains(body, `const prefix = ".";`) {
		t.Errorf("index missing escaped title or prefix:\n%s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "/s?q=.r+golang", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Errorf("GET /s status = %d, want %d", rec.Code, http.StatusFound)
	}
	if got := rec.Header().Get("Location"); got != "https://www.reddit.com/r/golang" {
		t.Errorf("Location = %q, want https://www.reddit.com/r/golang", got)
	}
	if got := rec.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/s?q=.p+hello+world", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("POST bong status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `action="https://search.example.com/search"`) ||
		!strings.Contains(body, `name="q" value="hello world"`) {
		t.Errorf("unexpected POST form HTML: %s", body)
	}

	req = httptest.NewRequest(http.MethodGet, "https://bong.example.org/opensearch.xml", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /opensearch.xml status = %d, want 200", rec.Code)
	}
	var osDoc struct {
		XMLName xml.Name `xml:"OpenSearchDescription"`
		Short   string   `xml:"ShortName"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &osDoc); err != nil {
		t.Fatalf("opensearch.xml is not valid XML: %v\n%s", err, rec.Body.String())
	}
	if osDoc.Short != "Test & <Bong>" {
		t.Errorf("ShortName = %q, want %q", osDoc.Short, "Test & <Bong>")
	}
	if !strings.Contains(rec.Body.String(), "https://bong.example.org/s?q={searchTerms}") ||
		!strings.Contains(rec.Body.String(), "https://bong.example.org/suggest?q={searchTerms}") ||
		!strings.Contains(rec.Body.String(), "https://bong.example.org/icon.svg") {
		t.Errorf("opensearch.xml missing expected URLs:\n%s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/opensearch.xml", nil)
	req.Host = ""
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("GET /opensearch.xml without Host status = %d, want 400", rec.Code)
	}

	for _, tc := range []struct {
		method, target string
		want           int
		allow          string
	}{
		{http.MethodHead, "/", http.StatusOK, ""},
		{http.MethodPost, "/search?q=.r+golang", http.StatusFound, ""},
		{http.MethodPut, "/", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodPut, "/s", http.StatusMethodNotAllowed, "GET, HEAD, POST"},
		{http.MethodPost, "/suggest?q=x", http.StatusMethodNotAllowed, "GET, HEAD"},
		{http.MethodGet, "/nope", http.StatusNotFound, ""},
	} {
		rec = httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code != tc.want || rec.Header().Get("Allow") != tc.allow {
			t.Errorf("%s %s = %d Allow %q, want %d Allow %q", tc.method, tc.target, rec.Code, rec.Header().Get("Allow"), tc.want, tc.allow)
		}
	}

	req = httptest.NewRequest(http.MethodGet, "/icon.svg", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("GET /icon.svg status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/svg+xml" {
		t.Errorf("GET /icon.svg Content-Type = %q, want image/svg+xml", got)
	}
	if !strings.Contains(rec.Body.String(), "<svg") {
		t.Errorf("GET /icon.svg body does not contain <svg")
	}
}

func TestSuggestHandler(t *testing.T) {
	referers := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "/?q="+url.QueryEscape(q), http.StatusFound)
			return
		}
		if q == "hopped" {
			referers <- r.Referer()
		}
		if q == "fail" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/x-suggestions+json")
		_ = json.NewEncoder(w).Encode([]any{q, []string{q + " one", q + " two"}})
	}))
	defer upstream.Close()

	cfg := &Config{
		Prefix:  ".",
		Default: "g",
		Bongs: map[string]Bong{
			"g": {
				Title:      "Google",
				Keywords:   []string{"구글"},
				MainURL:    "https://www.google.com",
				SearchURL:  "https://www.google.com/search?q=%s",
				SuggestURL: upstream.URL + "?q=%s",
			},
			"w": {
				Title:      "Wikipedia",
				MainURL:    "https://en.wikipedia.org",
				SearchURL:  "https://en.wikipedia.org/w/index.php?search=%s",
				SuggestURL: upstream.URL + "?q=%s",
			},
			"p":  {SearchURL: "https://p.example/?q=%s"},
			"p2": {SearchURL: "https://p2.example/?q=%s"},
			"r": {
				SearchURL:  "https://r.example/?q=%s",
				SuggestURL: upstream.URL + "/hop?q=%s",
			},
			"l": {
				SearchURL:  "https://l.example/?q=%s",
				SuggestURL: upstream.URL + "?q=%s-{language}",
			},
			"ndic": {
				Title:     "Naver Dictionary",
				Keywords:  []string{"국어사전"},
				SearchURL: "https://dict.naver.com/dict.search?query=%s",
			},
			"yt": {
				Title:     "YouTube",
				Keywords:  []string{"유튜브", "유튜브 뮤직"},
				MainURL:   "https://www.youtube.com",
				SearchURL: "https://www.youtube.com/results?search_query=%s",
			},
		},
	}
	if err := cfg.NormalizeAndValidate(); err != nil {
		t.Fatalf("NormalizeAndValidate: %v", err)
	}

	srv := NewServer(cfg, discardLogger())

	get := func(q, acceptLang string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/suggest?q="+url.QueryEscape(q), nil)
		if acceptLang != "" {
			req.Header.Set("Accept-Language", acceptLang)
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	cases := []struct {
		q          string
		acceptLang string
		want       []string
		wantDescs  []string
	}{
		{q: "nix", want: []string{}},
		{q: ".g nix", want: []string{".g nix one", ".g nix two"}},
		{q: ".p", want: []string{".p", ".p2"}},
		{q: ".wiki", want: []string{".w"}, wantDescs: []string{"Wikipedia"}},
		{q: ".yr", want: []string{".yt"}},
		{q: ".유튜브", want: []string{".yt"}},
		{q: ".ㅇㅌㅂ", want: []string{".yt"}},
		{q: ".윹", want: []string{".yt"}},
		{q: ".유튭", want: []string{".yt"}},
		{q: ".구그", want: []string{".g"}},
		{q: ".국", want: []string{".ndic", ".g"}},
		{q: ".유튜브뮤직", want: []string{".yt"}},
		{q: ".w ", want: []string{}},
		{q: ".r hopped", want: []string{".r hopped one", ".r hopped two"}},
		{q: ".l x", want: []string{".l x-en one", ".l x-en two"}},
		{q: ".l y", acceptLang: "de-DE,de;q=0.9", want: []string{".l y-de one", ".l y-de two"}},
	}
	for _, tc := range cases {
		t.Run(tc.q, func(t *testing.T) {
			rec := get(tc.q, tc.acceptLang)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var resp []json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			var items []string
			if err := json.Unmarshal(resp[1], &items); err != nil {
				t.Fatalf("decode suggestions: %v", err)
			}
			if !slices.Equal(items, tc.want) {
				t.Errorf("suggestions = %v, want %v", items, tc.want)
			}
			if tc.wantDescs == nil {
				return
			}
			var descs []string
			if err := json.Unmarshal(resp[2], &descs); err != nil {
				t.Fatalf("decode descriptions: %v", err)
			}
			if !slices.Equal(descs, tc.wantDescs) {
				t.Errorf("descriptions = %v, want %v", descs, tc.wantDescs)
			}
		})
	}

	var wiki []json.RawMessage
	if err := json.Unmarshal(get(".wiki", "").Body.Bytes(), &wiki); err != nil || len(wiki) != 5 {
		t.Fatalf(".wiki response: err %v, %d elements, want 5", err, len(wiki))
	}
	var meta struct {
		Types   []string            `json:"google:suggesttype"`
		Details []map[string]string `json:"google:suggestdetail"`
	}
	if err := json.Unmarshal(wiki[4], &meta); err != nil {
		t.Fatalf("decode suggest metadata: %v", err)
	}
	if !slices.Equal(meta.Types, []string{"ENTITY"}) || len(meta.Details) != 1 || meta.Details[0]["google:entityinfo"] != "EglXaWtpcGVkaWE=" {
		t.Errorf(".wiki metadata = %+v, want one ENTITY with entityinfo EglXaWtpcGVkaWE=", meta)
	}

	cfg.SuggestURL = upstream.URL + "?q=%s"
	if rec := get("fail", ""); rec.Code != http.StatusBadGateway {
		t.Errorf("plain query, top-level suggest_url, upstream 500: status = %d, want 502", rec.Code)
	}
	if ref := <-referers; ref != "" {
		t.Errorf("Referer after upstream redirect = %q, want none", ref)
	}
}

func TestUpstreamErrorOmitsQuery(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	_, _, err := NewSuggestor(&Config{}).fetchUpstream(context.Background(), closed.URL+"/?q=secret", "")
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("err = %v, want a connection error without the query", err)
	}
}

func TestJSONPathSuggestions(t *testing.T) {
	cases := []struct {
		raw, path string
		want      []string
	}{
		{`{"crates":[{"name":"serde"},{"name":"serde_json"}]}`, "crates.name", []string{"serde", "serde_json"}},
		{`{"suggestions":["a","b"]}`, "suggestions", []string{"a", "b"}},
		{`{"items":[[[["hello"],[""],["안녕"]],[["world"],[""],["세계"]]]]}`, "items.0.*.0.0", []string{"hello", "world"}},
	}
	for _, tc := range cases {
		got, err := parseJSONPathSuggestions([]byte(tc.raw), tc.path)
		if err != nil {
			t.Fatalf("parseJSONPathSuggestions(%s): %v", tc.path, err)
		}
		if !slices.Equal(got, tc.want) {
			t.Errorf("parseJSONPathSuggestions(%s) = %v, want %v", tc.path, got, tc.want)
		}
	}
}

func TestConfigInclude(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	write("kr.yaml", `
include:
  - "nested.yaml"
bongs:
  n:
    search_url: "https://search.naver.com/search.naver?query=%s"
    suggest_url: "https://ac.search.naver.com/nx/ac?q=%s"
  nw: "https://namu.wiki/Go?q=%s"
  daum:
    search_url: "https://search.daum.net/search?q=%s"
    suggest_url: "https://suggest.search.daum.net/sushi/opensearch/pc?q=%s"
  g:
    keywords: ["구글"]
    main_url: "https://www.google.co.kr"
  unused:
    keywords: ["미사용"]
  pp:
    search_url: "https://p.example/search"
    post_params: {q: "%s", src: kr}
  np:
    search_url: "https://a.example/search"
    main_url: "https://a.example/home"
    suggest_url: "https://a.example/suggest?q=%s"
    post_key: q
`)
	write("nested.yaml", `
bongs:
  d:
    aliases: [dd]
  orphan:
    aliases: [o]
`)
	mainFile := write("config.yaml", `
default: n
include:
  - "kr.yaml"
bongs:
  g: "https://www.google.com/search?q=%s"
  n: "https://search.naver.com/search.naver?query=%s"
  daum: "https://search.daum.net/search?q=%s "
  nw: "https://custom.namu.wiki/Go?q=%s"
  np: "https://b.example/?q=%s"
  pp:
    search_url: "https://p.example/search"
    post_key: query
  d: "https://d.example/?q=%s"
`)

	cfg, err := validated(loadRawConfigFile(mainFile))
	if err != nil {
		t.Fatalf("load config with include: %v", err)
	}

	if got := cfg.Bongs["n"].SuggestURL; got != "https://ac.search.naver.com/nx/ac?q=%s" {
		t.Errorf("n.SuggestURL = %q, want the included one", got)
	}
	if got := cfg.Bongs["daum"].SuggestURL; got != "https://suggest.search.daum.net/sushi/opensearch/pc?q=%s" {
		t.Errorf("daum.SuggestURL = %q, want the included one, since the root search_url differs only by a trailing space", got)
	}
	for _, k := range []string{"unused", "orphan"} {
		if _, ok := cfg.Bongs[k]; ok {
			t.Errorf("overlay %q kept, want dropped", k)
		}
	}
	if g := cfg.Bongs["g"]; !slices.Equal(g.Keywords, []string{"구글"}) || g.MainURL != "https://www.google.co.kr" {
		t.Errorf("g = %+v, want overlay keywords [구글] and main_url https://www.google.co.kr", g)
	}
	if got := cfg.Bongs["d"].Aliases; !slices.Equal(got, []string{"dd"}) {
		t.Errorf("d.Aliases = %v, want [dd] from nested include", got)
	}
	if got := cfg.Bongs["nw"].SearchURL; got != "https://custom.namu.wiki/Go?q=%s" {
		t.Errorf("nw.SearchURL = %q, want parent override", got)
	}
	if got := cfg.Bongs["pp"].PostParams; len(got) != 1 || got["query"] != "%s" {
		t.Errorf("pp.PostParams = %v, want the root post_key alone", got)
	}
	if np := cfg.Bongs["np"]; np.Post || np.PostKey != "" || len(np.PostParams) != 0 || np.MainURL != "https://b.example" || np.SuggestURL != "" {
		t.Errorf("np = %+v, want GET override of included POST bong", np)
	}

	write("settings.yaml", "prefix: \",\"\nbongs: {}\n")
	withSettings := write("root.yaml", "include: [settings.yaml]\n"+`bongs:
  g: "https://www.google.com/search?q=%s"
`)
	if _, err := loadRawConfigFile(withSettings); err == nil || !strings.Contains(err.Error(), "field prefix not found") {
		t.Errorf("err = %v, want field prefix not found", err)
	}
}

func TestCLIRun(t *testing.T) {
	env := map[string]string{"BONG_PREFIX": ",", "BONG_DEFAULT": "w"}
	getenv := func(k string) string { return env[k] }

	var stdout strings.Builder
	if code := run([]string{"-check", "-p", "!", "config.example.yaml"}, getenv, &stdout, io.Discard); code != 0 {
		t.Fatalf("run -check = %d, want 0", code)
	}
	if !strings.Contains(stdout.String(), `prefix "!", default "w"`) {
		t.Errorf("stdout = %q, want the -p flag over BONG_PREFIX and BONG_DEFAULT over the file", stdout.String())
	}

	var stderr strings.Builder
	if code := run([]string{"-c", "config.example.yaml", "other.yaml"}, getenv, io.Discard, &stderr); code != 2 || !strings.Contains(stderr.String(), "give one config path") {
		t.Errorf("run -c and a positional path = %d, stderr %q, want 2 and a usage error", code, stderr.String())
	}
}
