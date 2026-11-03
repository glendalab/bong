package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	htmlt "html/template"
	"log/slog"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	textt "text/template"
)

//go:embed templates/*.html templates/opensearch.xml
var templateFS embed.FS

//go:embed templates/icon.svg
var icon []byte

var (
	indexTmpl      = htmlt.Must(htmlt.ParseFS(templateFS, "templates/index.html"))
	postTmpl       = htmlt.Must(htmlt.ParseFS(templateFS, "templates/post.html"))
	openSearchTmpl = textt.Must(textt.ParseFS(templateFS, "templates/opensearch.xml"))
)

type Server struct {
	Config    *Config
	Suggestor *Suggestor
	Logger    *slog.Logger

	items []bongItem
	mux   *http.ServeMux
}

type bongItem struct {
	Key     string
	Aliases []string
	Title   string
	MainURL string
	Host    string
}

func NewServer(cfg *Config, logger *slog.Logger) *Server {
	s := &Server{
		Config:    cfg,
		Suggestor: NewSuggestor(cfg),
		Logger:    logger,
		items:     buildBongItems(cfg),
		mux:       http.NewServeMux(),
	}

	s.mux.HandleFunc("GET /{$}", s.handleIndex)
	for _, path := range []string{"/s", "/search"} {
		s.mux.HandleFunc("GET "+path, s.handleSearch)
		s.mux.HandleFunc("POST "+path, s.handleSearch)
	}
	s.mux.HandleFunc("GET /suggest", s.handleSuggest)
	s.mux.HandleFunc("GET /opensearch.xml", s.handleOpenSearch)
	s.mux.HandleFunc("GET /icon.svg", s.handleIcon)

	return s
}

func buildBongItems(cfg *Config) []bongItem {
	keys := slices.Sorted(maps.Keys(cfg.Bongs))
	if i := slices.Index(keys, cfg.Default); i > 0 {
		keys = slices.Insert(slices.Delete(keys, i, i+1), 0, cfg.Default)
	}
	items := make([]bongItem, 0, len(keys))
	for _, k := range keys {
		b := cfg.Bongs[k]
		items = append(items, bongItem{
			Key:     k,
			Aliases: b.Aliases,
			Title:   b.Title,
			MainURL: b.MainURL,
			Host:    strings.TrimPrefix(b.host, "www."),
		})
	}
	return items
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	s.mux.ServeHTTP(w, r)
}

type indexData struct {
	Title        string
	Prefix       string
	PlainSuggest bool
	Query        string
	Message      string
	Items        []bongItem
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	s.renderIndex(w, http.StatusOK, "", "")
}

func (s *Server) renderIndex(w http.ResponseWriter, status int, query, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	err := indexTmpl.Execute(w, indexData{
		Title:        s.Config.Title,
		Prefix:       s.Config.Prefix,
		PlainSuggest: s.Config.SuggestURL != "" && s.Config.Default != "",
		Query:        query,
		Message:      message,
		Items:        s.items,
	})
	if err != nil {
		s.Logger.Error("render index failed", "err", err)
	}
}

type postData struct {
	Action string
	Params []FormParam
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	q := r.FormValue("q")
	trimmed := strings.TrimSpace(q)
	if trimmed == "" || strings.EqualFold(trimmed, s.Config.Prefix) {
		s.renderIndex(w, http.StatusOK, "", "")
		return
	}

	res := s.Config.Resolve(q)
	if !res.Matched {
		s.renderIndex(w, http.StatusNotFound, q, "No matching shortcut and no default bong set.")
		return
	}

	lang := acceptLanguage(r.Header.Values("Accept-Language"))
	if res.UsePost() {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		err := postTmpl.Execute(w, postData{
			Action: strings.ReplaceAll(res.Bong.SearchURL, "{language}", lang),
			Params: res.PostFormParams(lang),
		})
		if err != nil {
			s.Logger.Error("render post failed", "err", err)
		}
		return
	}

	http.Redirect(w, r, res.TargetURL(lang), http.StatusFound)
}

var (
	languageSubtag = regexp.MustCompile(`^[A-Za-z]{2,8}$`)
	qWeight        = regexp.MustCompile(`(?i)^q=(0(?:\.[0-9]{0,3})?|1(?:\.0{0,3})?)$`)
)

func acceptLanguage(values []string) string {
	best, bestQ := "en", 0.0
	for _, header := range values {
		for _, entry := range strings.Split(header, ",") {
			tag, params, _ := strings.Cut(entry, ";")
			primary, _, _ := strings.Cut(strings.TrimSpace(tag), "-")
			if !languageSubtag.MatchString(primary) {
				continue
			}
			q := 1.0
			if params = strings.TrimSpace(params); params != "" {
				m := qWeight.FindStringSubmatch(params)
				if m == nil {
					continue
				}
				q, _ = strconv.ParseFloat(m[1], 64)
			}
			if q > bestQ {
				best, bestQ = strings.ToLower(primary), q
			}
		}
	}
	return best
}

func (s *Server) handleSuggest(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	suggestions, descriptions, err := s.Suggestor.Suggest(r.Context(), q, acceptLanguage(r.Header.Values("Accept-Language")))
	if err != nil {
		if r.Context().Err() == nil {
			s.Logger.Error("upstream suggest failed", "err", err)
		}
		http.Error(w, "upstream suggest failed", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/x-suggestions+json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	// OpenSearch clients reject null for the suggestion array
	if suggestions == nil {
		suggestions = []string{}
	}
	payload := []any{q, suggestions}
	if len(descriptions) > 0 {
		types := make([]string, len(descriptions))
		details := make([]map[string]string, len(descriptions))
		for i, desc := range descriptions {
			if desc != "" {
				types[i] = "ENTITY"
				details[i] = map[string]string{
					"google:entityinfo": encodeEntityAnnotation(desc),
				}
			} else {
				types[i] = "QUERY"
				details[i] = map[string]string{}
			}
		}
		payload = []any{
			q,
			suggestions,
			descriptions,
			[]string{},
			map[string]any{
				"google:suggesttype":   types,
				"google:suggestdetail": details,
			},
		}
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		s.Logger.Error("encode suggestions failed", "err", err)
	}
}

// Chromium decodes google:entityinfo as a base64 proto, tag 0x12 = field 2, length-delimited
func encodeEntityAnnotation(annotation string) string {
	buf := []byte{0x12}
	buf = binary.AppendUvarint(buf, uint64(len(annotation)))
	buf = append(buf, annotation...)
	return base64.StdEncoding.EncodeToString(buf)
}

type openSearchData struct {
	Title   string
	BaseURL string
}

func (s *Server) handleOpenSearch(w http.ResponseWriter, r *http.Request) {
	baseURL := s.Config.BaseURL
	if baseURL == "" {
		if r.Host == "" {
			http.Error(w, "missing Host header and no base_url configured", http.StatusBadRequest)
			return
		}
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		baseURL = scheme + "://" + r.Host
	}
	w.Header().Set("Content-Type", "application/opensearchdescription+xml; charset=utf-8")
	err := openSearchTmpl.Execute(w, openSearchData{
		Title:   xmlEscapeString(s.Config.Title),
		BaseURL: xmlEscapeString(baseURL),
	})
	if err != nil {
		s.Logger.Error("render opensearch failed", "err", err)
	}
}

func (s *Server) handleIcon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(icon)
}

func xmlEscapeString(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}
