# bong

Search shortcut redirector.

## Install

```sh
go install github.com/glendalab/bong@latest
```

## Usage

```sh
cp config.example.yaml config.yaml
bong config.yaml
```

Flags:

```
-c string   config file path (env: BONG_CONFIG, default: config.yaml)
-l string   listen address (env: BONG_LISTEN)
-b string   base URL for opensearch.xml (env: BONG_BASE_URL, default: request scheme + Host)
-p string   shortcut prefix (env: BONG_PREFIX, default: ".")
-d string   default shortcut key (env: BONG_DEFAULT)
-check      validate config and exit
-version    print version and exit
```

Flags override env vars, which override the config file.

## Config

```yaml
default: g                                  # bong for queries without a shortcut
suggest_url: "https://suggestqueries.google.com/complete/search?client=firefox&q=%s"
include: [kr.yaml]                          # files with only include and bongs, relative to this one
bongs:
  g: "https://www.google.com/search?q=%s"    # search_url only
  gh:
    title: GitHub                           # default: the key
    aliases: [git]                          # .git works too
    keywords: [code]                        # completion only
    main_url: "https://github.com"          # target of a bare .gh, default: search_url's scheme and host
    search_url: "https://github.com/search?q=%s"
    suggest_url: "https://api.github.com/search/repositories?q=%s"
    suggest_path: "items.full_name"         # dot path into a non-OpenSearch JSON response
```

Placeholders:

- `%s`, `{q}`, `{searchTerms}`: the query, percent-encoded in URLs with space as `%20`, raw in `post_params`
- `{language}`: the browser's preferred language, `ko` for `ko-KR`, `en` if none
- a GET `search_url` and every `suggest_url` need a query placeholder
- none in a URL's host, none at all in `main_url`

Suggestions:

- `.gh foo`: the `suggest_url` of `gh`, else the top-level one
- `foo`: the top-level `suggest_url` only, none without it or without a default bong

POST, sent to `search_url`:

- `post_key: q`: the query as field `q`
- `post_params: {q: "%s", hl: "{language}"}`: several fields
- `post: true`: the query parameters of `search_url`, moved out of the URL

Includes: later includes override earlier ones, and the including file overrides both. Overlay rules are in `config.example.yaml`.

Typing a partial shortcut matches exact keys and aliases first, then key or alias prefixes, words in the title, keywords or host, and substrings, falling back to typos if nothing else matches. Hangul initials and partial syllables also work, and Tab completes the first match.
