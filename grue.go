package main

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"golang.org/x/term"
)


// -----------------------------------------------------------------------------
// Main
// -----------------------------------------------------------------------------

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "connect":
		cmdConnect()
	case "push":
		cmdPush()
	case "pull":
		cmdPull()
	case "ls":
		cmdList()
	case "rm":
		cmdRemove()
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "grue: unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`grue - your bucket is your registry

Usage:
  grue connect              configure an S3-compatible storage backend
  grue push   <name[:tag]>  push a local image to the bucket
  grue pull   <name[:tag]>  pull an image from the bucket into Docker
  grue ls                   list repositories and tags
  grue rm     <name[:tag]>   delete a tag (or all tags of a repo)

Environment overrides (take precedence over the config file):
  GRUE_ENDPOINT, GRUE_REGION, GRUE_ACCESS_KEY, GRUE_SECRET_KEY, GRUE_BUCKET, GRUE_PREFIX
`)
}

// -----------------------------------------------------------------------------
// Configuration
// -----------------------------------------------------------------------------

// Config is the on-disk configuration written by `grue connect`.
type Config struct {
	Endpoint  string `json:"endpoint"`
	Region    string `json:"region"`
	AccessKey string `json:"access_key"`
	SecretKey string `json:"secret_key"`
	Bucket    string `json:"bucket"`
	Prefix    string `json:"prefix"`
}

// configPath returns the resolved config file path.
func configPath() string {
	if p := os.Getenv("GRUE_CONFIG"); p != "" {
		return p
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fatal("cannot determine home directory: %v", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "grue", "config.json")
}

// loadConfig reads the on-disk config; returns an empty Config if absent.
func loadConfig() (*Config, error) {
	p := configPath()
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &c, nil
}

func saveConfig(c *Config) error {
	p := configPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(p, data, 0o600)
}

// resolveConfig applies environment overrides on top of the file, in the
// priority order GRUE_* > file. It also normalizes the key prefix to end
// with "/".
func resolveConfig() (*Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	pick := func(cur string, envs ...string) string {
		for _, e := range envs {
			if v := os.Getenv(e); v != "" {
				return v
			}
		}
		return cur
	}
	cfg.Endpoint = pick(cfg.Endpoint, "GRUE_ENDPOINT")
	cfg.Region = pick(cfg.Region, "GRUE_REGION")
	cfg.Bucket = pick(cfg.Bucket, "GRUE_BUCKET")
	cfg.Prefix = pick(cfg.Prefix, "GRUE_PREFIX")
	cfg.AccessKey = pick(cfg.AccessKey, "GRUE_ACCESS_KEY")
	cfg.SecretKey = pick(cfg.SecretKey, "GRUE_SECRET_KEY")
	if cfg.Prefix != "" && !strings.HasSuffix(cfg.Prefix, "/") {
		cfg.Prefix += "/"
	}
	return cfg, nil
}

// -----------------------------------------------------------------------------
// S3
// -----------------------------------------------------------------------------

type store struct {
	client *minio.Client
	bucket string
	prefix string
}

// newStore validates required fields and builds a minio client with static
// credentials from the given config. No AWS credential chain fallback.
func newStore(cfg *Config) (*store, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("no backend configured - run `grue connect` or set GRUE_BUCKET")
	}

	endpoint := cfg.Endpoint
	secure := true
	if strings.HasPrefix(endpoint, "https://") {
		endpoint = strings.TrimPrefix(endpoint, "https://")
	} else if strings.HasPrefix(endpoint, "http://") {
		endpoint = strings.TrimPrefix(endpoint, "http://")
		secure = false
	}
	client, err := minio.New(endpoint, &minio.Options{
		Secure: secure,
		Region: cfg.Region,
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
	})
	if err != nil {
		return nil, err
	}
	return &store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

func (s *store) blobKey(digest string) string   { return s.prefix + "blobs/" + digest }

func (s *store) tagKey(name, tag string) string { return s.prefix + "repos/" + name + "/tags/" + tag }

// putBlobIfAbsent uploads a blob only if it is not already present.
// Returns true when it uploaded, false when it dedup-skipped.
func (s *store) putBlobIfAbsent(ctx context.Context, digest string, r io.Reader, size int64, contentType string) (bool, error) {
	key := s.blobKey(digest)
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return false, nil
	}
	o := minio.PutObjectOptions{}
	if contentType != "" {
		o.ContentType = contentType
	}
	if _, err := s.client.PutObject(ctx, s.bucket, key, r, size, o); err != nil {
		return false, err
	}
	return true, nil
}

func (s *store) getBlob(ctx context.Context, digest string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.blobKey(digest), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	return obj, nil
}

func (s *store) putTag(ctx context.Context, name, tag string, data []byte) error {
	r := bytes.NewReader(data)
	_, err := s.client.PutObject(ctx, s.bucket, s.tagKey(name, tag), r, int64(len(data)),
		minio.PutObjectOptions{ContentType: mediaTypeManifest})
	return err
}

func (s *store) getTag(ctx context.Context, name, tag string) ([]byte, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.tagKey(name, tag), minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer obj.Close()
	return io.ReadAll(obj)
}

func (s *store) deleteTag(ctx context.Context, name, tag string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.tagKey(name, tag), minio.RemoveObjectOptions{})
}

// -----------------------------------------------------------------------------
// Docker
// -----------------------------------------------------------------------------

const (
	// mediaType constants from the OCI image format specification.
	mediaTypeManifest = "application/vnd.oci.image.manifest.v1+json"
	mediaTypeConfig   = "application/vnd.oci.image.config.v1+json"
	mediaTypeLayer    = "application/vnd.oci.image.layer.v1.tar+gzip"

	// annUncompressedSize carries the uncompressed size of a gzip layer blob.
	// We stash it in a vendor annotation so the manifest stays a valid OCI manifest.
	annUncompressedSize = "io.grue.uncompressed.size"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type imageManifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	MediaType     string       `json:"mediaType"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

// saveManifestItem is the single entry of a `docker save` tar's manifest.json.
type saveManifestItem struct {
	Config   string   `json:"Config"`
	RepoTags []string `json:"RepoTags"`
	Layers   []string `json:"Layers"`
}

type dockerClient struct {
	http *http.Client
	base string
}

func newDockerClient() (*dockerClient, error) {
	network, addr, base, err := dockerEndpoint()
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	return &dockerClient{http: &http.Client{Transport: tr}, base: base}, nil
}

func dockerEndpoint() (network, addr, base string, err error) {
	h := os.Getenv("DOCKER_HOST")
	if h == "" {
		if runtime.GOOS == "darwin" {
			home, e := os.UserHomeDir()
			if e != nil {
				return "", "", "", e
			}
			return "unix", filepath.Join(home, ".docker", "run", "docker.sock"), "http://docker", nil
		}
		return "unix", defaultDockerSocket(), "http://docker", nil
	}
	switch {
	case strings.HasPrefix(h, "unix://"):
		return "unix", strings.TrimPrefix(h, "unix://"), "http://docker", nil
	case strings.HasPrefix(h, "tcp://"):
		addr = strings.TrimPrefix(h, "tcp://")
		return "tcp", addr, "http://" + addr, nil
	default:
		return "", "", "", fmt.Errorf("unsupported DOCKER_HOST %q", h)
	}
}

func (d *dockerClient) request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, body)
	if err != nil {
		return nil, err
	}
	return d.http.Do(req)
}

// defaultDockerSocket returns the default Docker daemon socket on Linux,
// falling back to the rootless Docker socket ($XDG_RUNTIME_DIR/docker.sock)
// when the system socket is absent.
func defaultDockerSocket() string {
	const sys = "/var/run/docker.sock"
	if _, err := os.Stat(sys); err == nil {
		return sys
	}
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "docker.sock")
	}
	return sys
}

// dockerRef URL-encodes an image reference for use in a Docker API path,
// keeping "/" literal.
func dockerRef(ref string) string {
	return strings.ReplaceAll(url.PathEscape(ref), "%2F", "/")
}

// -----------------------------------------------------------------------------
// connect
// -----------------------------------------------------------------------------

func cmdConnect() {
	cfg, _ := loadConfig()
	if cfg == nil {
		cfg = &Config{}
	}
	cfg.Endpoint = prompt("Endpoint (e.g. s3.amazonaws.com, or <account>.r2.cloudflarestorage.com)", cfg.Endpoint)
	cfg.Region = prompt("Region (e.g. us-east-1; blank if unused)", cfg.Region)
	cfg.AccessKey = prompt("Access key", cfg.AccessKey)
	cfg.SecretKey = promptSecret("Secret key")
	cfg.Bucket = prompt("Bucket", cfg.Bucket)
	cfg.Prefix = prompt("Key prefix", defaultStr(cfg.Prefix, "grue/"))

	if cfg.AccessKey == "" || cfg.SecretKey == "" || cfg.Bucket == "" {
		fatal("access key, secret key and bucket are required")
	}
	if !strings.HasSuffix(cfg.Prefix, "/") && cfg.Prefix != "" {
		cfg.Prefix += "/"
	}

	if err := saveConfig(cfg); err != nil {
		fatal("cannot save config: %v", err)
	}

	fmt.Printf("\nSaved credentials in %s.\n", configPath())
	fmt.Println("Note: the file stores credentials in plaintext.")
	fmt.Println("For servers/CI, prefer the GRUE_* environment variables instead.")

	st, err := newStore(cfg)
	if err != nil {
		fatal("%v", err)
	}
	ctx := context.Background()
	if exists, err := st.client.BucketExists(ctx, cfg.Bucket); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not reach bucket %q: %v\n", cfg.Bucket, err)
		fmt.Fprintln(os.Stderr, "Config was still saved; fix the values and re-run `grue connect`.")
		os.Exit(1)
	} else if !exists {
		fmt.Fprintf(os.Stderr, "Warning: bucket %q does not exist.\n", cfg.Bucket)
		fmt.Fprintln(os.Stderr, "Config was still saved; create the bucket and re-run `grue connect`.")
		os.Exit(1)
	}
	fmt.Printf("OK - connected to bucket %q.\n", cfg.Bucket)
}

// readLine reads a single line from r up to and including the newline, but
// never reads past it, so a subsequent raw-fd read (term.ReadPassword) does
// not miss bytes that a bufio.Reader would have buffered ahead. Used for all
// interactive prompts to keep the secret prompt's term.ReadPassword path safe.
func readLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				return sb.String(), nil
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if sb.Len() > 0 {
				return sb.String(), nil
			}
			return "", err
		}
	}
}

func prompt(label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, err := readLine(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr)
		fatal("input error: %v", err)
	}
	if v := strings.TrimSpace(line); v != "" {
		return v
	}
	return def
}

func promptSecret(label string) string {
	fmt.Printf("%s: ", label)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Println()
		if err != nil {
			fatal("input error: %v", err)
		}
		return strings.TrimSpace(string(b))
	}
	line, err := readLine(os.Stdin)
	if err != nil {
		fmt.Fprintln(os.Stderr)
		fatal("input error: %v", err)
	}
	return strings.TrimSpace(line)
}

func confirm(prompt string) bool {
	fmt.Printf("%s [y/N]: ", prompt)
	line, err := readLine(os.Stdin)
	if err != nil {
		return false
	}
	line = strings.TrimSpace(strings.ToLower(line))
	return line == "y" || line == "yes"
}

// -----------------------------------------------------------------------------
// push
// -----------------------------------------------------------------------------

func cmdPush() {
	if len(os.Args) < 3 {
		fatal("usage: grue push <name[:tag]>")
	}
	name, tag, hasTag, err := parseRef(os.Args[2])
	if err != nil {
		fatal("%v", err)
	}
	if !hasTag {
		tag = "latest"
	}
	ref := name + ":" + tag

	ctx := context.Background()
	cfg, err := resolveConfig()
	if err != nil {
		fatal("%v", err)
	}
	st, err := newStore(cfg)
	if err != nil {
		fatal("%v", err)
	}
	dk, err := newDockerClient()
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("Pushing %s\n", ref)
	resp, err := dk.request(ctx, http.MethodGet, "/images/"+dockerRef(ref)+"/get", nil)
	if err != nil {
		fatal("docker: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fatal("docker save %s: %s", ref, resp.Status)
	}

	// Save the tar to a temp file so we can walk it twice: once for
	// manifest.json, once for config and layers.
	saveFile, err := os.CreateTemp("", "grue-save-*.tar")
	if err != nil {
		fatal("creating temp file: %v", err)
	}
	defer os.Remove(saveFile.Name())
	defer saveFile.Close()
	if _, err := io.Copy(saveFile, resp.Body); err != nil {
		fatal("reading save tar: %v", err)
	}
	if _, err := saveFile.Seek(0, io.SeekStart); err != nil {
		fatal("seeking save tar: %v", err)
	}

	// First pass: read manifest.json.
	var manifestJSON []byte
	tr := tar.NewReader(saveFile)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fatal("reading save tar: %v", err)
		}
		if hdr.Name == "manifest.json" {
			manifestJSON, err = io.ReadAll(tr)
			if err != nil {
				fatal("reading manifest.json: %v", err)
			}
			break
		}
	}

	if len(manifestJSON) == 0 {
		fatal("save tar contained no manifest.json (unsupported docker save format)")
	}
	var items []saveManifestItem
	if err := json.Unmarshal(manifestJSON, &items); err != nil {
		fatal("parsing manifest.json: %v", err)
	}
	if len(items) == 0 {
		fatal("manifest.json had no images")
	}
	item := items[0]

	// Build a set of layer paths so we can match tar entries by name.
	layerPaths := make(map[string]bool, len(item.Layers))
	for _, p := range item.Layers {
		layerPaths[p] = true
	}

	// Second pass: read config and process layers.
	if _, err := saveFile.Seek(0, io.SeekStart); err != nil {
		fatal("seeking save tar: %v", err)
	}
	tr = tar.NewReader(saveFile)

	var configBytes []byte
	layers := map[string]layerRec{} // tar path -> descriptor info

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			fatal("reading save tar: %v", err)
		}
		switch {
		case hdr.Name == "manifest.json":
			// already read in the first pass
		case hdr.Name == item.Config:
			configBytes, err = io.ReadAll(tr)
			if err != nil {
				fatal("reading %s: %v", hdr.Name, err)
			}
		case layerPaths[hdr.Name]:
			rec, err := processLayer(ctx, st, tr)
			if err != nil {
				fatal("layer %s: %v", hdr.Name, err)
			}
			layers[hdr.Name] = rec
		default:
			// ignore: repositories, VERSION, index.json, oci-layout, etc.
		}
	}

	if len(configBytes) == 0 {
		fatal("config file %q referenced by manifest.json not found in tar", item.Config)
	}
	configDigest := digestOf(configBytes)
	uploaded, err := st.putBlobIfAbsent(ctx, configDigest, bytes.NewReader(configBytes),
		int64(len(configBytes)), mediaTypeConfig)
	if err != nil {
		fatal("uploading config: %v", err)
	}
	fmt.Printf("  config %s (%s)\n", configDigest, statusWord(uploaded))

	manifest := imageManifest{
		SchemaVersion: 2,
		MediaType:     mediaTypeManifest,
		Config: descriptor{
			MediaType: mediaTypeConfig,
			Digest:    configDigest,
			Size:      int64(len(configBytes)),
		},
	}
	for _, p := range item.Layers {
		rec, ok := layers[p]
		if !ok {
			fatal("layer %q referenced by manifest.json not found in tar", p)
		}
		manifest.Layers = append(manifest.Layers, descriptor{
			MediaType:   mediaTypeLayer,
			Digest:      rec.digest,
			Size:        rec.size,
			Annotations: map[string]string{annUncompressedSize: fmt.Sprintf("%d", rec.uncompressed)},
		})
	}

	mb, err := json.Marshal(manifest)
	if err != nil {
		fatal("encoding manifest: %v", err)
	}
	if err := st.putTag(ctx, name, tag, mb); err != nil {
		fatal("writing tag: %v", err)
	}
	fmt.Printf("Pushed %s\n", ref)
}

// layerRec carries a layer's compressed digest/size and uncompressed size.
type layerRec struct {
	digest       string
	size         int64
	uncompressed int64
}

// compressAndUpload gzip-compresses a layer stream from r, uploads it as a
// content-addressed blob (deduping against the bucket) and returns its
// descriptor info.
func compressAndUpload(ctx context.Context, st *store, r io.Reader) (layerRec, error) {
	tmp, err := os.CreateTemp("", "grue-layer-*")
	if err != nil {
		return layerRec{}, err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	gw := gzip.NewWriter(io.MultiWriter(tmp, h))
	cr := &countingReader{r: r}
	if _, err := io.Copy(gw, cr); err != nil {
		tmp.Close()
		return layerRec{}, err
	}
	if err := gw.Close(); err != nil {
		tmp.Close()
		return layerRec{}, err
	}
	return uploadTempLayer(ctx, st, tmp, h, cr.n)
}

// processLayer uploads a layer from a docker save tar entry. It detects
// whether the layer is already gzip-compressed (new Docker save OCI format)
// or uncompressed (old format) and handles each case.
func processLayer(ctx context.Context, st *store, r io.Reader) (layerRec, error) {
	br := bufio.NewReader(r)
	header, _ := br.Peek(2)
	if len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		return uploadCompressedLayer(ctx, st, br)
	}
	return compressAndUpload(ctx, st, br)
}

// uploadCompressedLayer uploads a layer that is already gzip-compressed. It
// saves the compressed stream to a temp file, computes the compressed digest,
// counts the uncompressed bytes, and uploads the blob as-is.
func uploadCompressedLayer(ctx context.Context, st *store, r io.Reader) (layerRec, error) {
	tmp, err := os.CreateTemp("", "grue-layer-*")
	if err != nil {
		return layerRec{}, err
	}
	defer os.Remove(tmp.Name())

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return layerRec{}, err
	}

	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return layerRec{}, err
	}
	gr, err := gzip.NewReader(tmp)
	if err != nil {
		tmp.Close()
		return layerRec{}, err
	}
	uc, err := io.Copy(io.Discard, gr)
	gr.Close()
	if err != nil {
		tmp.Close()
		return layerRec{}, err
	}

	return uploadTempLayer(ctx, st, tmp, h, uc)
}

// uploadTempLayer uploads the contents of tmp (already written; h holds the
// sha256 of those bytes) as a content-addressed blob, deduping against the
// bucket. uc is the uncompressed byte count, stored in the layer descriptor.
// tmp is closed by this function.
func uploadTempLayer(ctx context.Context, st *store, tmp *os.File, h hash.Hash, uc int64) (layerRec, error) {
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		tmp.Close()
		return layerRec{}, err
	}
	fi, err := tmp.Stat()
	if err != nil {
		tmp.Close()
		return layerRec{}, err
	}
	digest := "sha256:" + hex.EncodeToString(h.Sum(nil))
	uploaded, err := st.putBlobIfAbsent(ctx, digest, tmp, fi.Size(), mediaTypeLayer)
	tmp.Close()
	if err != nil {
		return layerRec{}, err
	}
	fmt.Printf("  layer %s (%s)\n", digest, statusWord(uploaded))
	return layerRec{digest: digest, size: fi.Size(), uncompressed: uc}, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func statusWord(uploaded bool) string {
	if uploaded {
		return "uploaded"
	}
	return "exists"
}

// -----------------------------------------------------------------------------
// pull
// -----------------------------------------------------------------------------

func cmdPull() {
	if len(os.Args) < 3 {
		fatal("usage: grue pull <name[:tag]>")
	}
	name, tag, hasTag, err := parseRef(os.Args[2])
	if err != nil {
		fatal("%v", err)
	}
	if !hasTag {
		tag = "latest"
	}
	ref := name + ":" + tag

	ctx := context.Background()
	cfg, err := resolveConfig()
	if err != nil {
		fatal("%v", err)
	}
	st, err := newStore(cfg)
	if err != nil {
		fatal("%v", err)
	}
	dk, err := newDockerClient()
	if err != nil {
		fatal("%v", err)
	}

	fmt.Printf("Pulling %s\n", ref)
	mb, err := st.getTag(ctx, name, tag)
	if err != nil {
		fatal("reading tag: %v", err)
	}
	var m imageManifest
	if err := json.Unmarshal(mb, &m); err != nil {
		fatal("parsing manifest: %v", err)
	}
	if m.SchemaVersion != 2 || m.MediaType != mediaTypeManifest {
		fatal("not an OCI image manifest")
	}

	configBytes, err := readAll(ctx, st, m.Config.Digest)
	if err != nil {
		fatal("reading config blob: %v", err)
	}
	configHex := strings.TrimPrefix(m.Config.Digest, "sha256:")
	configFile := configHex + ".json"

	layerPaths := make([]string, len(m.Layers))
	for i := range m.Layers {
		layerPaths[i] = fmt.Sprintf("%d/layer.tar", i)
	}
	loadManifest, _ := json.Marshal([]saveManifestItem{{
		Config:   configFile,
		RepoTags: []string{ref},
		Layers:   layerPaths,
	}})

	pr, pw := io.Pipe()
	go func() {
		err := writeLoadTar(pw, st, &m, configBytes, configFile, loadManifest)
		pw.CloseWithError(err)
	}()

	resp, err := dk.request(ctx, http.MethodPost, "/images/load", pr)
	if err != nil {
		fatal("docker: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		fatal("docker load: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if err := printLoadProgress(resp.Body); err != nil {
		fatal("docker load: %v", err)
	}
	fmt.Printf("Loaded %s\n", ref)
}

// writeLoadTar writes a `docker load`-compatible tar to w, streaming blobs
// straight from the bucket.
func writeLoadTar(w io.Writer, st *store, m *imageManifest, configBytes []byte, configFile string, loadManifest []byte) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	write := func(name string, body []byte) error {
		if err := tw.WriteHeader(&tar.Header{
			Name:     name,
			Mode:     0o644,
			Size:     int64(len(body)),
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}

	if err := write(configFile, configBytes); err != nil {
		return err
	}

	for i, ld := range m.Layers {
		size, err := uncompressedSize(ld)
		if err != nil {
			return fmt.Errorf("layer %d: %w", i, err)
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     fmt.Sprintf("%d/layer.tar", i),
			Mode:     0o644,
			Size:     size,
			Typeflag: tar.TypeReg,
		}); err != nil {
			return err
		}
		rc, err := st.getBlob(context.Background(), ld.Digest)
		if err != nil {
			return err
		}
		gr, err := gzip.NewReader(rc)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(tw, gr); err != nil {
			rc.Close()
			return err
		}
		gr.Close()
		rc.Close()
		fmt.Printf("  layer %d/%d %s\n", i+1, len(m.Layers), ld.Digest)
	}

	if err := write("manifest.json", loadManifest); err != nil {
		return err
	}
	if err := write("VERSION", []byte("1.0")); err != nil {
		return err
	}
	return nil
}

func uncompressedSize(ld descriptor) (int64, error) {
	s := ld.Annotations[annUncompressedSize]
	if s == "" {
		return 0, fmt.Errorf("missing %s annotation", annUncompressedSize)
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, err
	}
	return n, nil
}

// printLoadProgress reads Docker's JSON progress stream from a load response.
func printLoadProgress(r io.Reader) error {
	dec := json.NewDecoder(r)
	for {
		var msg struct {
			Stream string `json:"stream"`
			Error  string `json:"error"`
		}
		if err := dec.Decode(&msg); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if msg.Error != "" {
			return errors.New(msg.Error)
		}
		if msg.Stream != "" {
			fmt.Print(msg.Stream)
		}
	}
}

// readAll fetches a blob fully into memory (used for the small config blob).
func readAll(ctx context.Context, st *store, digest string) ([]byte, error) {
	rc, err := st.getBlob(ctx, digest)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// -----------------------------------------------------------------------------
// ls
// -----------------------------------------------------------------------------

func cmdList() {
	ctx := context.Background()
	cfg, err := resolveConfig()
	if err != nil {
		fatal("%v", err)
	}
	st, err := newStore(cfg)
	if err != nil {
		fatal("%v", err)
	}

	type entry struct{ repo, tag, digest string }
	var entries []entry

	prefix := st.prefix + "repos/"
	ch := st.client.ListObjects(ctx, st.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for obj := range ch {
		if obj.Err != nil {
			fatal("listing: %v", obj.Err)
		}
		rel := strings.TrimPrefix(obj.Key, prefix)
		// rel == <name>/tags/<tag>; match the trailing /tags/ so repo names
		// containing a "tags" path segment are not mis-split.
		const tags = "/tags/"
		idx := strings.LastIndex(rel, tags)
		if idx < 0 {
			continue
		}
		repo := rel[:idx]
		tag := rel[idx+len(tags):]
		objRc, err := st.client.GetObject(ctx, st.bucket, obj.Key, minio.GetObjectOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "grue: reading %s: %v\n", obj.Key, err)
			continue
		}
		b, err := io.ReadAll(objRc)
		objRc.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "grue: reading %s: %v\n", obj.Key, err)
			continue
		}
		entries = append(entries, entry{repo, tag, digestOf(b)})
	}

	if len(entries) == 0 {
		return
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].repo != entries[j].repo {
			return entries[i].repo < entries[j].repo
		}
		return entries[i].tag < entries[j].tag
	})
	cur := ""
	for _, e := range entries {
		if e.repo != cur {
			cur = e.repo
			fmt.Println(cur + ":")
		}
		fmt.Printf("  %s\t%s\n", e.tag, e.digest)
	}
}

// -----------------------------------------------------------------------------
// rm
// -----------------------------------------------------------------------------

func cmdRemove() {
	if len(os.Args) < 3 {
		fatal("usage: grue rm <name[:tag]>")
	}
	name, tag, hasTag, err := parseRef(os.Args[2])
	if err != nil {
		fatal("%v", err)
	}
	ctx := context.Background()
	cfg, err := resolveConfig()
	if err != nil {
		fatal("%v", err)
	}
	st, err := newStore(cfg)
	if err != nil {
		fatal("%v", err)
	}

	if hasTag && tag != "" {
		if err := st.deleteTag(ctx, name, tag); err != nil {
			fatal("deleting %s:%s: %v", name, tag, err)
		}
		fmt.Printf("Deleted %s:%s\n", name, tag)
		return
	}

	// No tag: delete all tags of the repo.
	prefix := st.prefix + "repos/" + name + "/tags/"
	var keys []string
	ch := st.client.ListObjects(ctx, st.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true})
	for obj := range ch {
		if obj.Err != nil {
			fatal("listing: %v", obj.Err)
		}
		keys = append(keys, obj.Key)
	}
	if len(keys) == 0 {
		fatal("no tags found for %q", name)
	}
	if !confirm(fmt.Sprintf("Delete all %d tag(s) of %q?", len(keys), name)) {
		fmt.Println("aborted")
		return
	}
	for _, k := range keys {
		if err := st.client.RemoveObject(ctx, st.bucket, k, minio.RemoveObjectOptions{}); err != nil {
			fatal("deleting %s: %v", k, err)
		}
	}
	fmt.Printf("Deleted %d tag(s) of %q (blobs retained).\n", len(keys), name)
}

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

func digestOf(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// parseRef splits an image reference into its repository name and tag
func parseRef(arg string) (name, tag string, hasTag bool, err error) {
	ref, perr := reference.Parse(arg)
	if perr != nil {
		return "", "", false, fmt.Errorf("invalid image reference %q: %w", arg, perr)
	}
	named, ok := ref.(reference.Named)
	if !ok {
		return "", "", false, fmt.Errorf("invalid image reference %q", arg)
	}
	if _, ok := ref.(reference.Digested); ok {
		return "", "", false, fmt.Errorf("digest refs are not supported (got %q)", arg)
	}
	name = reference.FamiliarName(named)
	if tagged, ok := ref.(reference.Tagged); ok {
		tag = tagged.Tag()
		hasTag = true
	}
	return
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func fatal(format string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "grue: "+format+"\n", a...)
	os.Exit(1)
}
