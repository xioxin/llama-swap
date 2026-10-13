package config

import (
	"fmt"
	"strings"
	"testing"
	"time"

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

func TestConfig_ComfyUISwapMode_DefaultUnload(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  port: 8188\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.Equal(t, "", cfg.ComfyUI.SwapMode)
	assert.False(t, cfg.ComfyUI.CleanupMode())
}

func TestConfig_ComfyUISwapMode_Cleanup(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  swapMode: cleanup\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.True(t, cfg.ComfyUI.CleanupMode())
}

func TestConfig_ComfyUISwapMode_ExplicitUnload(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  swapMode: unload\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.False(t, cfg.ComfyUI.CleanupMode())
}

func TestConfig_ComfyUISwapMode_Invalid(t *testing.T) {
	for _, mode := range []string{"soft", "Cleanup", "none"} {
		content := plainModel + "comfyui:\n  swapMode: " + mode + "\n"
		_, err := LoadConfigFromReader(strings.NewReader(content))
		require.Error(t, err, "swapMode %q should be rejected", mode)
		assert.Contains(t, err.Error(), "comfyui.swapMode")
	}
}

func TestConfig_ComfyUIVramFreeFraction_Default(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  swapMode: cleanup\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.Equal(t, DefaultComfyUIVramFreeFraction, cfg.ComfyUI.IdleVramFreeFraction())
}

func TestConfig_ComfyUIVramFreeFraction_Custom(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  vramFreeFraction: 0.05\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.InDelta(t, 0.05, cfg.ComfyUI.IdleVramFreeFraction(), 1e-9)
}

func TestConfig_ComfyUIVramFreeFraction_OutOfRange(t *testing.T) {
	for _, v := range []string{"-0.1", "1.5"} {
		content := plainModel + "comfyui:\n  vramFreeFraction: " + v + "\n"
		_, err := LoadConfigFromReader(strings.NewReader(content))
		require.Error(t, err, "vramFreeFraction %s should be rejected", v)
		assert.Contains(t, err.Error(), "comfyui.vramFreeFraction")
	}
}

func TestConfig_ComfyUI_NilCleanupModeSafe(t *testing.T) {
	var c *ComfyUIConfig
	assert.False(t, c.CleanupMode())
	assert.Equal(t, DefaultComfyUIVramFreeFraction, c.IdleVramFreeFraction())
}

func TestConfig_ComfyUIQueueTimeout_Default(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  swapMode: cleanup\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.Equal(t, time.Duration(0), cfg.ComfyUI.QueueWait())
}

func TestConfig_ComfyUIQueueTimeout_Set(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  queueTimeout: 300\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.Equal(t, 300*time.Second, cfg.ComfyUI.QueueWait())
}

func TestConfig_ComfyUIQueueTimeout_Negative(t *testing.T) {
	_, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  queueTimeout: -1\n"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "comfyui.queueTimeout")
}

func TestConfig_ComfyUIStartEvicts_Default(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  swapMode: cleanup\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.False(t, cfg.ComfyUI.StartEvicts)
}

func TestConfig_ComfyUIStartEvicts_True(t *testing.T) {
	cfg, err := LoadConfigFromReader(strings.NewReader(plainModel + "comfyui:\n  startEvicts: true\n"))
	require.NoError(t, err)
	require.NotNil(t, cfg.ComfyUI)
	assert.True(t, cfg.ComfyUI.StartEvicts)
}
