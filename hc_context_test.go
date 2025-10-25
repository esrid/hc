package hc

import (
	"bytes"
	"context"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	return full
}

func TestParseFileContext_WithFuncMapProviderMergesHelpers(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/hello.html", `<div>{{ upper .Props.greet }} {{ ctxName .Ctx }} {{ .Data.Message }}-{{ .Root.Message }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Hello greet="{{ ctxValue }}" />`)

	ctxKey := struct{}{}

	providerCalls := 0
	engine := NewHC(filepath.Join(tmp, "components"),
		WithFuncMap(template.FuncMap{
			"upper": strings.ToUpper,
			"ctxName": func(ctx context.Context) string {
				if ctx == nil {
					return ""
				}
				if v, ok := ctx.Value(ctxKey).(string); ok {
					return v
				}
				return ""
			},
		}),
		WithFuncMapProvider(func(ctx context.Context) template.FuncMap {
			providerCalls++
			return template.FuncMap{
				"ctxValue": func() string {
					if ctx == nil {
						return ""
					}
					if v, ok := ctx.Value(ctxKey).(string); ok {
						return v
					}
					return ""
				},
			}
		}),
	)

	rootData := map[string]any{"Message": "message"}
	ctx := context.WithValue(context.Background(), ctxKey, "dynamic")

	var buf bytes.Buffer
	if err := engine.ParseFileContext(ctx, &buf, pagePath, rootData); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "<div>DYNAMIC dynamic message-message</div>"
	if got != want {
		t.Fatalf("rendered output mismatch\nwant: %q\ngot:  %q", want, got)
	}

	if providerCalls != 1 {
		t.Fatalf("provider called %d times, want 1", providerCalls)
	}

	if _, ok := rootData["Ctx"]; ok {
		t.Fatalf("root data mutated; found unexpected Ctx key")
	}
}

func TestParseFileContext_DynamicFuncPerContext(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/greet.html", `<div>{{ .Props.greet }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Greet greet="{{ ctxValue }}" />`)

	ctxKey := struct{}{}
	providerCalls := 0
	engine := NewHC(filepath.Join(tmp, "components"),
		WithFuncMapProvider(func(ctx context.Context) template.FuncMap {
			providerCalls++
			return template.FuncMap{
				"ctxValue": func() string {
					if ctx == nil {
						return ""
					}
					if v, ok := ctx.Value(ctxKey).(string); ok {
						return v
					}
					return ""
				},
			}
		}),
	)

	render := func(value string) string {
		ctx := context.WithValue(context.Background(), ctxKey, value)
		var buf bytes.Buffer
		if err := engine.ParseFileContext(ctx, &buf, pagePath, nil); err != nil {
			t.Fatalf("ParseFileContext: %v", err)
		}
		return strings.TrimSpace(buf.String())
	}

	first := render("alpha")
	second := render("bravo")

	if first != "<div>alpha</div>" {
		t.Fatalf("first render mismatch: %q", first)
	}
	if second != "<div>bravo</div>" {
		t.Fatalf("second render mismatch: %q", second)
	}

	if providerCalls != 2 {
		t.Fatalf("provider called %d times, want 2", providerCalls)
	}
}

func TestParseFileContext_WithDataAugmenter(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/layout.html", `<div>{{ .Data.Default }} {{ .Data.Message }} {{ if .Ctx }}ctx{{ end }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Layout />`)

	augmentCalls := 0
	engine := NewHC(filepath.Join(tmp, "components"),
		WithDataAugmenter(func(ctx context.Context, data any) any {
			augmentCalls++
			base := map[string]any{
				"Default": "augmented",
			}
			if data == nil {
				return base
			}
			if existing, ok := data.(map[string]any); ok {
				copy := make(map[string]any, len(existing)+1)
				for k, v := range existing {
					copy[k] = v
				}
				for k, v := range base {
					copy[k] = v
				}
				return copy
			}
			return data
		}),
	)

	root := map[string]any{"Message": "original"}
	ctx := context.Background()

	var buf bytes.Buffer
	if err := engine.ParseFileContext(ctx, &buf, pagePath, root); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "<div>augmented original ctx</div>"
	if got != want {
		t.Fatalf("rendered output mismatch\nwant: %q\ngot:  %q", want, got)
	}

	if augmentCalls != 1 {
		t.Fatalf("augmenter called %d times, want 1", augmentCalls)
	}

	if _, ok := root["Default"]; ok {
		t.Fatalf("root data mutated; found unexpected Default key")
	}
}
