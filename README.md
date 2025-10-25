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
- `ParseFile(writer io.Writer, filename string, data any) error` loads the top-level template, resolves every component in up to 16 passes, and writes the final markup to `writer`. Pass `nil` as the writer if you only need to check for errors (no buffer will be returned).
- `ParseFileContext(ctx context.Context, writer io.Writer, filename string, data any) error` behaves like `ParseFile` but lets you pass the active request context. The renderer forwards this context to helpers created by `WithFuncMapProvider`.

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

When the root `data` is a `map[string]…`, the renderer clones it and injects a `Ctx` entry pointing at the supplied context so attribute expressions can call `{{ .Ctx }}` without additional wiring.

Attribute values run through Go's `text/template` with the same func map, so props can reference fields from `data` or call helper functions. Inside component templates you have access to:

- `.Props` and `.Attrs` for resolved attributes (`.Attrs` keeps original casing so `forwardAttrs` can re-emit them).
- `.Children` for rendered nested markup (empty for self-closing components).
- `.Ctx` for the `context.Context` supplied to `ParseFileContext` (`context.Background()` when using `ParseFile`).
- `.Data` (alias `.Root`) for the root data object passed to `ParseFile`.

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
