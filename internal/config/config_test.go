package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDataDir(t *testing.T) {
	base := map[string]any{
		"readeck": map[string]any{"host": "https://readeck.example.com"},
		"users": []map[string]any{
			{
				"token":                "test-token",
				"readeck_access_token": "test-readeck-token",
			},
		},
	}
	write := func(t *testing.T, cfg map[string]any) string {
		t.Helper()
		dir := t.TempDir()
		path := dir + "/config.yaml"
		data, err := yaml.Marshal(cfg)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		return path
	}

	t.Run("default data_dir is data", func(t *testing.T) {
		cfg, err := Load(write(t, base))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Server.DataDir != "data" {
			t.Errorf("default data_dir = %q, want %q", cfg.Server.DataDir, "data")
		}
	})

	t.Run("explicit data_dir", func(t *testing.T) {
		cfgMap := map[string]any{
			"readeck": base["readeck"],
			"users":   base["users"],
			"server":  map[string]any{"data_dir": "/var/lib/readeckobo"},
		}
		cfg, err := Load(write(t, cfgMap))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Server.DataDir != "/var/lib/readeckobo" {
			t.Errorf("data_dir = %q, want /var/lib/readeckobo", cfg.Server.DataDir)
		}
	})
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name        string
		config      map[string]any
		yamlContent string
		noFile      bool
		wantErr     bool
	}{
		{
			name: "valid config",
			config: map[string]any{
				"readeck": map[string]any{
					"host": "https://readeck.example.com",
				},
				"server": map[string]any{
					"port": 8080,
				},
				"users": []map[string]any{
					{
						"token":                "test-token",
						"readeck_access_token": "test-readeck-token",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid config missing readeck.host",
			config: map[string]any{
				"users": []map[string]any{
					{
						"token":                "test-token",
						"readeck_access_token": "test-readeck-token",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "invalid config missing users",
			config: map[string]any{
				"readeck": map[string]any{
					"host": "https://readeck.example.com",
				},
			},
			wantErr: true,
		},
		{
			name: "invalid server.port too high",
			config: map[string]any{
				"readeck": map[string]any{
					"host": "https://readeck.example.com",
				},
				"server": map[string]any{
					"port": 65536,
				},
				"users": []map[string]any{
					{
						"token":                "test-token",
						"readeck_access_token": "test-readeck-token",
					},
				},
			},
			wantErr: true,
		},
		{
			name: "valid server.port",
			config: map[string]any{
				"readeck": map[string]any{
					"host": "https://readeck.example.com",
				},
				"server": map[string]any{
					"port": 8080,
				},
				"users": []map[string]any{
					{
						"token":                "test-token",
						"readeck_access_token": "test-readeck-token",
					},
				},
			},
			wantErr: false,
		},
		{
			name: "invalid readeck.host format",
			config: map[string]any{
				"readeck": map[string]any{
					"host": "invalid-url",
				},
				"users": []map[string]any{
					{
						"token":                "test-token",
						"readeck_access_token": "test-readeck-token",
					},
				},
			},
			wantErr: true,
		},
		{
			name:    "file does not exist",
			noFile:  true,
			wantErr: true,
		},
		{
			name:        "invalid yaml syntax",
			yamlContent: "readeck: host: [unbalanced brackets",
			wantErr:     true,
		},
		{
			name:        "invalid yaml structure for unmarshal",
			yamlContent: "readeck: \"should be a map but is a string\"",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "config-test")
			if err != nil {
				t.Fatalf("Failed to create temp dir: %v", err)
			}
			defer func() {
				if err := os.RemoveAll(tmpDir); err != nil {
					t.Errorf("Failed to remove temp dir: %v", err)
				}
			}()

			configPath := filepath.Join(tmpDir, "config.yaml")

			if !tt.noFile {
				var data []byte
				if tt.yamlContent != "" {
					data = []byte(tt.yamlContent)
				} else {
					data, err = yaml.Marshal(tt.config)
					if err != nil {
						t.Fatalf("Failed to marshal test config: %v", err)
					}
				}

				if err := os.WriteFile(configPath, data, 0644); err != nil {
					t.Fatalf("Failed to write dummy config file: %v", err)
				}
			}

			_, err = Load(configPath)

			if (err != nil) != tt.wantErr {
				t.Errorf("Load() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
		})
	}
}
