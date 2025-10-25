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
	"time"
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

type PostProcessor func(context.Context, []byte, any, template.FuncMap) ([]byte, error)

type LocaleExtractor func(context.Context) string

type ComponentInstrumentationStage string

const (
	ComponentStageBegin ComponentInstrumentationStage = "begin"
	ComponentStageEnd   ComponentInstrumentationStage = "end"
)

type ComponentInstrumentationEvent struct {
	Component string
	Stage     ComponentInstrumentationStage
	Err       error
	Duration  time.Duration
}

type ComponentInstrumentationHook func(context.Context, ComponentInstrumentationEvent)

type ComponentAugmenter func(context.Context, string, map[string]any) error

type AttrRuleOption func(*attrPolicy)

type attrPolicy struct {
	required    map[string]struct{}
	allowed     map[string]struct{}
	allowOthers bool
}

type Config struct {
	fs                  *embed.FS
	funcMap             template.FuncMap
	funcMapProvider     func(context.Context) template.FuncMap
	dataAugmenter       func(context.Context, any) any
	cacheKeyFunc        func(context.Context, string) string
	finalTemplatePass   bool
	postProcessors      []PostProcessor
	pagePipelines       [][]PostProcessor
	streamingWrites     bool
	localeExtractor     LocaleExtractor
	localeFallback      string
	componentAugmenters map[string][]ComponentAugmenter
	attrPolicies        map[string]attrPolicy
	instrumentHooks     []ComponentInstrumentationHook
}

type Option func(*HC)

func NewHC(folder string, opts ...Option) *HC {
	hc := &HC{folder: folder}
	hc.cache.entries = make(map[string]cacheEntry)
	hc.cache.sources = make(map[string]componentSource)
	hc.cfg.componentAugmenters = make(map[string][]ComponentAugmenter)
	hc.cfg.attrPolicies = make(map[string]attrPolicy)
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

func WithFinalTemplatePass() Option {
	return func(h *HC) {
		h.cfg.finalTemplatePass = true
	}
}

func WithPostProcessor(proc PostProcessor) Option {
	return func(h *HC) {
		if proc == nil {
			return
		}
		h.cfg.postProcessors = append(h.cfg.postProcessors, proc)
	}
}

func WithPagePipeline(steps ...PostProcessor) Option {
	return func(h *HC) {
		filtered := make([]PostProcessor, 0, len(steps))
		for _, step := range steps {
			if step != nil {
				filtered = append(filtered, step)
			}
		}
		if len(filtered) == 0 {
			return
		}
		h.cfg.pagePipelines = append(h.cfg.pagePipelines, filtered)
	}
}

func WithStreamingWrites() Option {
	return func(h *HC) {
		h.cfg.streamingWrites = true
	}
}

func WithLocaleCacheKeys(defaultLocale string, extractor LocaleExtractor) Option {
	return func(h *HC) {
		h.cfg.localeFallback = strings.TrimSpace(defaultLocale)
		h.cfg.localeExtractor = extractor
	}
}

func WithLocaleCacheKeysFromValue(key any, defaultLocale string) Option {
	return WithLocaleCacheKeys(defaultLocale, func(ctx context.Context) string {
		if ctx == nil {
			return ""
		}
		if v := ctx.Value(key); v != nil {
			if str, ok := v.(string); ok {
				return str
			}
		}
		return ""
	})
}

func WithComponentAugmenter(component string, augmenter ComponentAugmenter) Option {
	return func(h *HC) {
		if augmenter == nil {
			return
		}
		name := strings.TrimSpace(component)
		if name == "" {
			name = "*"
		}
		key := strings.ToLower(name)
		if name == "*" {
			key = "*"
		}
		h.cfg.componentAugmenters[key] = append(h.cfg.componentAugmenters[key], augmenter)
	}
}

func WithAttrRules(component string, opts ...AttrRuleOption) Option {
	return func(h *HC) {
		name := strings.TrimSpace(component)
		if name == "" {
			return
		}
		policy := attrPolicy{
			required: make(map[string]struct{}),
			allowed:  make(map[string]struct{}),
		}
		for _, opt := range opts {
			if opt != nil {
				opt(&policy)
			}
		}
		for key := range policy.required {
			policy.allowed[key] = struct{}{}
		}
		h.cfg.attrPolicies[strings.ToLower(name)] = policy
	}
}

func RequireAttrs(names ...string) AttrRuleOption {
	return func(policy *attrPolicy) {
		if policy.required == nil {
			policy.required = make(map[string]struct{})
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			policy.required[strings.ToLower(name)] = struct{}{}
		}
	}
}

func AllowAttrs(names ...string) AttrRuleOption {
	return func(policy *attrPolicy) {
		if policy.allowed == nil {
			policy.allowed = make(map[string]struct{})
		}
		for _, name := range names {
			if name == "" {
				continue
			}
			policy.allowed[strings.ToLower(name)] = struct{}{}
		}
	}
}

func AllowOtherAttrs() AttrRuleOption {
	return func(policy *attrPolicy) {
		policy.allowOthers = true
	}
}

func WithComponentInstrumentation(hook ComponentInstrumentationHook) Option {
	return func(h *HC) {
		if hook == nil {
			return
		}
		h.cfg.instrumentHooks = append(h.cfg.instrumentHooks, hook)
	}
}

func (h *HC) ParseFile(writer io.Writer, filename string, data any) error {
	return h.ParseFileContext(context.Background(), writer, filename, data)
}

func (h *HC) ParseFileContext(ctx context.Context, writer io.Writer, filename string, data any) error {
	raw, state, err := h.prepareRenderState(ctx, filename, data)
	if err != nil {
		return err
	}

	canStream := h.cfg.streamingWrites && writer != nil && !h.cfg.finalTemplatePass && len(h.cfg.postProcessors) == 0 && len(h.cfg.pagePipelines) == 0
	if canStream {
		return h.renderStreaming(state, raw, writer)
	}

	rendered, err := h.renderMarkupBytes(state, raw, 0)
	if err != nil {
		return err
	}

	final, err := h.applyPostProcessing(state, rendered, h.cfg.finalTemplatePass)
	if err != nil {
		return err
	}

	if writer != nil {
		_, err = writer.Write(final)
		return err
	}
	return nil
}

func (h *HC) ParseFileTemplate(ctx context.Context, writer io.Writer, filename string, data any) error {
	raw, state, err := h.prepareRenderState(ctx, filename, data)
	if err != nil {
		return err
	}

	rendered, err := h.renderMarkupBytes(state, raw, 0)
	if err != nil {
		return err
	}

	final, err := h.applyPostProcessing(state, rendered, true)
	if err != nil {
		return err
	}

	if writer != nil {
		_, err = writer.Write(final)
		return err
	}
	return nil
}

func (h *HC) prepareRenderState(ctx context.Context, filename string, data any) ([]byte, *renderState, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	raw, err := h.readFile(filename)
	if err != nil {
		return nil, nil, err
	}
	if len(raw) == 0 {
		return nil, nil, ErrEmptyFile
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
	return raw, state, nil
}

func (h *HC) renderStreaming(state *renderState, input []byte, writer io.Writer) error {
	if writer == nil {
		return errors.New("streaming requires a writer")
	}
	return h.renderMarkupStream(state, input, writer, 0)
}

func (h *HC) renderMarkupBytes(state *renderState, input []byte, depth int) ([]byte, error) {
	var buf bytes.Buffer
	if err := h.renderMarkupStream(state, input, &buf, depth); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (h *HC) renderMarkupStream(state *renderState, input []byte, writer io.Writer, depth int) error {
	decoder := xml.NewDecoder(bytes.NewReader(input))
	decoder.Strict = false
	decoder.AutoClose = xml.HTMLAutoClose

	cursor := 0
	for {
		startOffset := decoder.InputOffset()
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}

		startElem, ok := token.(xml.StartElement)
		if !ok || !isComponentName(startElem.Name.Local) {
			continue
		}

		depthCount := 1
		var endOffset int64
		for depthCount > 0 {
			token, err = decoder.Token()
			if err == io.EOF {
				return fmt.Errorf("unclosed component tag: %s", startElem.Name.Local)
			}
			if err != nil {
				return err
			}
			switch token.(type) {
			case xml.StartElement:
				depthCount++
			case xml.EndElement:
				depthCount--
			}
			if depthCount == 0 {
				endOffset = decoder.InputOffset()
			}
		}

		start := int(startOffset)
		end := int(endOffset)
		if start < 0 || end > len(input) || start >= end {
			return fmt.Errorf("invalid offsets for component %s", startElem.Name.Local)
		}

		if start > cursor {
			if _, err := writer.Write(input[cursor:start]); err != nil {
				return err
			}
		}

		rendered, err := h.renderComponent(state, startElem, input[start:end], depth+1)
		if err != nil {
			return err
		}

		if err := h.renderMarkupStream(state, rendered, writer, depth+1); err != nil {
			return err
		}

		cursor = end
	}

	if cursor < len(input) {
		_, err := writer.Write(input[cursor:])
		return err
	}
	return nil
}

func (h *HC) applyPostProcessing(state *renderState, input []byte, enableFinalTemplate bool) ([]byte, error) {
	current := input
	var err error

	if enableFinalTemplate {
		current, err = h.executeFinalTemplate(state, current)
		if err != nil {
			return nil, err
		}
	}

	if len(h.cfg.pagePipelines) > 0 {
		for _, pipeline := range h.cfg.pagePipelines {
			for _, step := range pipeline {
				current, err = step(state.ctx, current, state.data, state.funcs)
				if err != nil {
					return nil, err
				}
				if current == nil {
					current = []byte{}
				}
			}
		}
	}

	if len(h.cfg.postProcessors) == 0 {
		return current, nil
	}

	for _, proc := range h.cfg.postProcessors {
		current, err = proc(state.ctx, current, state.data, state.funcs)
		if err != nil {
			return nil, err
		}
		if current == nil {
			current = []byte{}
		}
	}

	return current, nil
}

func (h *HC) emitInstrumentation(ctx context.Context, component string, stage ComponentInstrumentationStage, err error, duration time.Duration) {
	if len(h.cfg.instrumentHooks) == 0 {
		return
	}
	event := ComponentInstrumentationEvent{
		Component: component,
		Stage:     stage,
		Err:       err,
		Duration:  duration,
	}
	for _, hook := range h.cfg.instrumentHooks {
		hook(ctx, event)
	}
}

func (h *HC) executeFinalTemplate(state *renderState, input []byte) ([]byte, error) {
	tpl := template.New("hc-final").Option("missingkey=zero")
	if len(state.funcs) > 0 {
		tpl = tpl.Funcs(state.funcs)
	}

	parsed, err := tpl.Parse(string(input))
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := parsed.Execute(&buf, state.data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (h *HC) applyComponentAugmenters(state *renderState, component string, payload map[string]any) error {
	if len(h.cfg.componentAugmenters) == 0 {
		return nil
	}

	if augmenters := h.cfg.componentAugmenters["*"]; len(augmenters) > 0 {
		for _, aug := range augmenters {
			if err := aug(state.ctx, component, payload); err != nil {
				return err
			}
		}
	}

	if augmenters := h.cfg.componentAugmenters[strings.ToLower(component)]; len(augmenters) > 0 {
		for _, aug := range augmenters {
			if err := aug(state.ctx, component, payload); err != nil {
				return err
			}
		}
	}

	return nil
}

func (h *HC) validateAttributes(component string, props map[string]any) error {
	if len(h.cfg.attrPolicies) == 0 {
		return nil
	}

	policy, ok := h.cfg.attrPolicies[strings.ToLower(component)]
	if !ok {
		return nil
	}

	for req := range policy.required {
		if _, ok := props[req]; !ok {
			return fmt.Errorf("component %s missing required attr %q", component, req)
		}
	}

	if policy.allowOthers {
		return nil
	}

	for name := range props {
		if _, ok := policy.allowed[name]; !ok {
			return fmt.Errorf("component %s received unsupported attr %q", component, name)
		}
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
	if h.cfg.cacheKeyFunc != nil {
		if key := strings.TrimSpace(h.cfg.cacheKeyFunc(ctx, componentName)); key != "" {
			base = key
		}
	}

	if h.cfg.localeExtractor != nil {
		locale := strings.TrimSpace(h.cfg.localeExtractor(ctx))
		if locale == "" {
			locale = h.cfg.localeFallback
		}
		if locale != "" {
			base = strings.ToLower(locale) + ":" + base
		}
	}

	return base
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

func (h *HC) renderComponent(state *renderState, elem xml.StartElement, raw []byte, depth int) ([]byte, error) {
	component := elem.Name.Local
	start := time.Now()
	h.emitInstrumentation(state.ctx, component, ComponentStageBegin, nil, 0)

	var execErr error
	defer func() {
		h.emitInstrumentation(state.ctx, component, ComponentStageEnd, execErr, time.Since(start))
	}()

	if depth > maxComponentPasses {
		execErr = fmt.Errorf("component rendering exceeded %d passes", maxComponentPasses)
		return nil, execErr
	}

	tpl, err := h.loadComponentTemplate(state, component)
	if err != nil {
		execErr = err
		return nil, err
	}

	children, selfClosing, err := splitComponentBody(raw, component)
	if err != nil {
		execErr = err
		return nil, err
	}

	renderedChildren := template.HTML("")
	if len(children) > 0 {
		childOutput, err2 := h.renderMarkupBytes(state, children, depth+1)
		if err2 != nil {
			execErr = err2
			return nil, err2
		}
		renderedChildren = template.HTML(string(childOutput))
	}

	props, resolved, err := h.resolveAttrs(state, elem.Attr)
	if err != nil {
		execErr = err
		return nil, err
	}

	if err := h.validateAttributes(component, props); err != nil {
		execErr = err
		return nil, err
	}

	payload := map[string]any{
		"Props":       props,
		"Attrs":       resolved,
		"Ctx":         state.ctx,
		"Data":        state.data,
		"Root":        state.data,
		"Component":   component,
		"HasChildren": len(children) > 0,
		"ChildrenRaw": string(children),
		"Children":    renderedChildren,
		"SelfClosing": selfClosing,
	}

	if err := h.applyComponentAugmenters(state, component, payload); err != nil {
		execErr = err
		return nil, err
	}

	var buf bytes.Buffer
	if err := tpl.Execute(&buf, payload); err != nil {
		execErr = fmt.Errorf("render component %s: %w", component, err)
		return nil, execErr
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

	content, source, err := h.getComponentSource(name)
	if err != nil {
		return nil, err
	}

	funcs := h.componentFuncMap(state.funcs)
	tpl, err := template.New(name).Funcs(funcs).Option("missingkey=zero").Parse(string(content))
	if err != nil {
		if tmplErr, ok := err.(*template.Error); ok {
			location := source
			if location == "" {
				location = name
			}
			if tmplErr.Line > 0 {
				return nil, fmt.Errorf("parse component %s (%s:%d): %s", name, location, tmplErr.Line, tmplErr.Description)
			}
			return nil, fmt.Errorf("parse component %s (%s): %s", name, location, tmplErr.Description)
		}
		if source != "" {
			return nil, fmt.Errorf("parse component %s (%s): %w", name, source, err)
		}
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
