package i18n

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/esrid/hc"
)

type mapTranslator map[string]string

func (m mapTranslator) T(key string, args ...any) string {
	msg, ok := m[key]
	if !ok {
		if len(args) == 0 {
			return key
		}
		return fmt.Sprintf(key, args...)
	}
	if len(args) == 0 {
		return msg
	}
	return fmt.Sprintf(msg, args...)
}

func writeFile(t *testing.T, dir, name, contents string) string {
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

func TestProvider_AcceptLanguageNegotiation(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeFile(t, tmp, "components/greeting.html", `<span data-locale="{{ Locale }}">{{ T "hello" }}</span>`)
	pagePath := writeFile(t, tmp, "pages/page.gohtml", `<Greeting />`)

	translations := map[string]map[string]string{
		"en": {"hello": "Hello"},
		"fr": {"hello": "Bonjour"},
	}
	loaderHits := make(map[string]int)

	options := Options{
		DefaultLocale:    "en",
		SupportedLocales: []string{"en", "fr"},
		Loader: func(ctx context.Context, locale string) (Translator, error) {
			loaderHits[locale]++
			if bundle, ok := translations[locale]; ok {
				return mapTranslator(bundle), nil
			}
			return nil, nil
		},
	}

	engine := hc.NewHC(filepath.Join(tmp, "components"), Provider(options))

	render := func(header string) string {
		ctx := context.Background()
		if header != "" {
			ctx = WithAcceptLanguage(ctx, header)
		}
		var buf bytes.Buffer
		if err := engine.ParseFileContext(ctx, &buf, pagePath, nil); err != nil {
			t.Fatalf("ParseFileContext(%q): %v", header, err)
		}
		return buf.String()
	}

	got := render("fr-CA,fr;q=0.9,en;q=0.6")
	want := `<span data-locale="fr">Bonjour</span>`
	if got != want {
		t.Fatalf("unexpected render output\nwant: %q\ngot:  %q", want, got)
	}

	if loaderHits["fr"] != 1 {
		t.Fatalf("loader invoked %d times for fr, want 1", loaderHits["fr"])
	}

	got = render("fr;q=0.8,en;q=0.7")
	if got != want {
		t.Fatalf("unexpected re-render output\nwant: %q\ngot:  %q", want, got)
	}

	if loaderHits["fr"] != 1 {
		t.Fatalf("expected cached translator for fr; hits=%d", loaderHits["fr"])
	}

	got = render("en-US,en;q=0.9")
	want = `<span data-locale="en">Hello</span>`
	if got != want {
		t.Fatalf("unexpected english render\nwant: %q\ngot:  %q", want, got)
	}

	if loaderHits["en"] != 1 {
		t.Fatalf("loader invoked %d times for en, want 1", loaderHits["en"])
	}
}

func TestProvider_DefaultLocaleFallback(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	writeFile(t, tmp, "components/greeting.html", `<span>{{ T "hello" }}</span>`)
	pagePath := writeFile(t, tmp, "pages/page.gohtml", `<Greeting />`)

	options := Options{
		DefaultLocale:    "en",
		SupportedLocales: []string{"en", "fr"},
		Loader: func(ctx context.Context, locale string) (Translator, error) {
			return mapTranslator(map[string]string{
				"hello": "Hello",
			}), nil
		},
	}

	engine := hc.NewHC(filepath.Join(tmp, "components"), Provider(options))

	var buf bytes.Buffer
	ctx := WithAcceptLanguage(context.Background(), "es-ES,es;q=0.9")
	if err := engine.ParseFileContext(ctx, &buf, pagePath, nil); err != nil {
		t.Fatalf("ParseFileContext: %v", err)
	}

	got := buf.String()
	want := `<span>Hello</span>`
	if got != want {
		t.Fatalf("default locale render mismatch\nwant: %q\ngot:  %q", want, got)
	}
}
