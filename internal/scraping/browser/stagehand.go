package browser

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/sashabaranov/go-openai"
)

type SelectorEntry struct {
	CSS            string
	Description    string
	FallbackChain  []string
	LastVerifiedAt time.Time
	HealedFrom     string
}

type SelectorRegistry struct {
	mu      sync.RWMutex
	entries map[string]*SelectorEntry
	service string
}

func NewSelectorRegistry(service string) *SelectorRegistry {
	return &SelectorRegistry{
		entries: make(map[string]*SelectorEntry),
		service: service,
	}
}

func (r *SelectorRegistry) Register(key, css, description string, fallbacks ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[key] = &SelectorEntry{
		CSS:           css,
		Description:   description,
		FallbackChain: fallbacks,
	}
}

func (r *SelectorRegistry) Get(key string) (*SelectorEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[key]
	if !ok {
		return nil, false
	}
	cpy := *e
	return &cpy, true
}

func (r *SelectorRegistry) UpdateHealed(key, newCSS, healedFrom string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entries[key]; ok {
		e.FallbackChain = append([]string{e.CSS}, e.FallbackChain...)
		e.CSS = newCSS
		e.HealedFrom = healedFrom
		e.LastVerifiedAt = time.Now()
	}
}

type StagehandConfig struct {
	OpenAIKey   string
	Model       string
	MaxRetries  int
	HealTimeout time.Duration
}

func DefaultStagehandConfig() StagehandConfig {
	return StagehandConfig{
		Model:       openai.GPT4oMini,
		MaxRetries:  2,
		HealTimeout: 15 * time.Second,
	}
}

type Stagehand struct {
	cfg      StagehandConfig
	client   *openai.Client
	service  string
	registry *SelectorRegistry
}

func NewStagehand(service string, cfg StagehandConfig, registry *SelectorRegistry) *Stagehand {
	var client *openai.Client
	if cfg.OpenAIKey != "" {
		client = openai.NewClient(cfg.OpenAIKey)
	}
	return &Stagehand{
		cfg:      cfg,
		client:   client,
		service:  service,
		registry: registry,
	}
}

func (s *Stagehand) Click(ctx context.Context, selectorKey string) error {
	return s.executeWithHealing(ctx, selectorKey, func(sel string) error {
		return chromedp.Do(ctx, chromedp.Click(chromedp.CSS(sel)))
	})
}

func (s *Stagehand) SendKeys(ctx context.Context, selectorKey, value string) error {
	return s.executeWithHealing(ctx, selectorKey, func(sel string) error {
		return chromedp.Do(ctx, chromedp.SendKeys(chromedp.CSS(sel), value))
	})
}

func (s *Stagehand) GetValue(ctx context.Context, selectorKey string, result *string) error {
	return s.executeWithHealing(ctx, selectorKey, func(sel string) error {
		v, err := chromedp.Run(ctx, chromedp.Value(chromedp.CSS(sel)))
		if err != nil {
			return err
		}
		*result = v
		return nil
	})
}

func (s *Stagehand) GetText(ctx context.Context, selectorKey string, result *string) error {
	return s.executeWithHealing(ctx, selectorKey, func(sel string) error {
		v, err := chromedp.Run(ctx, chromedp.Text(chromedp.CSS(sel)))
		if err != nil {
			return err
		}
		*result = v
		return nil
	})
}

func (s *Stagehand) WaitVisible(ctx context.Context, selectorKey string, opts ...chromedp.QueryOption) error {
	return s.executeWithHealing(ctx, selectorKey, func(sel string) error {
		return chromedp.Do(ctx, chromedp.WaitVisible(chromedp.CSS(sel)))
	})
}

func (s *Stagehand) WaitReady(ctx context.Context, selectorKey string) error {
	return s.executeWithHealing(ctx, selectorKey, func(sel string) error {
		return chromedp.Do(ctx, chromedp.WaitReady(chromedp.CSS(sel)))
	})
}

func (s *Stagehand) Navigate(ctx context.Context, url string) error {
	return chromedp.Do(ctx, chromedp.Navigate(url))
}

func (s *Stagehand) Exists(ctx context.Context, selectorKey string) (bool, error) {
	entry, ok := s.registry.Get(selectorKey)
	if !ok {
		return false, fmt.Errorf("stagehand: selector %q not registered for %s", selectorKey, s.service)
	}

	selectors := []string{entry.CSS}
	selectors = append(selectors, entry.FallbackChain...)

	for _, sel := range selectors {
		nodes, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll(sel)))
		if err != nil {
			continue
		}
		if len(nodes) > 0 {
			return true, nil
		}
	}

	return false, nil
}

func (s *Stagehand) MaybeClick(ctx context.Context, selectorKey string) (bool, error) {
	entry, ok := s.registry.Get(selectorKey)
	if !ok {
		return false, fmt.Errorf("stagehand: selector %q not registered for %s", selectorKey, s.service)
	}

	selectors := []string{entry.CSS}
	selectors = append(selectors, entry.FallbackChain...)

	for i, sel := range selectors {
		nodes, err := chromedp.Run(ctx, chromedp.Nodes(chromedp.CSSAll(sel)))
		if err != nil {
			continue
		}
		if len(nodes) == 0 {
			continue
		}

		if err := chromedp.Do(ctx, chromedp.Click(chromedp.CSS(sel))); err != nil {
			return false, err
		}
		if i > 0 {
			s.registry.UpdateHealed(selectorKey, sel, "fallback")
		}
		return true, nil
	}

	return false, nil
}

func (s *Stagehand) Execute(ctx context.Context, actions ...chromedp.Action[chromedp.Void]) error {
	return chromedp.Do(ctx, actions...)
}

func (s *Stagehand) executeWithHealing(ctx context.Context, selectorKey string, fn func(string) error) error {
	entry, ok := s.registry.Get(selectorKey)
	if !ok {
		return fmt.Errorf("stagehand: selector %q not registered for %s", selectorKey, s.service)
	}

	selectors := []string{entry.CSS}
	selectors = append(selectors, entry.FallbackChain...)

	for i, sel := range selectors {
		err := fn(sel)
		if err == nil {
			if i > 0 {
				s.registry.UpdateHealed(selectorKey, sel, "fallback")
				log.Printf("[stagehand] %s/%s healed via fallback: %s → %s", s.service, selectorKey, selectors[0], sel)
			}
			return nil
		}
		if i < len(selectors)-1 {
			continue
		}
	}

	if s.client != nil {
		healed, newSel, err := s.healSelector(ctx, selectorKey, entry)
		if err != nil {
			return fmt.Errorf("stagehand: %s/%s failed and self-healing unsuccessful: %w", s.service, selectorKey, err)
		}
		if healed {
			s.registry.UpdateHealed(selectorKey, newSel, "ai")
			log.Printf("[stagehand] %s/%s healed via AI: %s → %s", s.service, selectorKey, entry.CSS, newSel)
			return fn(newSel)
		}
	}

	return fmt.Errorf("stagehand: %s/%s all selectors failed and AI healing unavailable", s.service, selectorKey)
}

func (s *Stagehand) healSelector(ctx context.Context, key string, entry *SelectorEntry) (bool, string, error) {
	healCtx, cancel := context.WithTimeout(ctx, s.cfg.HealTimeout)
	defer cancel()

	domSnapshot, err := chromedp.Run(healCtx,
		chromedp.Evaluate[string](`
			(() => {
				const snapshot = [];
				function walk(el, depth) {
					if (depth > 6 || !el || el.nodeType !== 1) return;
					const tag = el.tagName.toLowerCase();
					const id = el.id ? '#' + el.id : '';
					const cls = el.className && typeof el.className === 'string' ? '.' + el.className.trim().split(/\s+/).slice(0,3).join('.') : '';
					const type = el.getAttribute('type') ? '[type="' + el.getAttribute('type') + '"]' : '';
					const role = el.getAttribute('role') ? '[role="' + el.getAttribute('role') + '"]' : '';
					const aria = el.getAttribute('aria-label') ? '[aria-label="' + el.getAttribute('aria-label') + '"]' : '';
					const placeholder = el.getAttribute('placeholder') ? '[placeholder="' + el.getAttribute('placeholder') + '"]' : '';
					const text = el.childNodes.length === 1 && el.childNodes[0].nodeType === 3 ? el.childNodes[0].textContent.trim().slice(0,50) : '';
					snapshot.push('  '.repeat(depth) + tag + id + cls + type + role + aria + placeholder + (text ? ' "' + text + '"' : ''));
					for (const child of el.children) walk(child, depth + 1);
				}
				walk(document.body, 0);
				return snapshot.join('\n');
			})()
		`),
	)
	if err != nil {
		return false, "", fmt.Errorf("capture DOM snapshot: %w", err)
	}

	if len(domSnapshot) > 12000 {
		domSnapshot = domSnapshot[:12000]
	}

	prompt := fmt.Sprintf(`You are a web automation self-healing agent for the service "%s".

The original CSS selector "%s" for element "%s" has stopped working.

Here is the current DOM snapshot of the page:

%s

Find the NEW CSS selector for the element described as "%s".
Return ONLY a valid CSS selector string. No explanation, no markdown, no quotes.

Rules:
- Prefer id selectors (#id) when available
- Then attribute selectors ([type="..."], [aria-label="..."])
- Then class selectors (.class)
- Keep selectors short and stable
- Avoid overly specific selectors that are likely to break again`, s.service, entry.CSS, key, domSnapshot, entry.Description)

	resp, err := s.client.CreateChatCompletion(healCtx, openai.ChatCompletionRequest{
		Model: s.cfg.Model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "You are a CSS selector expert. Return only valid CSS selectors."},
			{Role: openai.ChatMessageRoleUser, Content: prompt},
		},
		MaxTokens:   100,
		Temperature: 0,
	})
	if err != nil {
		return false, "", fmt.Errorf("openai request: %w", err)
	}

	if len(resp.Choices) == 0 {
		return false, "", fmt.Errorf("openai returned no choices")
	}

	newSel := strings.TrimSpace(resp.Choices[0].Message.Content)
	newSel = stripMarkdown(newSel)

	if !isValidCSSSelector(newSel) {
		return false, "", fmt.Errorf("AI returned invalid CSS selector: %q", newSel)
	}

	log.Printf("[stagehand] AI proposed selector for %s/%s: %q", s.service, key, newSel)
	return true, newSel, nil
}

func stripMarkdown(s string) string {
	s = strings.TrimPrefix(s, "```css")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

func isValidCSSSelector(sel string) bool {
	if sel == "" || len(sel) > 500 {
		return false
	}
	if strings.Contains(sel, " ") && strings.Contains(sel, "eval") {
		return false
	}
	invalid := regexp.MustCompile(`(?i)(javascript:|on\w+=|<script|fetch\(|import\(|require\()`)
	return !invalid.MatchString(sel)
}
