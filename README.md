# HC Component Templates

This repository shows how to build reusable HTML components on top of Go's `html/template`.  
`main.go` is the minimal, beginner-friendly entry point—it wires everything together and renders `web/pages/page.gohtml`.

## Quick Start

```bash
go run main.go
```

The command renders the sample page to standard output. Redirect it to a file if you want to open it in a browser:

```bash
go run main.go > page.html
open page.html
```

## How It Works

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
hc := NewHC("web/components",
  WithFS(content),
  WithFuncMap(template.FuncMap{
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
