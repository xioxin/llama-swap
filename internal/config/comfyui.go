package config

import (
	"fmt"
	"net/url"
	"time"
)

// ComfyUI swap modes. SwapMode controls what "swapping out" ComfyUI means.
const (
	// SwapModeUnload stops the ComfyUI process on a swap-out. This is the
	// default and matches how every other model is swapped.
	SwapModeUnload = "unload"

	// SwapModeCleanup keeps the ComfyUI process running on a swap-out and
	// only clears its GPU memory via POST /free. The process is still
	// stopped when its TTL expires or when it is explicitly unloaded.
	SwapModeCleanup = "cleanup"
)

// DefaultComfyUIVramFreeFraction is the fraction of total device VRAM that may
// still be in use while ComfyUI counts as idle. ComfyUI keeps a small CUDA
// context resident even after POST /free (measured at ~30-500MiB on a 33GiB
// card), so "no VRAM in use" can never be taken literally. A device whose used
// fraction is below this threshold counts as free.
const DefaultComfyUIVramFreeFraction = 0.02

// ComfyUIConfig controls the optional dedicated ComfyUI listener and how
// ComfyUI participates in model swapping.
//
// When Port is non-zero, llama-swap opens a second HTTP listener that serves
// the fixed comfyui_auto model at the path root, for ComfyUI clients (MCP
// servers, comfy-cli) that cannot address the /comfyui/ subdirectory.
//
// The listener applies the same start gating as the /comfyui/ endpoint: a
// GET to an ignored path (static assets, /ws, /api/jobs) cannot start an
// unloaded model. The only difference is that the model is served at the
// path root of the dedicated port instead of under the /comfyui/ prefix.
// Websocket handling still follows the model's compat.ignoreWebsockets
// setting.
type ComfyUIConfig struct {
	// Port is the TCP port of the dedicated listener (all interfaces).
	// 0 (the default) disables the listener; values must be 1..65535.
	Port int `yaml:"port"`

	// SwapMode selects how ComfyUI gives up the GPU when it is swapped out
	// for another model. "unload" (the default) stops the process;
	// "cleanup" keeps it running and only frees its GPU memory so the next
	// swap-in is fast. TTL expiry always stops the process regardless.
	SwapMode string `yaml:"swapMode"`

	// VramFreeFraction is the fraction of total device VRAM that may still
	// be in use while ComfyUI is treated as memory-free. Larger values
	// tolerate a bigger resident CUDA context; smaller values are stricter.
	// Defaults to DefaultComfyUIVramFreeFraction. Valid range 0..1.
	VramFreeFraction float64 `yaml:"vramFreeFraction"`

	// QueueTimeout is the maximum seconds a workflow submit (POST /prompt)
	// waits in llama-swap's queue for the GPU before it is rejected with 503.
	// 0 (the default) waits indefinitely. It bounds the request so a submit
	// can never hang forever behind a stuck model swap.
	QueueTimeout int `yaml:"queueTimeout"`
}

// QueueWait returns the maximum wait for a queued workflow submit. A nil
// section or 0 means wait indefinitely (returns 0).
func (c *ComfyUIConfig) QueueWait() time.Duration {
	if c == nil || c.QueueTimeout <= 0 {
		return 0
	}
	return time.Duration(c.QueueTimeout) * time.Second
}

// CleanupMode reports whether ComfyUI should be swapped out by clearing its
// GPU memory instead of stopping the process.
func (c *ComfyUIConfig) CleanupMode() bool {
	return c != nil && c.SwapMode == SwapModeCleanup
}

// IdleVramFreeFraction returns the VRAM threshold used for idle detection. A
// nil section, or an explicitly unset value, falls back to the default. A
// configured 0 is kept as-is (strict: only an entirely free device is idle).
func (c *ComfyUIConfig) IdleVramFreeFraction() float64 {
	if c == nil || c.VramFreeFraction == 0 {
		return DefaultComfyUIVramFreeFraction
	}
	return c.VramFreeFraction
}

// validateComfyUI checks the optional ComfyUI section. A nil section is always
// valid: the listener is disabled and ComfyUI swaps like any other model.
func validateComfyUI(config Config, allocated []AllocatedPort) error {
	if config.ComfyUI == nil {
		return nil
	}
	if err := validateComfyUIPort(config, allocated); err != nil {
		return err
	}
	if err := validateComfyUISwap(config.ComfyUI); err != nil {
		return err
	}
	return validateComfyUIModel(config)
}

// validateComfyUIModel checks that the fixed comfyui_auto model can be reached
// by the built-in ComfyUI client when a feature needs it (dedicated listener,
// or cleanup swap mode). Its proxy URL must be an absolute HTTP(S) URL: the
// client calls ComfyUI's API on it directly.
func validateComfyUIModel(config Config) error {
	needsClient := config.ComfyUI.CleanupMode() || config.ComfyUI.Port != 0
	if !needsClient {
		return nil
	}
	m, ok := config.Models[ComfyUIModelID]
	if !ok {
		// Not defined here (e.g. on a peer). The /comfyui endpoint reports
		// this at request time; the swap/listener features degrade gracefully.
		return nil
	}
	if m.Proxy == "" {
		return fmt.Errorf("comfyui: model %s needs an absolute proxy URL to reach its API", ComfyUIModelID)
	}
	u, err := url.Parse(m.Proxy)
	if err != nil {
		return fmt.Errorf("comfyui: model %s has an invalid proxy URL %q: %w", ComfyUIModelID, m.Proxy, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("comfyui: model %s proxy %q must be an absolute http(s) URL", ComfyUIModelID, m.Proxy)
	}
	return nil
}

// validateComfyUISwap checks swapMode and vramFreeFraction.
func validateComfyUISwap(c *ComfyUIConfig) error {
	switch c.SwapMode {
	case "", SwapModeUnload, SwapModeCleanup:
	default:
		return fmt.Errorf("comfyui.swapMode must be %q or %q, got %q", SwapModeUnload, SwapModeCleanup, c.SwapMode)
	}
	if c.VramFreeFraction < 0 || c.VramFreeFraction > 1 {
		return fmt.Errorf("comfyui.vramFreeFraction must be between 0 and 1, got %g", c.VramFreeFraction)
	}
	if c.QueueTimeout < 0 {
		return fmt.Errorf("comfyui.queueTimeout must be >= 0, got %d", c.QueueTimeout)
	}
	return nil
}

// validateComfyUIPort checks the optional dedicated ComfyUI listener port.
// A nil section or port 0 disables the listener and is always valid.
func validateComfyUIPort(config Config, allocated []AllocatedPort) error {
	if config.ComfyUI == nil || config.ComfyUI.Port == 0 {
		return nil
	}
	port := config.ComfyUI.Port
	if port < 1 || port > 65535 {
		return fmt.Errorf("comfyui.port must be 0 (disabled) or between 1 and 65535, got %d", port)
	}
	for _, alloc := range allocated {
		if alloc.Port == port {
			return fmt.Errorf("comfyui.port %d conflicts with the automatic ${PORT} port allocated to model %q", port, alloc.Model)
		}
	}
	return nil
}
