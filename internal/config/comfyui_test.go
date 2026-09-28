package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const plainModel = "models:\n  m1:\n    cmd: svr\n    proxy: http://localhost:9999\n"

func TestConfig_ComfyUIPort_DefaultDisabled(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel))
	require.NoError(t, err)
	assert.Nil(t, cfg.ComfyUI)
}

func TestConfig_ComfyUIPort_Valid(t *testing.T) {
	for _, port := range []int{1, 8188, 65535} {
		content := fmt.Sprintf("%scomfyui:\n  port: %d\n", plainModel, port)
		cfg, err := LoadConfigFromReader(strings.NewReader(content))
		require.NoError(t, err, "port %d", port)
		require.NotNil(t, cfg.ComfyUI)
		assert.Equal(t, port, cfg.ComfyUI.Port)
	}
}

func TestConfig_ComfyUIPort_ZeroDisables(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  port: 0\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.Equal(t, 0, cfg.ComfyUI.Port)
}

func TestConfig_ComfyUIPort_InvalidValues(t *testing.T) {
	for _, port := range []int{-1, 65536, 100000} {
		content := fmt.Sprintf("%scomfyui:\n  port: %d\n", plainModel, port)
		_, err := LoadConfigFromReader(strings.NewReader(content))
		require.Error(t, err, "port %d should be rejected", port)
		assert.Contains(t, err.Error(), "comfyui.port")
	}
}

// With the default startPort of 5800, two ${PORT} models are allocated
// 5800 (a1) and 5801 (a2) in sorted model-ID order.
func TestConfig_ComfyUIPort_ConflictsWithAllocatedPorts(t *testing.T) {
	tests := []struct {
		port  int
		ok    bool
		model string
	}{
		{port: 5800, ok: false, model: "a1"},
		{port: 5801, ok: false, model: "a2"},
		{port: 5802, ok: true},
		{port: 8188, ok: true},
	}

	for _, tc := range tests {
		content := fmt.Sprintf(
			"models:\n  a1:\n    cmd: svr --port ${PORT}\n  a2:\n    cmd: svr --port ${PORT}\ncomfyui:\n  port: %d\n",
			tc.port)
		cfg, err := LoadConfigFromReader(strings.NewReader(content))
		if tc.ok {
			require.NoError(t, err, "port %d", tc.port)
			require.NotNil(t, cfg.ComfyUI)
			assert.Equal(t, tc.port, cfg.ComfyUI.Port)
			continue
		}
		require.Error(t, err, "port %d should conflict", tc.port)
		assert.Contains(t, err.Error(), "comfyui.port")
		assert.Contains(t, err.Error(), "conflicts with the automatic")
		assert.Contains(t, err.Error(), tc.model)
	}
}

func TestConfig_ComfyUIPort_Macro(t *testing.T) {
	content := "macros:\n  comfy_port: 8188\n" + plainModel + "comfyui:\n  port: ${comfy_port}\n"
	cfg, err := LoadConfigFromReader(strings.NewReader(content))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.Equal(t, 8188, cfg.ComfyUI.Port)
}
