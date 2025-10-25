# HC Component Templates

This module lets you build composable HTML components on top of Go's `html/template`.  
Install it in your own project, point it at a directory of component templates, and call `ParseFile` to render pages.

## Installation

```bash
go get github.com/esrid/hc
```

> The repository also ships a tiny demo under `main.go`, but you never need to run it in your project.

## Set Up a Renderer

Create a shared renderer as part of your application. This example embeds templates and exposes an HTTP handler, but the same renderer can be reused in CLI tools or background jobs:

```go
package renderer

import (
  "embed"
  "html/template"
  "net/http"
  "strings"

  "github.com/esrid/hc"
)

//go:embed web/**
var templateFS embed.FS

var components = hc.NewHC("web/components",
  hc.WithFS(templateFS), // load from embed.FS
  hc.WithFuncMap(template.FuncMap{
    "upper": strings.ToUpper,
  }),
)

func PageHandler(w http.ResponseWriter, r *http.Request) {
  data := map[string]any{
    "Primary": "save changes",
    "Message": "Welcome back",
  }
  if err := components.ParseFile(w, "web/pages/page.gohtml", data); err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
  }
}
```

- `NewHC(folder string, opts ...Option)` initialises the engine and memoizes compiled component templates keyed by lowercase component names. Reuse the same instance across requests; the cache is concurrency-safe.
- `WithFS(embed.FS)` lets you serve templates out of `//go:embed` bundles. Without it, files are read from disk relative to `folder`.
- `WithFuncMap(template.FuncMap)` merges additional helpers into both the component templates and attribute evaluator. Helpers can be consumed inside component files (`{{ upper .Props.text }}`) or attribute expressions (`text="{{ upper .Primary }}"`).
- `WithFuncMapProvider(func(context.Context) template.FuncMap)` supplies request-scoped helpers (translations, authorization checks, etc.). The provider is invoked once per render and merged with the static func map.
- `WithDataAugmenter(func(context.Context, any) any)` lets you layer default fields onto the data model once per render (for example, injecting `.User` based on the request context).
- `WithCacheKeyFunc(func(context.Context, string) string)` customises the component cache key so you can reuse compiled templates per locale or feature flag while keeping the shared renderer.
- `WithFinalTemplatePass()` runs the fully expanded markup back through Go's `html/template` using the merged func map, so final translations or loops can run outside component files.
- `WithPostProcessor(func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error))` installs callbacks that can mutate or replace the rendered HTML after component expansion (minifiers, extra templating, audit hooks, etc.). Post-processors run after the optional final template pass and receive the merged func map for convenience.
- `WithStreamingWrites()` tells HC to stream directly into the provided `io.Writer` as components resolve, avoiding a full in-memory buffer when no final template pass or post-processors are configured.
- `WithLocaleCacheKeys(defaultLocale string, extractor hc.LocaleExtractor)` prefixes component cache keys with the caller's locale so you can safely reuse a shared renderer across multiple languages.
- `WithComponentInstrumentation(func(context.Context, hc.ComponentInstrumentationEvent))` wraps each component render with begin/end callbacks for logging, metrics, or tracing.
- `WithComponentAugmenter(component string, func(context.Context, string, map[string]any) error)` lets you inject default props or mutate payloads before the component template executes.
- `WithAttrRules(component string, opts ...hc.AttrRuleOption)` enforces required and allowed attributes using helpers like `hc.RequireAttrs`, `hc.AllowAttrs`, and `hc.AllowOtherAttrs`.
- `WithPagePipeline(steps ...hc.PostProcessor)` chains multiple post-processing stages (markdown, sanitizers, localization) without leaving HC; pipelines run before any individual post-processors.
- `ParseFile(writer io.Writer, filename string, data any) error` loads the top-level template, resolves every component in up to 16 passes, and writes the final markup to `writer`. Pass `nil` as the writer if you only need to check for errors (no buffer will be returned).
- `ParseFileContext(ctx context.Context, writer io.Writer, filename string, data any) error` behaves like `ParseFile` but lets you pass the active request context. The renderer forwards this context to helpers created by `WithFuncMapProvider`.
- `ParseFileTemplate(ctx context.Context, writer io.Writer, filename string, data any) error` is a convenience wrapper that always performs the final `html/template` pass before writing.

```go
components := hc.NewHC("web/components",
  hc.WithFuncMapProvider(func(ctx context.Context) template.FuncMap {
    user := ctx.Value(userKey{}) // grab user off the request context
    return template.FuncMap{
      "T": func(key string) string {
        return translations.Lookup(ctx, key)
      },
      "User": func() any { return user },
    }
  }),
  hc.WithDataAugmenter(func(ctx context.Context, data any) any {
    base := map[string]any{"User": ctx.Value(userKey{})}
    if src, ok := data.(map[string]any); ok {
      merged := make(map[string]any, len(src)+len(base))
      for k, v := range src {
        merged[k] = v
      }
      for k, v := range base {
        merged[k] = v
      }
      return merged
    }
    return base
  }),
)
```

For localisation-friendly helpers, reach for the optional add-on under `hcx/i18n`. It inspects `Accept-Language`, loads locale bundles, and installs a ready-made `T` helper:

```go
engine := hc.NewHC("web/components",
  i18n.Provider(i18n.Options{
    Loader: func(ctx context.Context, locale string) (i18n.Translator, error) {
      return bundles.Load(locale), nil
    },
    DefaultLocale:    "en",
    SupportedLocales: []string{"en", "fr"},
  }),
  hc.WithCacheKeyFunc(func(ctx context.Context, name string) string {
    header := i18n.AcceptLanguageFromContext(ctx)
    if header == "" {
      return strings.ToLower(name)
    }
    return header + ":" + strings.ToLower(name)
  }),
)
```

Use `i18n.WithAcceptLanguage(ctx, header)` in your HTTP handlers to pass the request header into the renderer.

## Locale Cache Keys

HC ships a helper so your component cache can follow the active locale automatically.

**Example 1: Prefix cache keys with `Accept-Language`**

```go
engine := hc.NewHC("web/components",
  i18n.Provider( /* ... */ ),
  hc.WithLocaleCacheKeys("en", i18n.AcceptLanguageFromContext),
)

func handler(w http.ResponseWriter, r *http.Request) {
  ctx := i18n.WithAcceptLanguage(r.Context(), r.Header.Get("Accept-Language"))
  if err := engine.ParseFileContext(ctx, w, "web/pages/home.gohtml", nil); err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
  }
}
```

**Example 2: Cache per user preference stored on the context**

```go
type localeKey struct{}

engine := hc.NewHC("web/components",
  hc.WithLocaleCacheKeysFromValue(localeKey{}, "en"),
)

func middleware(next http.Handler) http.Handler {
  return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
    ctx := context.WithValue(r.Context(), localeKey{}, readUserLocale(r))
    next.ServeHTTP(w, r.WithContext(ctx))
  })
}
```

When the root `data` is a `map[string]…`, the renderer clones it and injects a `Ctx` entry pointing at the supplied context so attribute expressions can call `{{ .Ctx }}` without additional wiring.

Attribute values run through Go's `text/template` with the same func map, so props can reference fields from `data` or call helper functions. Inside component templates you have access to:

- `.Props` and `.Attrs` for resolved attributes (`.Attrs` keeps original casing so `forwardAttrs` can re-emit them).
- `.Children` for rendered nested markup (empty for self-closing components).
- `.Ctx` for the `context.Context` supplied to `ParseFileContext` (`context.Background()` when using `ParseFile`).
- `.Data` (alias `.Root`) for the root data object passed to `ParseFile`.

## Final Template Pass

Enabling the final template pass feeds the rendered HTML back through Go's `html/template` with the same func map the components used. This is handy for localisation helpers, conditional wrappers, or iterative logic that is easier to express outside component files.

**Example 1: Translate plain strings after component expansion**

```go
engine := hc.NewHC("web/components",
  hc.WithFinalTemplatePass(),
  hc.WithFuncMap(template.FuncMap{
    "T": func(key string) template.HTML {
      translations := map[string]string{"hello": "Bonjour"}
      return template.HTML(html.EscapeString(translations[key]))
    },
  }),
)

// web/pages/home.gohtml
// <GreetingCard>{{ T .GreetingKey }}</GreetingCard>

var buf bytes.Buffer
engine.ParseFileContext(context.Background(), &buf, "web/pages/home.gohtml", map[string]any{
  "GreetingKey": "hello",
})
// buf.String() == "<section class=\"card\">Bonjour</section>"
```

**Example 2: Run layout control structures after components render**

```go
engine := hc.NewHC("web/components",
  hc.WithFinalTemplatePass(),
)

// web/pages/dashboard.gohtml
// <DashboardShell>
//   {{ range .Announcements }}
//     <Alert>{{ . }}</Alert>
//   {{ else }}
//     <EmptyState>No news.</EmptyState>
//   {{ end }}
// </DashboardShell>

data := map[string]any{"Announcements": []string{"System upgrade tonight"}}
var buf bytes.Buffer
engine.ParseFileTemplate(context.Background(), &buf, "web/pages/dashboard.gohtml", data)
// Final HTML now contains a rendered <Alert> with the announcement.
```

## Post-Processing Hooks

Post-processors let you reshape or validate output after the core renderer finishes. Each hook receives the request context, raw bytes, the data payload (with `.Ctx` already injected when applicable), and the merged func map, then returns the bytes to use for the next hook.

**Example 1: Run the output through `html/template` with extra helpers**

```go
engine := hc.NewHC("web/components",
  hc.WithPostProcessor(func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
    enhanced := template.Must(template.New("final").
      Funcs(funcs).
      Funcs(template.FuncMap{"NowYear": func() int { return time.Now().Year() }}).
      Parse(string(raw)))

    var buf bytes.Buffer
    if err := enhanced.Execute(&buf, data); err != nil {
      return nil, err
    }
    return buf.Bytes(), nil
  }),
)
```

**Example 2: Add an auditing wrapper that records render time and injects a banner**

```go
engine := hc.NewHC("web/components",
  hc.WithPostProcessor(func(ctx context.Context, raw []byte, data any, _ template.FuncMap) ([]byte, error) {
    start := time.Now()
    defer metrics.RecordRenderDuration(time.Since(start))

    return []byte(`<div class="notice">Preview mode</div>` + string(raw)), nil
  }),
  hc.WithPostProcessor(func(ctx context.Context, raw []byte, data any, _ template.FuncMap) ([]byte, error) {
    return bytes.ReplaceAll(raw, []byte("\n"), []byte{}), nil // very naive minifier example
  }),
)
```

## Page Pipelines

Pipelines let you register ordered sequences of post-processors without wiring them up manually in calling code. Each pipeline step receives the output from the previous step and can return new bytes for the next.

**Example 1: Markdown → sanitizer pipeline**

```go
engine := hc.NewHC("web/components",
  hc.WithPagePipeline(
    func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
      return markdown.ToHTML(raw, nil, nil), nil
    },
    func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
      return sanitizer.StripUnsafe(raw), nil
    },
  ),
)
```

**Example 2: Localize, then minify, then emit metrics**

```go
engine := hc.NewHC("web/components",
  hc.WithPagePipeline(
    localization.Step(),
    func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
      return minify.HTML(raw), nil
    },
  ),
  hc.WithPostProcessor(func(ctx context.Context, raw []byte, data any, _ template.FuncMap) ([]byte, error) {
    metrics.Count("page.render.bytes", len(raw))
    return raw, nil
  }),
)
```

## Streaming Writes

Streaming writes keep memory usage flat by piping render output directly to the supplied `io.Writer`. This mode is enabled via `WithStreamingWrites()` and is automatically skipped whenever a final template pass or post-processor is configured (those features require buffering).

**Example 1: Stream a large page straight to an HTTP response**

```go
engine := hc.NewHC("web/components",
  hc.WithStreamingWrites(),
)

func articleHandler(w http.ResponseWriter, r *http.Request) {
  payload := map[string]any{
    "Title": r.URL.Query().Get("title"),
    "Body":  loadArticleBody(r.Context()),
  }
  if err := engine.ParseFileContext(r.Context(), w, "web/pages/article.gohtml", payload); err != nil {
    log.Printf("render: %v", err)
    http.Error(w, "render error", http.StatusInternalServerError)
  }
}
```

**Example 2: Chain a `gzip.Writer` to compress large exports without buffering**

```go
func exportHandler(w http.ResponseWriter, r *http.Request) {
  engine := hc.NewHC("web/components",
    hc.WithStreamingWrites(),
  )

  gz := gzip.NewWriter(w)
  defer gz.Close()

  if err := engine.ParseFileContext(r.Context(), gz, "web/pages/export.gohtml", gatherData(r.Context())); err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
    return
  }
}
```

## ParseFileTemplate Convenience

`ParseFileTemplate` is a helper that always runs the final `html/template` execution. Use it in handlers when you want to guarantee localisation or other helpers run even if the engine instance was created without `WithFinalTemplatePass()`.

**Example 1: Always localise in HTTP handlers**

```go
engine := hc.NewHC("web/components",
  i18n.Provider( /* ... */ ),
)

func handler(w http.ResponseWriter, r *http.Request) {
  ctx := i18n.WithAcceptLanguage(r.Context(), r.Header.Get("Accept-Language"))
  data := map[string]any{"GreetingKey": "hello"}
  if err := engine.ParseFileTemplate(ctx, w, "web/pages/home.gohtml", data); err != nil {
    http.Error(w, err.Error(), http.StatusInternalServerError)
  }
}
```

**Example 2: Render to a buffer in tests with layout logic**

```go
func TestHomePage(t *testing.T) {
  engine := hc.NewHC("web/components")

  var buf bytes.Buffer
  if err := engine.ParseFileTemplate(context.Background(), &buf, "web/pages/home.gohtml", map[string]any{
    "Announcements": []string{"New features"},
  }); err != nil {
    t.Fatalf("render: %v", err)
  }

  require.Contains(t, buf.String(), "New features")
}
```

## Template Diagnostics

When a component template fails to parse, HC now surfaces the originating file path and line number alongside the standard Go template message. This mirrors `html/template` diagnostics while grounding the error in your component tree.

**Example 1: Log parse failures with file and line details**

```go
if err := engine.ParseFileContext(ctx, nil, "web/pages/dashboard.gohtml", data); err != nil {
  var tplErr *template.Error
  log.Printf("component render failed: %v", err) // e.g. parse component Card (/app/web/components/card.html:17): unexpected "end"
}
```

**Example 2: Assert diagnostics in tests to prevent regressions**

```go
func TestBrokenComponent(t *testing.T) {
  _, err := newEngine().ParseFileContext(context.Background(), nil, "web/pages/broken.gohtml", nil)
  require.Error(t, err)
  require.Contains(t, err.Error(), "components/card.html:17")
}
```

## Component Instrumentation

Instrumentation hooks fire before and after every component render so you can capture timings, call stacks, or errors.

**Example 1: Log durations**

```go
logger := zap.L()

engine := hc.NewHC("web/components",
  hc.WithComponentInstrumentation(func(ctx context.Context, evt hc.ComponentInstrumentationEvent) {
    if evt.Stage == hc.ComponentStageEnd {
      logger.Info("rendered component",
        zap.String("component", evt.Component),
        zap.Duration("duration", evt.Duration),
        zap.Error(evt.Err),
      )
    }
  }),
)
```

**Example 2: Emit per-component Prometheus metrics**

```go
var renderLatency = prometheus.NewHistogramVec(
  prometheus.HistogramOpts{Name: "hc_component_seconds"},
  []string{"component"},
)

engine := hc.NewHC("web/components",
  hc.WithComponentInstrumentation(func(ctx context.Context, evt hc.ComponentInstrumentationEvent) {
    if evt.Stage == hc.ComponentStageEnd && evt.Duration >= 0 {
      renderLatency.WithLabelValues(evt.Component).Observe(evt.Duration.Seconds())
    }
  }),
)
```

## Component Augmenters

Augmenters receive the payload passed into a component template and can mutate it before execution. Use them to inject defaults (CSRF tokens, analytics IDs) or to enforce shared behaviour across families of components.

**Example 1: Add CSRF tokens to every `<Form>`**

```go
engine := hc.NewHC("web/components",
  hc.WithComponentAugmenter("Form", func(ctx context.Context, name string, payload map[string]any) error {
    props, _ := payload["Props"].(map[string]any)
    if props == nil {
      props = map[string]any{}
      payload["Props"] = props
    }
    props["csrf"] = csrf.FromContext(ctx)
    return nil
  }),
)
```

**Example 2: Stamp the component name onto every payload**

```go
engine := hc.NewHC("web/components",
  hc.WithComponentAugmenter("*", func(ctx context.Context, name string, payload map[string]any) error {
    props, _ := payload["Props"].(map[string]any)
    if props == nil {
      props = map[string]any{}
    }
    props["component"] = name
    payload["Props"] = props
    return nil
  }),
)
```

## Attribute Validation DSL

Declare required and permitted attributes so pages fail fast when they omit or introduce props.

**Example 1: Enforce required props**

```go
engine := hc.NewHC("web/components",
  hc.WithAttrRules("Button", hc.RequireAttrs("label", "href"), hc.AllowAttrs("variant")),
)
```

If a page renders `<Button variant="primary"/>` without `label` or `href`, HC returns `component Button missing required attr "label"`.

**Example 2: Allow arbitrary data attributes while keeping required props**

```go
engine := hc.NewHC("web/components",
  hc.WithAttrRules("Card",
    hc.RequireAttrs("title"),
    hc.AllowAttrs("icon"),
    hc.AllowOtherAttrs(),
  ),
)
```

The card still requires `title`, but arbitrary `data-*` or `aria-*` values can flow through without errors.

## Rendering Outside HTTP

To generate HTML in scripts or tests, point the renderer at an `io.Writer` of your choice:

```go
var buf bytes.Buffer
if err := components.ParseFile(&buf, "web/pages/page.gohtml", map[string]any{"Message": "Hi"}); err != nil {
  t.Fatal(err)
}
got := buf.String()
```

## Template Conventions

- Components live in `web/components/*.html`. The component name must start with an uppercase letter (for example `Button` → `web/components/button.html`).
- Pages and partials can use components by writing a matching HTML-like tag: `<Button text="Save"/>`.
- Attributes become component props. Inside the template they are available via `.Props` (map with lower-cased keys) and `.Attrs` (original attribute casing for forwarding).
- Child markup between the opening and closing tags is rendered recursively and exposed as `.Children`.
- The helper `forwardAttrs` copies arbitrary attributes from usage sites onto the rendered HTML tag, making it easy to support `class`, `id`, ARIA attributes, and boolean flags.
- Custom template helpers can be registered through `WithFuncMap`. In `main.go` a `upper` function is injected so attributes may call `{{ upper .Primary }}`.

## Button Example

**Usage in a page**

```html
<Button text="{{ upper .Primary }}" class="primary" data-role="cta"/>
```

**Component (`web/components/button.html`)**

```html
{{- $label := "" -}}
{{- with .Props.text -}}
  {{- $label = . -}}
{{- end -}}
<button{{ forwardAttrs .Attrs "text" }}>{{ $label }}</button>
```

**Rendered HTML**

```html
<button class="primary" data-role="cta">SAVE CHANGES</button>
```

Because `text` is excluded from `forwardAttrs`, it becomes the button label while the rest of the attributes flow through.

## Card Layout Example

**Usage in a page**

```html
<Card class="card shadow">
  <h2>Account</h2>
  <p>Keep your profile up to date.</p>
  <Button text="Update" class="secondary"/>
</Card>
```

**Component (`web/components/card.html`)**

```html
<div class="card"{{ forwardAttrs .Attrs }}>
  {{ .Children }}
</div>
```

**Rendered HTML**

```html
<div class="card shadow">
  <h2>Account</h2>
  <p>Keep your profile up to date.</p>
  <button class="secondary">Update</button>
</div>
```

The `Card` component uses `.Children` to inject the inner content, letting you stack buttons, headings, and paragraphs without editing the component file.

## Nesting Components

Components can wrap one another. The `WithChildren` example demonstrates how to slot content into a layout region:

**Usage (`web/pages/page.gohtml`)**

```html
<WithChildren>
  <Card>
    <Button text="Nested"/>
  </Card>
  <span>Plain HTML still works.</span>
</WithChildren>
```

**Component (`web/components/withchildren.html`)**

```html
<div class="with-children"{{ forwardAttrs .Attrs }}>
  <strong>Slot:</strong>
  <div class="content">
    {{ .Children }}
  </div>
</div>
```

The renderer runs repeatedly (up to 16 passes) until every custom component is expanded, so you can nest components deeply.

## Creating Your Own Component

1. Add a template file to `web/components`. Name it after the component (`Button` → `button.html`); `.tmpl` and `.gohtml` extensions also work.
2. Reference it from a page using the capitalized tag: `<Alert type="info">Heads up!</Alert>`.
3. Access props inside the template via `.Props["type"]` or `.Props.type`. Use `.Children` to render inner content.
4. Call `forwardAttrs` to copy through arbitrary attributes while excluding any that you handle manually.

If you need extra helpers, extend the func map in `main.go`:

```go
engine := hc.NewHC("web/components",
  hc.WithFS(content),
  hc.WithFuncMap(template.FuncMap{
    "upper": strings.ToUpper,
    "title": cases.Title(language.English).String,
  }),
)
```

Now attributes and templates can call `{{ title .Primary }}` just like any other template function.

## Troubleshooting

- **"component X not found"** – make sure the template file exists under `web/components` with a supported extension (`.html`, `.gohtml`, or `.tmpl`) and the name matches the tag in your page.
- **Empty output** – the renderer refuses to process empty files and returns `ErrEmptyFile`. Check that your component or page template contains markup.
- **Unclosed component tag** – ensure every `<Component>` has a corresponding `</Component>` unless it is self-closing (`<Component />`).

With these patterns you can incrementally grow a library of HTML-building blocks while staying inside familiar Go templates. Explore `main.go` and `web/pages/page.gohtml` to see the complete, beginner-friendly example.

## Optional: Run the Demo

The repository includes a runnable sample you can inspect locally:

```bash
go run main.go > page.html
open page.html
```
