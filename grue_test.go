package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -----------------------------------------------------------------------------
// parseRef
// -----------------------------------------------------------------------------

func TestParseRef(t *testing.T) {
	tests := []struct {
		in        string
		name      string
		tag       string
		hasTag    bool
		wantError bool
	}{
		{"nginx", "nginx", "", false, false},
		{"nginx:latest", "nginx", "latest", true, false},
		{"library/nginx:1.2", "library/nginx", "1.2", true, false},
		{"registry.example.com:5000/repo:tag", "registry.example.com:5000/repo", "tag", true, false},
		{"repo/sub:tag", "repo/sub", "tag", true, false},
		{"nginx@sha256:88390c1a1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1d1", "", "", false, true},
		{"!!bad!!", "", "", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			name, tag, hasTag, err := parseRef(tt.in)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error, got nil (name=%q tag=%q hasTag=%v)", name, tag, hasTag)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if name != tt.name || tag != tt.tag || hasTag != tt.hasTag {
				t.Fatalf("got name=%q tag=%q hasTag=%v; want name=%q tag=%q hasTag=%v",
					name, tag, hasTag, tt.name, tt.tag, tt.hasTag)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// dockerEndpoint
// -----------------------------------------------------------------------------

func TestDockerEndpoint(t *testing.T) {
	t.Run("unix", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///tmp/docker.sock")
		network, addr, base, err := dockerEndpoint()
		if err != nil || network != "unix" || addr != "/tmp/docker.sock" || base != "http://docker" {
			t.Fatalf("got (%q,%q,%q,%v)", network, addr, base, err)
		}
	})
	t.Run("tcp", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "tcp://1.2.3.4:2375")
		network, addr, base, err := dockerEndpoint()
		if err != nil || network != "tcp" || addr != "1.2.3.4:2375" || base != "http://1.2.3.4:2375" {
			t.Fatalf("got (%q,%q,%q,%v)", network, addr, base, err)
		}
	})
	t.Run("unsupported", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "fd://3")
		_, _, _, err := dockerEndpoint()
		if err == nil {
			t.Fatal("expected error for unsupported scheme")
		}
	})
	t.Run("empty_defaults_to_unix", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "")
		network, _, _, err := dockerEndpoint()
		if err != nil || network != "unix" {
			t.Fatalf("got network=%q err=%v", network, err)
		}
	})
}

// -----------------------------------------------------------------------------
// resolveConfig
// -----------------------------------------------------------------------------

func TestResolveConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRUE_CONFIG", filepath.Join(dir, "config.json"))

	// Seed a file config.
	if err := saveConfig(&Config{
		Endpoint: "file.example.com",
		Region:   "file-region",
		Bucket:   "file-bucket",
		Prefix:   "fileprefix",
		AccessKey: "file-ak",
		SecretKey: "file-sk",
	}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}

	t.Run("env_overrides_file", func(t *testing.T) {
		t.Setenv("GRUE_ENDPOINT", "env.example.com")
		t.Setenv("GRUE_BUCKET", "env-bucket")
		t.Setenv("GRUE_ACCESS_KEY", "env-ak")
		t.Setenv("GRUE_SECRET_KEY", "env-sk")
		cfg, err := resolveConfig()
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.Endpoint != "env.example.com" {
			t.Errorf("Endpoint=%q want env.example.com", cfg.Endpoint)
		}
		if cfg.Bucket != "env-bucket" {
			t.Errorf("Bucket=%q want env-bucket", cfg.Bucket)
		}
		if cfg.AccessKey != "env-ak" {
			t.Errorf("AccessKey=%q want env-ak", cfg.AccessKey)
		}
		if cfg.SecretKey != "env-sk" {
			t.Errorf("SecretKey=%q want env-sk", cfg.SecretKey)
		}
		// Region not overridden -> file value retained.
		if cfg.Region != "file-region" {
			t.Errorf("Region=%q want file-region", cfg.Region)
		}
	})

	t.Run("prefix_normalized_from_file", func(t *testing.T) {
		// File prefix "fileprefix" (no trailing slash) -> "fileprefix/".
		cfg, err := resolveConfig()
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.Prefix != "fileprefix/" {
			t.Errorf("Prefix=%q want fileprefix/", cfg.Prefix)
		}
	})

	t.Run("prefix_with_slash_unchanged", func(t *testing.T) {
		t.Setenv("GRUE_PREFIX", "keep/")
		cfg, err := resolveConfig()
		if err != nil {
			t.Fatalf("resolveConfig: %v", err)
		}
		if cfg.Prefix != "keep/" {
			t.Errorf("Prefix=%q want keep/", cfg.Prefix)
		}
	})

}

func TestResolveConfigEmptyPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRUE_CONFIG", filepath.Join(dir, "config.json"))
	t.Setenv("GRUE_PREFIX", "")
	if err := saveConfig(&Config{Bucket: "b"}); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	cfg, err := resolveConfig()
	if err != nil {
		t.Fatalf("resolveConfig: %v", err)
	}
	if cfg.Prefix != "" {
		t.Fatalf("Prefix=%q want empty", cfg.Prefix)
	}
}

// -----------------------------------------------------------------------------
// loadConfig / saveConfig round-trip
// -----------------------------------------------------------------------------

func TestLoadSaveConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRUE_CONFIG", filepath.Join(dir, "config.json"))

	in := &Config{
		Endpoint:  "s3.example.com",
		Region:    "us-east-1",
		AccessKey: "AKIA...",
		SecretKey: "shh",
		Bucket:    "my-bucket",
		Prefix:    "grue/",
	}
	if err := saveConfig(in); err != nil {
		t.Fatalf("saveConfig: %v", err)
	}
	out, err := loadConfig()
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if *out != *in {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", *out, *in)
	}
}

func TestLoadConfigMissingFileReturnsEmpty(t *testing.T) {
	t.Setenv("GRUE_CONFIG", filepath.Join(t.TempDir(), "nope.json"))
	cfg, err := loadConfig()
	if err != nil {
		t.Fatalf("expected nil error for missing file, got %v", err)
	}
	if cfg == nil || *cfg != (Config{}) {
		t.Fatalf("expected empty Config, got %+v", cfg)
	}
}

func TestLoadConfigMalformed(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("GRUE_CONFIG", p)
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected error for malformed config, got nil")
	}
}

// -----------------------------------------------------------------------------
// uncompressedSize
// -----------------------------------------------------------------------------

func TestUncompressedSize(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		ld := descriptor{Annotations: map[string]string{annUncompressedSize: "4096"}}
		n, err := uncompressedSize(ld)
		if err != nil || n != 4096 {
			t.Fatalf("got (%d, %v)", n, err)
		}
	})
	t.Run("missing_annotation", func(t *testing.T) {
		_, err := uncompressedSize(descriptor{})
		if err == nil {
			t.Fatal("expected error for missing annotation")
		}
	})
	t.Run("malformed", func(t *testing.T) {
		ld := descriptor{Annotations: map[string]string{annUncompressedSize: "not-a-number"}}
		_, err := uncompressedSize(ld)
		if err == nil {
			t.Fatal("expected error for malformed number")
		}
	})
}

// -----------------------------------------------------------------------------
// dockerRef
// -----------------------------------------------------------------------------

func TestDockerRef(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"nginx", "nginx"},
		{"nginx:latest", "nginx:latest"},
		{"library/nginx:latest", "library/nginx:latest"},
		{"a b", "a%20b"},
		{"a/b/c:d", "a/b/c:d"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := dockerRef(tt.in)
			if got != tt.want {
				t.Fatalf("dockerRef(%q)=%q want %q", tt.in, got, tt.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// readLine
// -----------------------------------------------------------------------------

func TestReadLine(t *testing.T) {
	t.Run("terminated", func(t *testing.T) {
		got, err := readLine(strings.NewReader("hello\nworld"))
		if err != nil || got != "hello" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
	t.Run("no_newline_at_eof", func(t *testing.T) {
		got, err := readLine(strings.NewReader("partial"))
		if err != nil || got != "partial" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
	t.Run("empty_input", func(t *testing.T) {
		_, err := readLine(strings.NewReader(""))
		if err == nil {
			t.Fatal("expected error for empty input")
		}
	})
	t.Run("only_newline", func(t *testing.T) {
		got, err := readLine(strings.NewReader("\nrest"))
		if err != nil || got != "" {
			t.Fatalf("got (%q, %v)", got, err)
		}
	})
}

// -----------------------------------------------------------------------------
// printLoadProgress
// -----------------------------------------------------------------------------

func TestPrintLoadProgress(t *testing.T) {
	t.Run("stream_and_eof", func(t *testing.T) {
		in := strings.NewReader(`{"stream":"Loaded image: foo\n"}` + "\n")
		if err := printLoadProgress(in); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	t.Run("error_message", func(t *testing.T) {
		in := strings.NewReader(`{"error":"something broke"}`)
		err := printLoadProgress(in)
		if err == nil || !strings.Contains(err.Error(), "something broke") {
			t.Fatalf("got err=%v", err)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if err := printLoadProgress(strings.NewReader("")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// -----------------------------------------------------------------------------
// digestOf
// -----------------------------------------------------------------------------

func TestDigestOf(t *testing.T) {
	empty := sha256.Sum256(nil)
	want := "sha256:" + hex.EncodeToString(empty[:])
	if got := digestOf(nil); got != want {
		t.Fatalf("digestOf(nil)=%q want %q", got, want)
	}
	if !strings.HasPrefix(want, "sha256:e3b0c442") {
		t.Fatalf("sanity: expected known empty sha256 vector, got %q", want)
	}
}

