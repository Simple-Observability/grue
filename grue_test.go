package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
		Endpoint:  "file.example.com",
		Region:    "file-region",
		Bucket:    "file-bucket",
		Prefix:    "fileprefix",
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

// -----------------------------------------------------------------------------
// prune
// -----------------------------------------------------------------------------

// Reference Discovery

func testManifest(config string, layers ...string) []byte {
	m := imageManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		Config:        descriptor{Digest: config},
	}
	for _, layer := range layers {
		m.Layers = append(m.Layers, descriptor{Digest: layer})
	}
	data, _ := json.Marshal(m)
	return data
}

type fakeTagSource struct {
	keys    []string
	data    map[string][]byte
	listErr error
	readErr map[string]error
}

type fakeBlobSource struct {
	blobs []blobInfo
	err   error
}

func (f *fakeTagSource) listTagKeys(ctx context.Context) ([]string, error) {
	return f.keys, f.listErr
}

func (f *fakeTagSource) readTag(ctx context.Context, key string) ([]byte, error) {
	if err := f.readErr[key]; err != nil {
		return nil, err
	}
	data, ok := f.data[key]
	if !ok {
		return nil, errors.New("object not found")
	}
	return data, nil
}

func (f *fakeBlobSource) listBlobs(ctx context.Context) ([]blobInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.blobs, nil
}

func TestDiscoverReferences(t *testing.T) {
	config := "sha256:" + strings.Repeat("a", 64)
	layer := "sha256:" + strings.Repeat("b", 64)
	shared := "sha256:" + strings.Repeat("c", 64)

	tests := []struct {
		name    string
		source  *fakeTagSource
		want    map[string][]string
		wantErr bool
	}{
		{
			name: "multiple repositories and tags",
			source: &fakeTagSource{
				keys: []string{
					"repos/one/tags/latest",
					"repos/one/tags/v1",
					"repos/two/tags/latest",
				},
				data: map[string][]byte{
					"repos/one/tags/latest": testManifest(config, layer),
					"repos/one/tags/v1":     testManifest(config),
					"repos/two/tags/latest": testManifest(shared),
				},
			},
			want: map[string][]string{
				config: {"one:latest", "one:v1"},
				layer:  {"one:latest"},
				shared: {"two:latest"},
			},
		},
		{
			name:   "empty storage",
			source: &fakeTagSource{data: map[string][]byte{}},
			want:   map[string][]string{},
		},
		{
			name: "malformed JSON",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				data: map[string][]byte{
					"repos/one/tags/latest": []byte("{"),
				},
			},
			wantErr: true,
		},
		{
			name: "unsupported schema version",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				data: map[string][]byte{
					"repos/one/tags/latest": []byte(`{"schemaVersion":1,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`),
				},
			},
			wantErr: true,
		},
		{
			name: "unsupported media type",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				data: map[string][]byte{
					"repos/one/tags/latest": []byte(`{"schemaVersion":2,"mediaType":"unsupported"}`),
				},
			},
			wantErr: true,
		},
		{
			name: "missing config digest",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				data: map[string][]byte{
					"repos/one/tags/latest": []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{},"layers":[]}`),
				},
			},
			wantErr: true,
		},
		{
			name: "invalid config digest",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				data: map[string][]byte{
					"repos/one/tags/latest": testManifest("invalid"),
				},
			},
			wantErr: true,
		},
		{
			name: "invalid layer digest",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				data: map[string][]byte{
					"repos/one/tags/latest": testManifest(config, "invalid"),
				},
			},
			wantErr: true,
		},
		{
			name: "listing failure",
			source: &fakeTagSource{
				listErr: errors.New("listing failed"),
			},
			wantErr: true,
		},
		{
			name: "read failure",
			source: &fakeTagSource{
				keys: []string{"repos/one/tags/latest"},
				readErr: map[string]error{
					"repos/one/tags/latest": errors.New("read failed"),
				},
			},
			wantErr: true,
		},
		{
			name: "invalid tag object key",
			source: &fakeTagSource{
				keys: []string{"repos/one/metadata"},
				data: map[string][]byte{},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := discoverReferences(context.Background(), tt.source, "")

			if (err != nil) != tt.wantErr {
				t.Fatalf("discoverReferences() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil {
				if got != nil {
					t.Fatalf("discoverReferences() returned partial references: %v", got)
				}
				return
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("discoverReferences() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Blob validation

func TestValidateBlobs(t *testing.T) {
	config := "sha256:" + strings.Repeat("a", 64)
	layer := "sha256:" + strings.Repeat("b", 64)
	orphan := "sha256:" + strings.Repeat("c", 64)
	missing := "sha256:" + strings.Repeat("d", 64)

	prefix := "test/"
	configKey := prefix + "blobs/" + config
	layerKey := prefix + "blobs/" + layer
	orphanKey := prefix + "blobs/" + orphan

	tests := []struct {
		name        string
		refs        map[string][]string
		blobs       []blobInfo
		want        []blobInfo
		wantMissing map[string][]string
		wantErr     bool
	}{
		{
			name: "referenced and unreferenced blobs",
			refs: map[string][]string{
				config: {"one:latest"},
				layer:  {"one:latest"},
			},
			blobs: []blobInfo{
				{key: configKey, size: 100},
				{key: layerKey, size: 200},
				{key: orphanKey, size: 300},
			},
			want: []blobInfo{
				{key: orphanKey, size: 300},
			},
			wantMissing: map[string][]string{},
		},
		{
			name: "missing blob referenced by multiple tags",
			refs: map[string][]string{
				missing: {"one:latest", "two:v1"},
			},
			blobs: []blobInfo{
				{key: orphanKey, size: 300},
			},
			want: []blobInfo{
				{key: orphanKey, size: 300},
			},
			wantMissing: map[string][]string{
				missing: {"one:latest", "two:v1"},
			},
		},
		{
			name: "missing config and layer blobs",
			refs: map[string][]string{
				config: {"one:latest"},
				layer:  {"one:latest"},
			},
			blobs: []blobInfo{},
			want:  []blobInfo{},
			wantMissing: map[string][]string{
				config: {"one:latest"},
				layer:  {"one:latest"},
			},
		},
		{
			name: "empty references",
			refs: map[string][]string{},
			blobs: []blobInfo{
				{key: orphanKey, size: 300},
			},
			want: []blobInfo{
				{key: orphanKey, size: 300},
			},
			wantMissing: map[string][]string{},
		},
		{
			name:        "empty references and blobs",
			refs:        map[string][]string{},
			blobs:       []blobInfo{},
			want:        []blobInfo{},
			wantMissing: map[string][]string{},
		},
		{
			name: "duplicate references",
			refs: map[string][]string{
				config: {"one:latest", "one:v1", "two:latest"},
			},
			blobs: []blobInfo{
				{key: configKey, size: 100},
			},
			want:        []blobInfo{},
			wantMissing: map[string][]string{},
		},
		{
			name: "unexpected blob key",
			refs: map[string][]string{},
			blobs: []blobInfo{
				{key: "test/other/object", size: 100},
			},
			want:        []blobInfo{},
			wantMissing: map[string][]string{},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateBlobs(tt.refs, tt.blobs, prefix)
			if tt.wantErr {
				if err == nil {
					t.Fatal("validateBlobs() error = nil, want error")
				}
			} else {
				if err != nil {
					t.Fatalf("validateBlobs() error = %v", err)
				}

				if !slices.Equal(got.candidates, tt.want) {
					t.Errorf("candidates = %v, want %v", got.candidates, tt.want)
				}
				if !reflect.DeepEqual(got.missing, tt.wantMissing) {
					t.Errorf("missing = %v, want %v", got.missing, tt.wantMissing)
				}
			}
		})
	}
}

// validation run and results

func TestFormatBlobValidation(t *testing.T) {
	const prefix = "test/"
	const (
		digest1 = "sha256:111"
		digest2 = "sha256:222"
	)

	tests := []struct {
		name   string
		result blobValidation
		want   string
	}{
		{
			name: "candidates and total size",
			result: blobValidation{
				candidates: []blobInfo{
					{key: prefix + "blobs/" + digest1, size: 1024},
					{key: prefix + "blobs/" + digest2, size: 2048},
				},
				missing: map[string][]string{},
			},
			want: "Unreferenced blobs:\n" +
				"  " + digest1 + "  1024 bytes\n" +
				"  " + digest2 + "  2048 bytes\n" +
				"\n2 blobs, 3072 bytes reclaimable\n",
		},
		{
			name: "missing references",
			result: blobValidation{
				missing: map[string][]string{
					digest1: {"repo:latest", "repo:v1"},
				},
			},
			want: "Unreferenced blobs:\n" +
				"\n0 blobs, 0 bytes reclaimable\n" +
				"\nMissing referenced blobs:\n" +
				"  " + digest1 + "  referenced by repo:latest, repo:v1\n",
		},
		{
			name: "empty result",
			result: blobValidation{
				missing: map[string][]string{},
			},
			want: "Unreferenced blobs:\n" +
				"\n0 blobs, 0 bytes reclaimable\n",
		},
		{
			name: "candidates and missing references",
			result: blobValidation{
				candidates: []blobInfo{
					{key: prefix + "blobs/" + digest1, size: 512},
				},
				missing: map[string][]string{
					digest2: {"repo:latest"},
				},
			},
			want: "Unreferenced blobs:\n" +
				"  " + digest1 + "  512 bytes\n" +
				"\n1 blobs, 512 bytes reclaimable\n" +
				"\nMissing referenced blobs:\n" +
				"  " + digest2 + "  referenced by repo:latest\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			formatBlobValidation(&buf, tt.result, prefix)

			if got := buf.String(); got != tt.want {
				t.Errorf("formatBlobValidation() output:\n%s\nwant:\n%s", got, tt.want)
			}
		})
	}
}

func TestInspectBlobs(t *testing.T) {
	const prefix = "test/"

	config := "sha256:" + strings.Repeat("1", 64)
	layer := "sha256:" + strings.Repeat("2", 64)
	orphan := "sha256:" + strings.Repeat("3", 64)
	tagKey := "test/repos/one/tags/latest"

	tests := []struct {
		name    string
		tags    *fakeTagSource
		blobs   *fakeBlobSource
		want    blobValidation
		wantErr bool
	}{
		{
			name: "successful inspection",
			tags: &fakeTagSource{
				keys: []string{tagKey},
				data: map[string][]byte{
					tagKey: testManifest(config, layer),
				},
			},
			blobs: &fakeBlobSource{
				blobs: []blobInfo{
					{key: prefix + "blobs/" + config, size: 100},
					{key: prefix + "blobs/" + layer, size: 200},
					{key: prefix + "blobs/" + orphan, size: 300},
				},
			},
			want: blobValidation{
				candidates: []blobInfo{
					{key: prefix + "blobs/" + orphan, size: 300},
				},
				missing: map[string][]string{},
			},
		},
		{
			name: "empty storage",
			tags: &fakeTagSource{
				data: map[string][]byte{},
			},
			blobs: &fakeBlobSource{
				blobs: []blobInfo{},
			},
			want: blobValidation{
				missing: map[string][]string{},
			},
		},
		{
			name: "blob listing failure",
			tags: &fakeTagSource{
				data: map[string][]byte{},
			},
			blobs: &fakeBlobSource{
				err: errors.New("listing failed"),
			},
			wantErr: true,
		},
		{
			name: "tag discovery failure",
			tags: &fakeTagSource{
				listErr: errors.New("tag listing failed"),
			},
			blobs:   &fakeBlobSource{},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := inspectBlobs(context.Background(), tt.tags, tt.blobs, prefix)

			if tt.wantErr {
				if err == nil {
					t.Fatal("inspectBlobs() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("inspectBlobs() error = %v", err)
			}

			if !slices.Equal(got.candidates, tt.want.candidates) {
				t.Errorf("candidates = %v, want %v", got.candidates, tt.want.candidates)
			}
			if !reflect.DeepEqual(got.missing, tt.want.missing) {
				t.Errorf("missing = %v, want %v", got.missing, tt.want.missing)
			}
		})
	}
}

func TestSameCandidates(t *testing.T) {
	tests := []struct {
		name string
		a    []blobInfo
		b    []blobInfo
		want bool
	}{
		{
			name: "identical",
			a: []blobInfo{
				{key: "blobs/a", size: 100},
				{key: "blobs/b", size: 200},
			},
			b: []blobInfo{
				{key: "blobs/a", size: 100},
				{key: "blobs/b", size: 200},
			},
			want: true,
		},
		{
			name: "different order",
			a: []blobInfo{
				{key: "blobs/a", size: 100},
				{key: "blobs/b", size: 200},
			},
			b: []blobInfo{
				{key: "blobs/b", size: 200},
				{key: "blobs/a", size: 100},
			},
			want: true,
		},
		{
			name: "candidate added",
			a:    []blobInfo{{key: "blobs/a", size: 100}},
			b: []blobInfo{
				{key: "blobs/a", size: 100},
				{key: "blobs/b", size: 200},
			},
			want: false,
		},
		{
			name: "candidate removed",
			a: []blobInfo{
				{key: "blobs/a", size: 100},
				{key: "blobs/b", size: 200},
			},
			b:    []blobInfo{{key: "blobs/a", size: 100}},
			want: false,
		},
		{
			name: "size changed",
			a:    []blobInfo{{key: "blobs/a", size: 100}},
			b:    []blobInfo{{key: "blobs/a", size: 101}},
			want: false,
		},
		{
			name: "empty sets",
			a:    []blobInfo{},
			b:    []blobInfo{},
			want: true,
		},
		{
			name: "duplicate key",
			a: []blobInfo{
				{key: "blobs/a", size: 100},
				{key: "blobs/a", size: 100},
			},
			b:    []blobInfo{{key: "blobs/a", size: 100}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sameCandidates(tt.a, tt.b); got != tt.want {
				t.Errorf("sameCandidates() = %v, want %v", got, tt.want)
			}
		})
	}
}
