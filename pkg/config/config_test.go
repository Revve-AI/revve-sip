package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigAdmissionDefaults(t *testing.T) {
	c, err := NewConfig("redis:\n  address: localhost:6379\nadmission:\n  url: http://x\n")
	require.NoError(t, err)
	require.NoError(t, c.Init())
	require.NotNil(t, c.Admission)
	require.Equal(t, time.Second, c.Admission.DefaultTimeout)
	require.Equal(t, 503, c.Admission.DefaultFallbackCode)
}

func TestConfigAdmissionAbsent(t *testing.T) {
	c, err := NewConfig("redis:\n  address: localhost:6379\n")
	require.NoError(t, err)
	require.NoError(t, c.Init())
	require.Nil(t, c.Admission)
}
