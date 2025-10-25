package hc

import (
	"bytes"
	"context"
	"embed"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	texttmpl "text/template"
	"unicode"
	"unicode/utf8"
)

var ErrEmptyFile = errors.New("file is empty")

const maxComponentPasses = 16

type HC struct {
	folder string
	cfg    Config

	cache struct {
		mu      sync.RWMutex
		entries map[string]cacheEntry
		sources map[string]componentSource
	}
}

type cacheEntry struct {
	tpl *template.Template
}

type componentSource struct {
	content []byte
	source  string
}

type Config struct {
	fs              *embed.FS
	funcMap         template.FuncMap
	funcMapProvider func(context.Context) template.FuncMap
	dataAugmenter   func(context.Context, any) any
	cacheKeyFunc    func(context.Context, string) string
}

type Option func(*HC)

func NewHC(folder string, opts ...Option) *HC {
	hc := &HC{folder: folder}
	hc.cache.entries = make(map[string]cacheEntry)
	hc.cache.sources = make(map[string]componentSource)
	for _, opt := range opts {
		opt(hc)
	}
	if hc.cfg.funcMap == nil {
		hc.cfg.funcMap = template.FuncMap{}
	}
	return hc
}

func WithFS(fs embed.FS) Option {
	return func(h *HC) {
		h.cfg.fs = &fs
	}
}

func WithFuncMap(fm template.FuncMap) Option {
	return func(h *HC) {
		if fm == nil {
			return
		}
		if h.cfg.funcMap == nil {
			h.cfg.funcMap = template.FuncMap{}
		}
		maps.Copy(h.cfg.funcMap, fm)
	}
}

func WithFuncMapProvider(provider func(context.Context) template.FuncMap) Option {
	return func(h *HC) {
		h.cfg.funcMapProvider = provider
	}
}

func WithDataAugmenter(augmenter func(context.Context, any) any) Option {
	return func(h *HC) {
		h.cfg.dataAugmenter = augmenter
	}
}

func WithCacheKeyFunc(fn func(context.Context, string) string) Option {
	return func(h *HC) {
		h.cfg.cacheKeyFunc = fn
	}
}

func (h *HC) ParseFile(writer io.Writer, filename string, data any) error {
	return h.ParseFileContext(context.Background(), writer, filename, data)
}

func (h *HC) ParseFileContext(ctx context.Context, writer io.Writer, filename string, data any) error {
	raw, err := h.readFile(filename)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return ErrEmptyFile
	}

	mergedFuncs := h.mergedFuncMap(ctx)
	augmented := data
	if h.cfg.dataAugmenter != nil {
		if result := h.cfg.dataAugmenter(ctx, data); result != nil {
			augmented = result
		}
	}

	state := &renderState{
		ctx:   ctx,
		funcs: mergedFuncs,
		data:  h.dataWithContext(augmented, ctx),
	}

	rendered, err := h.renderAll(state, raw)
	if err != nil {
		return err
	}

	if writer != nil {
		_, err = writer.Write(rendered)
		return err
	}
	return nil
}

type renderState struct {
	ctx   context.Context
	funcs template.FuncMap
	data  any
}

func (h *HC) mergedFuncMap(ctx context.Context) template.FuncMap {
	var merged template.FuncMap
	if h.cfg.funcMap != nil {
		merged = maps.Clone(h.cfg.funcMap)
	} else {
		merged = template.FuncMap{}
	}

	if h.cfg.funcMapProvider == nil {
		return merged
	}

	dynamic := h.cfg.funcMapProvider(ctx)
	if len(dynamic) == 0 {
		return merged
	}

	if merged == nil {
		merged = template.FuncMap{}
	}
	for name, fn := range dynamic {
		merged[name] = fn
	}
	return merged
}

func (h *HC) dataWithContext(data any, ctx context.Context) any {
	if ctx == nil {
		return data
	}
	if data == nil {
		return map[string]any{"Ctx": ctx}
	}

	switch v := data.(type) {
	case map[string]any:
		if _, exists := v["Ctx"]; exists {
			return v
		}
		copy := make(map[string]any, len(v)+1)
		for k, val := range v {
			copy[k] = val
		}
		copy["Ctx"] = ctx
		return copy
	case *map[string]any:
		if v == nil {
			return data
		}
		return h.dataWithContext(*v, ctx)
	}

	rv := reflect.ValueOf(data)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return data
		}
		rv = rv.Elem()
	}

	if rv.Kind() == reflect.Map && rv.Type().Key().Kind() == reflect.String {
		copy := make(map[string]any, rv.Len()+1)
		iter := rv.MapRange()
		for iter.Next() {
			copy[iter.Key().String()] = iter.Value().Interface()
		}
		if _, exists := copy["Ctx"]; !exists {
			copy["Ctx"] = ctx
		}
		return copy
	}

	return data
}

func (h *HC) cacheKey(ctx context.Context, componentName string) string {
	base := strings.ToLower(componentName)
	if h.cfg.cacheKeyFunc == nil {
		return base
	}
	key := h.cfg.cacheKeyFunc(ctx, componentName)
	if strings.TrimSpace(key) == "" {
		return base
	}
	return key
}

func (h *HC) getComponentSource(name string) ([]byte, string, error) {
	cacheKey := strings.ToLower(name)

	h.cache.mu.RLock()
	if src, ok := h.cache.sources[cacheKey]; ok && src.content != nil {
		h.cache.mu.RUnlock()
		return src.content, src.source, nil
	}
	h.cache.mu.RUnlock()

	content, source, err := h.readComponentFile(name)
	if err != nil {
		return nil, "", err
	}

	h.cache.mu.Lock()
	h.cache.sources[cacheKey] = componentSource{
		content: content,
		source:  source,
	}
	h.cache.mu.Unlock()

	return content, source, nil
}

func (h *HC) readFile(name string) ([]byte, error) {
	if h.cfg.fs != nil {
		return h.cfg.fs.ReadFile(name)
	}
	return os.ReadFile(name)
}

func (h *HC) renderAll(state *renderState, input []byte) ([]byte, error) {
	current := append([]byte(nil), input...)
	for range maxComponentPasses {
		out, changed, err := h.replaceOnce(state, current)
		if err != nil {
			return nil, err
		}
		if !changed {
			return out, nil
		}
		current = out
	}
	return nil, fmt.Errorf("component rendering exceeded %d passes", maxComponentPasses)
}

func (h *HC) replaceOnce(state *renderState, input []byte) ([]byte, bool, error) {
	decoder := xml.NewDecoder(bytes.NewReader(input))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose
	var replacements []componentReplacement
	for {
		startOffset := decoder.InputOffset()
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, false, err
		}

		startElem, ok := token.(xml.StartElement)
		if !ok || !isComponentName(startElem.Name.Local) {
			continue
		}
		depth := 1
		var endOffset int64
		for depth > 0 {
			token, err = decoder.Token()
			if err == io.EOF {
				return nil, false, fmt.Errorf("unclosed component tag: %s", startElem.Name.Local)
			}
			if err != nil {
				return nil, false, err
			}
			switch token.(type) {
			case xml.StartElement:
				depth++
			case xml.EndElement:
				depth--
			}
			if depth == 0 {
				endOffset = decoder.InputOffset()
			}
		}

		start := int(startOffset)
		end := int(endOffset)
		if start < 0 || end > len(input) || start >= end {
			return nil, false, fmt.Errorf("invalid offsets for component %s", startElem.Name.Local)
		}

		raw := input[start:end]
		rendered, err := h.renderComponent(state, startElem, raw)
		if err != nil {
			return nil, false, err
		}

		replacements = append(replacements, componentReplacement{
			start:    start,
			end:      end,
			rendered: rendered,
		})
	}

	if len(replacements) == 0 {
		return input, false, nil
	}

	var buf bytes.Buffer
	cursor := 0
	for _, repl := range replacements {
		buf.Write(input[cursor:repl.start])
		buf.Write(repl.rendered)
		cursor = repl.end
	}
	buf.Write(input[cursor:])

	return buf.Bytes(), true, nil
}

func (h *HC) renderComponent(state *renderState, elem xml.StartElement, raw []byte) ([]byte, error) {
	tpl, err := h.loadComponentTemplate(state, elem.Name.Local)
	if err != nil {
		return nil, err
	}

	children, selfClosing, err := splitComponentBody(raw, elem.Name.Local)
	if err != nil {
		return nil, err
	}

	renderedChildren := template.HTML("")
	if len(children) > 0 {
		childOutput, err2 := h.renderAll(state, children)
		if err2 != nil {
			return nil, err2
		}
		renderedChildren = template.HTML(string(childOutput))
	}

	props, resolved, err := h.resolveAttrs(state, elem.Attr)
	if err != nil {
		return nil, err
	}

	payload := map[string]any{
		"Props":       props,
		"Attrs":       resolved,
		"Ctx":         state.ctx,
		"Data":        state.data,
		"Root":        state.data,
		"Component":   elem.Name.Local,
		"HasChildren": len(children) > 0,
		"ChildrenRaw": string(children),
		"Children":    renderedChildren,
		"SelfClosing": selfClosing,
	}

	var buf bytes.Buffer
	if err := tpl.Execute(&buf, payload); err != nil {
		return nil, fmt.Errorf("render component %s: %w", elem.Name.Local, err)
	}
	return buf.Bytes(), nil
}

func (h *HC) resolveAttrs(state *renderState, attrs []xml.Attr) (map[string]any, []resolvedAttr, error) {
	props := make(map[string]any, len(attrs))
	resolved := make([]resolvedAttr, 0, len(attrs))

	for _, attr := range attrs {
		name := attr.Name.Local
		value, err := h.evaluateAttr(state, attr.Value)
		if err != nil {
			return nil, nil, fmt.Errorf("attr %s: %w", name, err)
		}
		canonical := strings.ToLower(name)
		props[canonical] = value
		resolved = append(resolved, resolvedAttr{
			Name:      name,
			Canonical: canonical,
			Value:     value,
		})
	}
	return props, resolved, nil
}

func (h *HC) evaluateAttr(state *renderState, raw string) (any, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}

	funcs := texttmpl.FuncMap{}
	if len(state.funcs) > 0 {
		funcs = make(texttmpl.FuncMap, len(state.funcs))
		for name, fn := range state.funcs {
			funcs[name] = fn
		}
	}

	tpl, err := texttmpl.New("attr").Funcs(funcs).Option("missingkey=zero").Parse(raw)
	if err != nil {
		return "", err
	}

	var buf bytes.Buffer
	if err := tpl.Execute(&buf, state.data); err != nil {
		return "", err
	}

	return interpretAttrValue(buf.String()), nil
}

func (h *HC) loadComponentTemplate(state *renderState, name string) (*template.Template, error) {
	key := h.cacheKey(state.ctx, name)
	provider := h.cfg.funcMapProvider

	if provider == nil {
		h.cache.mu.RLock()
		if entry, ok := h.cache.entries[key]; ok && entry.tpl != nil {
			h.cache.mu.RUnlock()
			return entry.tpl, nil
		}
		h.cache.mu.RUnlock()
	}

	content, _, err := h.getComponentSource(name)
	if err != nil {
		return nil, err
	}

	funcs := h.componentFuncMap(state.funcs)
	tpl, err := template.New(name).Funcs(funcs).Option("missingkey=zero").Parse(string(content))
	if err != nil {
		return nil, fmt.Errorf("parse component %s: %w", name, err)
	}

	if provider == nil {
		h.cache.mu.Lock()
		h.cache.entries[key] = cacheEntry{tpl: tpl}
		h.cache.mu.Unlock()
	}

	return tpl, nil
}

func (h *HC) componentFuncMap(funcs template.FuncMap) template.FuncMap {
	merged := make(template.FuncMap, len(funcs)+1)
	for name, fn := range funcs {
		merged[name] = fn
	}
	merged["forwardAttrs"] = forwardAttrs
	return merged
}

func (h *HC) readComponentFile(name string) ([]byte, string, error) {
	var attempts []string
	for _, candidate := range componentFileCandidates(name) {
		if h.cfg.fs != nil {
			paths := uniqueFSPaths(h.folder, candidate)
			for _, p := range paths {
				data, err := h.cfg.fs.ReadFile(p)
				if err == nil {
					return data, p, nil
				}
				attempts = append(attempts, p)
			}
			continue
		}

		// If no embed FS is configured read from the host filesystem.
		fullPath := filepath.Join(h.folder, candidate)
		data, err := os.ReadFile(fullPath)
		if err == nil {
			return data, fullPath, nil
		}
		attempts = append(attempts, fullPath)
	}

	// If we never even built an attempt list the component simply does not exist.
	if len(attempts) == 0 {
		return nil, "", fmt.Errorf("component %s not found", name)
	}
	// Provide a detailed error message listing every path that was checked.
	return nil, "", fmt.Errorf("component %s not found; looked in %s", name, strings.Join(attempts, ", "))
}

// componentReplacement records which slice of the original markup is being replaced.
type componentReplacement struct {
	start    int
	end      int
	rendered []byte
}

// resolvedAttr keeps the attribute name, its lower-case key, and the evaluated value.
type resolvedAttr struct {
	// Name preserves the author-written attribute casing for forwarding to templates.
	Name string
	// Canonical holds the lower-cased attribute name used for comparisons and exclusions.
	Canonical string
	// Value is the evaluated attribute result (string, bool, etc.).
	Value any
}

// splitComponentBody separates child markup from the outer tag and detects self-closing tags.
func splitComponentBody(raw []byte, name string) ([]byte, bool, error) {
	// openEnd locates the closing bracket of the start tag.
	openEnd := bytes.IndexByte(raw, '>')
	if openEnd == -1 {
		return nil, false, fmt.Errorf("component %s has no closing bracket", name)
	}

	// selfClosing becomes true when the tag ends with "/>".
	selfClosing := false
	for i := openEnd - 1; i >= 0; i-- {
		if raw[i] == '/' {
			selfClosing = true
			break
		}
		if !isSpaceByte(raw[i]) {
			break
		}
	}

	if selfClosing {
		return nil, true, nil
	}

	closeToken := "</" + name
	closeIdx := bytes.LastIndex(raw, []byte(closeToken))
	if closeIdx == -1 {
		return nil, false, fmt.Errorf("component %s missing closing tag", name)
	}

	children := raw[openEnd+1 : closeIdx]
	return children, false, nil
}

func componentFileCandidates(name string) []string {
	var candidates []string
	seen := make(map[string]struct{})

	lower := strings.ToLower(name)
	kebab := toKebabCase(name)
	basenames := []string{name, lower, kebab}
	exts := []string{".gohtml", ".tmpl", ".html"}

	for _, base := range basenames {
		if base == "" {
			continue
		}
		for _, ext := range exts {
			filename := base + ext
			if _, ok := seen[filename]; ok {
				continue
			}
			seen[filename] = struct{}{}
			candidates = append(candidates, filename)
		}
	}
	return candidates
}

func uniqueFSPaths(base, candidate string) []string {
	paths := []string{}
	seen := make(map[string]struct{})

	add := func(p string) {
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	if base != "" {
		add(path.Join(base, candidate))
	}
	add(candidate)
	return paths
}

func isComponentName(name string) bool {
	if name == "" {
		return false
	}
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func interpretAttrValue(raw string) any {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	if trimmed == "true" || trimmed == "false" {
		if val, err := strconv.ParseBool(trimmed); err == nil {
			return val
		}
	}

	return raw
}

func forwardAttrs(attrs []resolvedAttr, exclude ...string) template.HTMLAttr {
	if len(attrs) == 0 {
		return ""
	}

	skip := make(map[string]struct{}, len(exclude))
	for _, name := range exclude {
		if name == "" {
			continue
		}
		skip[strings.ToLower(name)] = struct{}{}
	}

	var buf strings.Builder
	for _, attr := range attrs {
		if _, ok := skip[attr.Canonical]; ok {
			continue
		}

		switch v := attr.Value.(type) {
		case nil:
			continue
		case bool:
			if v {
				buf.WriteByte(' ')
				buf.WriteString(html.EscapeString(attr.Name))
			}
		default:
			str := fmt.Sprint(v)
			if str == "" {
				continue
			}
			buf.WriteByte(' ')
			buf.WriteString(html.EscapeString(attr.Name))
			buf.WriteString(`="`)
			buf.WriteString(html.EscapeString(str))
			buf.WriteByte('"')
		}
	}
	return template.HTMLAttr(buf.String())
}

func toKebabCase(name string) string {
	if name == "" {
		return ""
	}

	var b strings.Builder
	b.Grow(len(name) + 4)
	prevDash := false
	for i, r := range name {
		switch {
		case unicode.IsUpper(r):
			if i > 0 && !prevDash {
				b.WriteByte('-')
			}
			b.WriteRune(unicode.ToLower(r))
			prevDash = false
		case unicode.IsLetter(r):
			b.WriteRune(unicode.ToLower(r))
			prevDash = false
		case unicode.IsDigit(r):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func isSpaceByte(b byte) bool {
	return b == ' ' || b == '\n' || b == '\r' || b == '\t'
}
