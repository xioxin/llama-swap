package config

import "fmt"

// ComfyUIConfig controls the optional dedicated ComfyUI listener. When Port
// is non-zero, llama-swap opens a second HTTP listener that serves the fixed
// comfyui_auto model at the path root, for ComfyUI clients (MCP servers,
// comfy-cli) that cannot address the /comfyui/ subdirectory.
//
// Unlike the /comfyui/ endpoint, any request path on the dedicated listener
// may start an unloaded model. Websocket handling still follows the model's
// compat.ignoreWebsockets setting.
type ComfyUIConfig struct {
	// Port is the TCP port of the dedicated listener (all interfaces).
	// 0 (the default) disables the listener; values must be 1..65535.
	Port int `yaml:"port"`
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
