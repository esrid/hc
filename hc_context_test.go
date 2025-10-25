package hc

import (
	"bytes"
	"context"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recordingWriter struct {
	writes int
	buf    bytes.Buffer
}

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.writes++
	return w.buf.Write(p)
}

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

func TestParseFileContext_FinalTemplatePass(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<div>{{ shout .Greeting }} {{ if .Ctx }}ctx{{ end }}</div>`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithFinalTemplatePass(),
		WithFuncMap(template.FuncMap{
			"shout": strings.ToUpper,
		}),
	)

	data := map[string]any{"Greeting": "hello"}

	var buf bytes.Buffer
	if err := engine.ParseFileContext(context.Background(), &buf, pagePath, data); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "<div>HELLO ctx</div>"
	if got != want {
		t.Fatalf("rendered output mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestParseFileContext_PostProcessorReceivesContextAndFuncs(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<p>{{ greet .Greeting }} {{ ctxValue }}</p>`)

	ctxKey := struct{}{}
	expectedCtx := context.WithValue(context.Background(), ctxKey, "dynamic")

	post := func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
		if ctx != expectedCtx {
			t.Fatalf("post-processor received unexpected context")
		}

		payload, ok := data.(map[string]any)
		if !ok {
			t.Fatalf("post-processor received data of type %T, want map[string]any", data)
		}
		if payload["Ctx"] != expectedCtx {
			t.Fatalf("post-processor data missing expected context")
		}

		if _, ok := funcs["ctxValue"]; !ok {
			t.Fatalf("post-processor funcs missing expected ctxValue helper")
		}

		tpl, err := template.New("post").Funcs(funcs).Option("missingkey=zero").Parse(string(raw))
		if err != nil {
			return nil, err
		}

		var buf bytes.Buffer
		if err := tpl.Execute(&buf, data); err != nil {
			return nil, err
		}
		return buf.Bytes(), nil
	}

	engine := NewHC(filepath.Join(tmp, "components"),
		WithFuncMap(template.FuncMap{
			"greet": strings.ToUpper,
		}),
		WithFuncMapProvider(func(ctx context.Context) template.FuncMap {
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
		WithPostProcessor(post),
	)

	data := map[string]any{"Greeting": "hello"}

	var buf bytes.Buffer
	if err := engine.ParseFileContext(expectedCtx, &buf, pagePath, data); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "<p>HELLO dynamic</p>"
	if got != want {
		t.Fatalf("rendered output mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestParseFileTemplate_ExecutesFinalTemplate(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<span>{{ .Greeting }}</span>`)

	engine := NewHC(filepath.Join(tmp, "components"))

	data := map[string]any{"Greeting": "hi"}

	var buf bytes.Buffer
	if err := engine.ParseFileTemplate(context.Background(), &buf, pagePath, data); err != nil {
		t.Fatalf("ParseFileTemplate: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := "<span>hi</span>"
	if got != want {
		t.Fatalf("rendered output mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestLocaleCacheKeys_SeparatesComponentEntries(t *testing.T) {
	t.Parallel()

	type localeKey struct{}

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/card.html", `<div>{{ .Props.message }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Card message="{{ .Greeting }}" />`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithLocaleCacheKeys("en", func(ctx context.Context) string {
			if ctx == nil {
				return ""
			}
			if v, ok := ctx.Value(localeKey{}).(string); ok {
				return v
			}
			return ""
		}),
	)

	render := func(locale string) {
		ctx := context.WithValue(context.Background(), localeKey{}, locale)
		if err := engine.ParseFileContext(ctx, nil, pagePath, map[string]any{"Greeting": locale}); err != nil {
			t.Fatalf("ParseFileContext: %v", err)
		}
	}

	render("en")
	render("fr")

	engine.cache.mu.RLock()
	defer engine.cache.mu.RUnlock()
	if got := len(engine.cache.entries); got != 2 {
		t.Fatalf("expected 2 cached templates, got %d", got)
	}
}

func TestComponentInstrumentationHooksReceiveEvents(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/alert.html", `<div class="alert">{{ .Props.message }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Alert message="{{ .Greeting }}" />`)

	var (
		mu      sync.Mutex
		events  []ComponentInstrumentationEvent
		ctxUsed []context.Context
	)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithComponentInstrumentation(func(ctx context.Context, evt ComponentInstrumentationEvent) {
			mu.Lock()
			defer mu.Unlock()
			events = append(events, evt)
			ctxUsed = append(ctxUsed, ctx)
		}),
	)

	ctx := context.WithValue(context.Background(), struct{}{}, "value")
	if err := engine.ParseFileContext(ctx, nil, pagePath, map[string]any{"Greeting": "hi"}); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(events) != 2 {
		t.Fatalf("expected 2 instrumentation events, got %d", len(events))
	}
	if events[0].Stage != ComponentStageBegin || events[0].Component != "Alert" {
		t.Fatalf("unexpected begin event: %+v", events[0])
	}
	if events[1].Stage != ComponentStageEnd || events[1].Component != "Alert" {
		t.Fatalf("unexpected end event: %+v", events[1])
	}
	if events[1].Err != nil {
		t.Fatalf("expected nil error on success, got %v", events[1].Err)
	}
	if events[1].Duration < 0 {
		t.Fatalf("expected non-negative duration, got %v", events[1].Duration)
	}
	if ctxUsed[0] != ctx || ctxUsed[1] != ctx {
		t.Fatalf("instrumentation received unexpected context")
	}
}

func TestComponentInstrumentationCapturesErrors(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/broken.html", `{{ if }}`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Broken />`)

	var captured ComponentInstrumentationEvent

	engine := NewHC(filepath.Join(tmp, "components"),
		WithComponentInstrumentation(func(ctx context.Context, evt ComponentInstrumentationEvent) {
			if evt.Stage == ComponentStageEnd {
				captured = evt
			}
		}),
	)

	err := engine.ParseFileContext(context.Background(), nil, pagePath, nil)
	if err == nil {
		t.Fatalf("expected error")
	}

	if captured.Err == nil {
		t.Fatalf("expected instrumentation error to be set")
	}
	if captured.Component != "Broken" {
		t.Fatalf("expected component name to be Broken, got %q", captured.Component)
	}
}

func TestComponentAugmenterInjectsDefaults(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/form.html", `<form><input type="hidden" name="csrf" value="{{ .Props.csrf }}" /></form>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Form></Form>`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithComponentAugmenter("Form", func(ctx context.Context, component string, payload map[string]any) error {
			props, _ := payload["Props"].(map[string]any)
			if props == nil {
				props = map[string]any{}
				payload["Props"] = props
			}
			if _, ok := props["csrf"]; !ok {
				props["csrf"] = "token"
			}
			return nil
		}),
	)

	var buf bytes.Buffer
	if err := engine.ParseFileContext(context.Background(), &buf, pagePath, nil); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := strings.TrimSpace(buf.String())
	want := `<form><input type="hidden" name="csrf" value="token" /></form>`
	if got != want {
		t.Fatalf("augmenter output mismatch\nwant: %q\ngot:  %q", want, got)
	}
}

func TestAttrValidationMissingRequired(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/alert.html", `<div>{{ .Props.message }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Alert></Alert>`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithAttrRules("Alert", RequireAttrs("message")),
	)

	err := engine.ParseFileContext(context.Background(), nil, pagePath, nil)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	if !strings.Contains(err.Error(), "missing required attr \"message\"") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestAttrValidationUnsupportedAttr(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/button.html", `<button>{{ .Props.label }}</button>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Button label="Save" data-id="123"></Button>`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithAttrRules("Button", RequireAttrs("label")),
	)

	err := engine.ParseFileContext(context.Background(), nil, pagePath, nil)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	if !strings.Contains(err.Error(), "unsupported attr \"data-id\"") {
		t.Fatalf("unexpected error: %v", err)
	}

	// Allow other attrs and ensure success.
	engine = NewHC(filepath.Join(tmp, "components"),
		WithAttrRules("Button", RequireAttrs("label"), AllowOtherAttrs()),
	)
	if err := engine.ParseFileContext(context.Background(), nil, pagePath, nil); err != nil {
		t.Fatalf("ParseFileContext with AllowOtherAttrs: %v", err)
	}
}

func TestPagePipelineRunsInOrder(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `base`)

	var order []string

	engine := NewHC(filepath.Join(tmp, "components"),
		WithPagePipeline(
			func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
				order = append(order, "pipeline-1")
				return []byte(string(raw) + "-p1"), nil
			},
			func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
				order = append(order, "pipeline-2")
				return []byte(string(raw) + "-p2"), nil
			},
		),
		WithPostProcessor(func(ctx context.Context, raw []byte, data any, funcs template.FuncMap) ([]byte, error) {
			order = append(order, "post")
			return []byte(string(raw) + "-post"), nil
		}),
	)

	var buf bytes.Buffer
	if err := engine.ParseFileContext(context.Background(), &buf, pagePath, nil); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := buf.String()
	want := "base-p1-p2-post"
	if got != want {
		t.Fatalf("pipeline output mismatch\nwant: %q\ngot:  %q", want, got)
	}

	if len(order) != 3 || order[0] != "pipeline-1" || order[1] != "pipeline-2" || order[2] != "post" {
		t.Fatalf("unexpected execution order: %v", order)
	}
}
func TestParseFileContext_StreamingWrites(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/banner.html", `<div class="banner">{{ .Props.message }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", "Start\n<Banner message=\"{{ .Greeting }}\" />\nEnd\n")

	engine := NewHC(filepath.Join(tmp, "components"),
		WithStreamingWrites(),
	)

	writer := &recordingWriter{}
	data := map[string]any{"Greeting": "hello"}

	if err := engine.ParseFileContext(context.Background(), writer, pagePath, data); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := writer.buf.String()
	want := "Start\n<div class=\"banner\">hello</div>\nEnd\n"
	if got != want {
		t.Fatalf("streamed output mismatch\nwant: %q\ngot:  %q", want, got)
	}

	if writer.writes <= 1 {
		t.Fatalf("expected multiple streaming writes, got %d", writer.writes)
	}
}

func TestParseFileContext_StreamingDisabledWithFinalTemplatePass(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/item.html", `<li>{{ .Props.text }}</li>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<ul><Item text="{{ .Value }}" /></ul>`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithStreamingWrites(),
		WithFinalTemplatePass(),
	)

	writer := &recordingWriter{}
	data := map[string]any{"Value": "entry"}

	if err := engine.ParseFileContext(context.Background(), writer, pagePath, data); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := strings.TrimSpace(writer.buf.String())
	want := "<ul><li>entry</li></ul>"
	if got != want {
		t.Fatalf("render mismatch\nwant: %q\ngot:  %q", want, got)
	}

	if writer.writes != 1 {
		t.Fatalf("expected buffered write when final template pass enabled; got %d writes", writer.writes)
	}
}

func TestParseFileContext_ComponentParseErrorIncludesSource(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/broken.html", `{{ if }}`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Broken />`)

	engine := NewHC(filepath.Join(tmp, "components"),
		WithStreamingWrites(),
	)

	err := engine.ParseFileContext(context.Background(), nil, pagePath, nil)
	if err == nil {
		t.Fatalf("expected parse error")
	}

	msg := err.Error()
	if !strings.Contains(msg, filepath.ToSlash("components/Broken.html")) {
		t.Fatalf("error missing component path; err=%v", err)
	}
	if !strings.Contains(msg, ":1") {
		t.Fatalf("error missing line hint; err=%v", err)
	}
}
