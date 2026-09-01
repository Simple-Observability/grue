<div align="center">
  <img src="logo.png" width="240" alt="grue" />
</div>

<div align="center">

`grue` - **because S3 is all you need**



</div>

Why run a container registry when you already have a S3?

`grue` is a CLI that turns any S3-compatible bucket into a container registry. It pushes and pulls Docker images directly to and from object storage. You can use Cloudflare R2, AWS S3, MinIO, Backblaze B2, or another S3-compatible store. Authentication uses your S3 API keys.

The whole program is one file, `grue.go`, and under 1000 lines of code.

### Commands

- `grue connect` - configure an S3-compatible storage backend
- `grue push <name[:tag]>` - push a local image to the bucket
- `grue pull <name[:tag]>` - pull an image from the bucket into Docker
- `grue ls` - list repositories and tags
- `grue rm <name[:tag]>` - delete a tag, or all tags of a repository

### Usage

```bash
# Connect to S3
grue connect

# Push a local container image to your bucket
grue push my-image:1.2.3

# On another machine, pull it back
grue pull my-image:1.2.3

# The image appears in Docker
docker run my-image:1.2.3
```

Use env secrets:

```bash
GRUE_ACCESS_KEY=... grue push my-image:1.2.3
```

List what is in the bucket:

```bash
grue ls
```

Delete a tag:

```bash
grue rm my-image:old
```

Delete all tags of a repository:

```bash
grue rm my-image
```

### Configuration

Environment variables take priority over the config file. The `AWS_*` variables use the standard AWS credential chain.

| Variable          | Purpose                      |
|-------------------|------------------------------|
| `GRUE_ENDPOINT`   | S3 endpoint URL              |
| `GRUE_REGION`     | Region                       |
| `GRUE_ACCESS_KEY` | Access key                   |
| `GRUE_SECRET_KEY` | Secret key                   |
| `GRUE_BUCKET`     | Bucket name                  |
| `GRUE_PREFIX`     | Key prefix (default `grue/`) |
| `GRUE_CONFIG`     | Path to the config file      |

The config file is at `~/.config/grue/config.json` by default. Override the path with `GRUE_CONFIG`.
