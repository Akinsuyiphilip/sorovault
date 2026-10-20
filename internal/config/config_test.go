package config_test

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/soroworks/sorovault/internal/config"
)

const testDatabaseURL = "postgres://user:pass@localhost:5432/sorovault"

// setEnv applies a set of variables for one test, clearing any others the
// package reads so the ambient environment cannot influence the result.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()

	for _, key := range []string{
		"RPC_URL", "NETWORK_PASSPHRASE", "DATABASE_URL", "DATABASE_URL_FILE",
		"HTTP_ADDR", "LOG_LEVEL", "RPC_TIMEOUT", "REFRESH_INTERVAL",
		"MAX_CONCURRENT_REGISTRATIONS", "REFRESH_WEBHOOK_URL",
		"REFRESH_WEBHOOK_SECRET",
	} {
		if v, ok := vars[key]; ok {
			t.Setenv(key, v)
			continue
		}
		t.Setenv(key, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": testDatabaseURL})

	cfg, err := config.Load()
	require.NoError(t, err)

	assert.Equal(t, config.DefaultRPCURL, cfg.RPCURL)
	assert.Equal(t, config.DefaultNetworkPassphrase, cfg.NetworkPassphrase)
	assert.Equal(t, config.DefaultHTTPAddr, cfg.HTTPAddr)
	assert.Equal(t, testDatabaseURL, cfg.DatabaseURL)
	assert.Equal(t, slog.LevelInfo, cfg.LogLevel)
	assert.Equal(t, config.DefaultRPCTimeout, cfg.RPCTimeout)
	assert.Zero(t, cfg.RefreshInterval, "automatic refresh is opt-in")
	assert.Zero(t, cfg.MaxConcurrentRegistrations,
		"the concurrency limit is left to the registry unless it is configured")
}

func TestLoadOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL":       testDatabaseURL,
		"RPC_URL":            "https://rpc.example.test",
		"NETWORK_PASSPHRASE": "Public Global Stellar Network ; September 2015",
		"HTTP_ADDR":          "127.0.0.1:9999",
		"LOG_LEVEL":          "debug",
		"RPC_TIMEOUT":        "90s",
		"REFRESH_INTERVAL":   "6h",
	})

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, 6*time.Hour, cfg.RefreshInterval)

	assert.Equal(t, "https://rpc.example.test", cfg.RPCURL)
	assert.Equal(t, "Public Global Stellar Network ; September 2015", cfg.NetworkPassphrase)
	assert.Equal(t, "127.0.0.1:9999", cfg.HTTPAddr)
	assert.Equal(t, slog.LevelDebug, cfg.LogLevel)
	assert.Equal(t, 90*time.Second, cfg.RPCTimeout)
}

func TestMaxConcurrentRegistrations(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    int
		wantErr string
	}{
		{
			name: "unset leaves the registry's own default alone",
		},
		{
			name:  "a limit",
			value: "8",
			want:  8,
		},
		{
			name:  "the smallest limit",
			value: "1",
			want:  1,
		},
		{
			name:  "the largest limit",
			value: "1000",
			want:  1000,
		},
		{
			name:  "surrounding space is ignored",
			value: " 12 ",
			want:  12,
		},
		{
			name:    "zero is not a limit",
			value:   "0",
			wantErr: "must be between",
		},
		{
			name:    "a negative limit",
			value:   "-1",
			wantErr: "must be between",
		},
		{
			name:    "above the largest limit",
			value:   "1001",
			wantErr: "must be between",
		},
		{
			name:    "not a number",
			value:   "lots",
			wantErr: "MAX_CONCURRENT_REGISTRATIONS",
		},
		{
			name:    "not a whole number",
			value:   "2.5",
			wantErr: "MAX_CONCURRENT_REGISTRATIONS",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			vars := map[string]string{"DATABASE_URL": testDatabaseURL}
			if tt.value != "" {
				vars["MAX_CONCURRENT_REGISTRATIONS"] = tt.value
			}
			setEnv(t, vars)

			cfg, err := config.Load()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.MaxConcurrentRegistrations)
		})
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	setEnv(t, nil)

	_, err := config.Load()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "DATABASE_URL")
}

func TestRefreshWebhook(t *testing.T) {
	t.Run("unset", func(t *testing.T) {
		setEnv(t, map[string]string{"DATABASE_URL": testDatabaseURL})

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Empty(t, cfg.RefreshWebhookURL)
		assert.Empty(t, cfg.RefreshWebhookSecret)
	})

	t.Run("set", func(t *testing.T) {
		setEnv(t, map[string]string{
			"DATABASE_URL":           testDatabaseURL,
			"REFRESH_WEBHOOK_URL":    "https://hooks.example.test/sorovault",
			"REFRESH_WEBHOOK_SECRET": "s3cret",
		})

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, "https://hooks.example.test/sorovault", cfg.RefreshWebhookURL)
		assert.Equal(t, "s3cret", cfg.RefreshWebhookSecret)
	})

	t.Run("a URL that cannot be posted to is refused", func(t *testing.T) {
		for _, url := range []string{"not a url", "/relative", "ftp://example.test/hook"} {
			setEnv(t, map[string]string{
				"DATABASE_URL":        testDatabaseURL,
				"REFRESH_WEBHOOK_URL": url,
			})

			_, err := config.Load()
			require.Error(t, err, "url %q", url)
			assert.Contains(t, err.Error(), "REFRESH_WEBHOOK_URL")
		}
	})
}

func TestLogLevels(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"DEBUG", slog.LevelDebug},
		{"  Info  ", slog.LevelInfo},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			setEnv(t, map[string]string{"DATABASE_URL": testDatabaseURL, "LOG_LEVEL": tt.in})

			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tt.want, cfg.LogLevel)
		})
	}
}

func TestInvalidValues(t *testing.T) {
	tests := []struct {
		name    string
		vars    map[string]string
		wantErr string
	}{
		{
			name:    "unknown log level",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "LOG_LEVEL": "chatty"},
			wantErr: "unknown LOG_LEVEL",
		},
		{
			name:    "unparseable timeout",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "RPC_TIMEOUT": "soon"},
			wantErr: "RPC_TIMEOUT",
		},
		{
			name:    "non-positive timeout",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "RPC_TIMEOUT": "0s"},
			wantErr: "must be positive",
		},
		{
			name:    "negative timeout",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "RPC_TIMEOUT": "-5s"},
			wantErr: "must be positive",
		},
		{
			name:    "unparseable refresh interval",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "REFRESH_INTERVAL": "daily"},
			wantErr: "REFRESH_INTERVAL",
		},
		{
			name:    "refresh interval below minimum",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "REFRESH_INTERVAL": "5s"},
			wantErr: "at least",
		},
		{
			name:    "negative refresh interval",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "REFRESH_INTERVAL": "-1h"},
			wantErr: "at least",
		},
		{
			name:    "both database url sources set",
			vars:    map[string]string{"DATABASE_URL": testDatabaseURL, "DATABASE_URL_FILE": "/tmp/secret"},
			wantErr: "set either DATABASE_URL or DATABASE_URL_FILE",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.vars)

			_, err := config.Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestLoadDatabaseURLFromFile(t *testing.T) {
	t.Run("reads contents", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "dburl")
		require.NoError(t, os.WriteFile(f, []byte(testDatabaseURL), 0o600))
		setEnv(t, map[string]string{"DATABASE_URL_FILE": f})

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, testDatabaseURL, cfg.DatabaseURL)
	})

	t.Run("trims trailing newline", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "dburl")
		require.NoError(t, os.WriteFile(f, []byte(testDatabaseURL+"\n"), 0o600))
		setEnv(t, map[string]string{"DATABASE_URL_FILE": f})

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, testDatabaseURL, cfg.DatabaseURL)
	})

	t.Run("trims carriage return", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "dburl")
		require.NoError(t, os.WriteFile(f, []byte(testDatabaseURL+"\r\n"), 0o600))
		setEnv(t, map[string]string{"DATABASE_URL_FILE": f})

		cfg, err := config.Load()
		require.NoError(t, err)
		assert.Equal(t, testDatabaseURL, cfg.DatabaseURL)
	})

	t.Run("unreadable file names path", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "missing")
		setEnv(t, map[string]string{"DATABASE_URL_FILE": f})

		_, err := config.Load()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "DATABASE_URL_FILE")
		assert.NotContains(t, err.Error(), testDatabaseURL)
		assert.Contains(t, err.Error(), f)
	})
}
