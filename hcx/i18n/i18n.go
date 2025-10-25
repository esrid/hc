package i18n

import (
	"context"
	"fmt"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/esrid/hc"
)

type Translator interface {
	T(key string, args ...any) string
}

type LoaderFunc func(context.Context, string) (Translator, error)

type Options struct {
	Loader           LoaderFunc
	DefaultLocale    string
	SupportedLocales []string
	HeaderExtractor  func(context.Context) string
}

type acceptLanguageKey struct{}

func WithAcceptLanguage(ctx context.Context, header string) context.Context {
	return context.WithValue(ctx, acceptLanguageKey{}, header)
}

func AcceptLanguageFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(acceptLanguageKey{}).(string); ok {
		return v
	}
	return ""
}

func Provider(opts Options) hc.Option {
	defaultLocale := opts.DefaultLocale
	if defaultLocale == "" {
		defaultLocale = "en"
	}
	defaultLower := normalizeLocale(defaultLocale)

	headerExtractor := opts.HeaderExtractor
	if headerExtractor == nil {
		headerExtractor = AcceptLanguageFromContext
	}

	supported := make(map[string]string, len(opts.SupportedLocales)+1)
	for _, loc := range opts.SupportedLocales {
		if loc == "" {
			continue
		}
		norm := normalizeLocale(loc)
		if _, exists := supported[norm]; !exists {
			supported[norm] = loc
		}
	}
	if len(supported) > 0 {
		if _, exists := supported[defaultLower]; !exists {
			supported[defaultLower] = defaultLocale
		}
	}

	cache := &translatorCache{
		loader: opts.Loader,
		items:  make(map[string]Translator),
	}

	return hc.WithFuncMapProvider(func(ctx context.Context) template.FuncMap {
		header := headerExtractor(ctx)
		locale := pickLocale(header, defaultLocale, supported)
		translator := cache.get(ctx, locale)

		translate := func(key string, args ...any) string {
			if translator == nil {
				if len(args) == 0 {
					return key
				}
				return fmt.Sprintf(key, args...)
			}
			return translator.T(key, args...)
		}

		return template.FuncMap{
			"T":      translate,
			"Locale": func() string { return locale },
		}
	})
}

type translatorCache struct {
	loader LoaderFunc
	mu     sync.RWMutex
	items  map[string]Translator
}

func (c *translatorCache) get(ctx context.Context, locale string) Translator {
	if c == nil || c.loader == nil {
		return nil
	}

	c.mu.RLock()
	if tr, ok := c.items[locale]; ok {
		c.mu.RUnlock()
		return tr
	}
	c.mu.RUnlock()

	tr, err := c.loader(ctx, locale)
	if err != nil || tr == nil {
		return nil
	}

	c.mu.Lock()
	c.items[locale] = tr
	c.mu.Unlock()
	return tr
}

func pickLocale(header string, defaultLocale string, supported map[string]string) string {
	defaultNorm := normalizeLocale(defaultLocale)
	if header == "" {
		return canonicalLocale(defaultNorm, defaultLocale, supported)
	}

	prefs := parseAcceptLanguage(header)
	if len(prefs) == 0 {
		return canonicalLocale(defaultNorm, defaultLocale, supported)
	}

	if len(supported) == 0 {
		return normalizeLocale(prefs[0].tag)
	}

	for _, pref := range prefs {
		norm := normalizeLocale(pref.tag)
		if locale, ok := supported[norm]; ok {
			return locale
		}
		if base := baseLocale(norm); base != "" {
			if locale, ok := supported[base]; ok {
				return locale
			}
		}
	}

	return canonicalLocale(defaultNorm, defaultLocale, supported)
}

type languagePreference struct {
	tag   string
	q     float64
	order int
}

func parseAcceptLanguage(header string) []languagePreference {
	raw := strings.Split(header, ",")
	prefs := make([]languagePreference, 0, len(raw))

	for idx, part := range raw {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		tag := part
		quality := 1.0

		if semi := strings.Index(part, ";"); semi != -1 {
			tag = strings.TrimSpace(part[:semi])
			params := strings.Split(part[semi+1:], ";")
			for _, param := range params {
				param = strings.TrimSpace(param)
				if strings.HasPrefix(param, "q=") {
					if val, err := strconv.ParseFloat(strings.TrimSpace(param[2:]), 64); err == nil {
						quality = val
					}
				}
			}
		}

		if tag == "" || quality <= 0 {
			continue
		}

		prefs = append(prefs, languagePreference{
			tag:   tag,
			q:     quality,
			order: idx,
		})
	}

	sort.SliceStable(prefs, func(i, j int) bool {
		if prefs[i].q == prefs[j].q {
			return prefs[i].order < prefs[j].order
		}
		return prefs[i].q > prefs[j].q
	})

	return prefs
}

func normalizeLocale(tag string) string {
	tag = strings.ReplaceAll(tag, "_", "-")
	return strings.ToLower(strings.TrimSpace(tag))
}

func baseLocale(tag string) string {
	if idx := strings.Index(tag, "-"); idx != -1 {
		return tag[:idx]
	}
	return ""
}

func canonicalLocale(normDefault, fallback string, supported map[string]string) string {
	if locale, ok := supported[normDefault]; ok {
		return locale
	}
	if fallback != "" {
		return fallback
	}
	return normDefault
}
