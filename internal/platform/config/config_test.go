package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadUsesMoraAPIURL(t *testing.T) {
	t.Setenv("MORA_API_URL", "http://mora-api:8080/")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "http://mora-api:8080", cfg.MoraAPIURL)
}

func TestFromEnvUsesMoraAPIURL(t *testing.T) {
	t.Setenv("MORA_API_URL", "http://mora-api:8080/")

	cfg := FromEnv()
	assert.Equal(t, "http://mora-api:8080", cfg.MoraAPIURL)
}
