package hc

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCacheKeyFuncByContext(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeTestFile(t, tmp, "components/card.html", `<div>{{ .Component }}</div>`)
	pagePath := writeTestFile(t, tmp, "pages/page.gohtml", `<Card />`)

	type localeKey struct{}
	cacheCalls := 0

	engine := NewHC(filepath.Join(tmp, "components"),
		WithCacheKeyFunc(func(ctx context.Context, name string) string {
			cacheCalls++
			if ctx == nil {
				return ""
			}
			locale, _ := ctx.Value(localeKey{}).(string)
			if locale == "" {
				return ""
			}
			return locale + ":" + strings.ToLower(name)
		}),
	)

	render := func(locale string) {
		ctx := context.Background()
		if locale != "" {
			ctx = context.WithValue(ctx, localeKey{}, locale)
		}
		var buf bytes.Buffer
		if err := engine.ParseFileContext(ctx, &buf, pagePath, nil); err != nil {
			t.Fatalf("ParseFileContext(%s): %v", locale, err)
		}
	}

	render("en")
	render("fr")
	render("en")
	render("")

	engine.cache.mu.RLock()
	entryCount := len(engine.cache.entries)
	sourceCount := len(engine.cache.sources)
	engine.cache.mu.RUnlock()

	if entryCount != 3 {
		t.Fatalf("expected 3 cache entries (en, fr, default); got %d", entryCount)
	}

	if sourceCount != 1 {
		t.Fatalf("expected 1 cached component source; got %d", sourceCount)
	}

	if cacheCalls != 4 {
		t.Fatalf("CacheKeyFunc invoked %d times, want 4", cacheCalls)
	}
}
