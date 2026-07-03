package browser

import (
	"context"
	"fmt"
	"time"

	"github.com/chromedp/chromedp"
)

type Config struct {
	Headless  bool
	ChromePath string
	Timeout   time.Duration
}

func DefaultConfig() Config {
	return Config{
		Headless: true,
		Timeout:  30 * time.Second,
	}
}

type Instance struct {
	allocCtx    context.Context
	allocCancel context.CancelFunc
	cfg         Config
}

func NewInstance(ctx context.Context, cfg Config) (*Instance, error) {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", cfg.Headless),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", false),
		chromedp.Flag("window-size", "1920,1080"),
		chromedp.Flag("user-agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)

	if cfg.ChromePath != "" {
		opts = append(opts, chromedp.ExecPath(cfg.ChromePath))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(ctx, opts...)

	return &Instance{
		allocCtx:    allocCtx,
		allocCancel: allocCancel,
		cfg:         cfg,
	}, nil
}

func (i *Instance) NewContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := chromedp.NewContext(parent, chromedp.WithLogf(func(s string, args ...any) {
		fmt.Printf("[browser] "+s+"\n", args...)
	}))
	return ctx, cancel
}

func (i *Instance) NewTab(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := chromedp.NewContext(parent)
	return ctx, cancel
}

func (i *Instance) Close() {
	i.allocCancel()
}
