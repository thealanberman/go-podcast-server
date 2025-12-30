# Go Podcast Server

A simple, powerful podcast server written in Go. It automatically generates an RSS feed from a directory of audio files (`.mp3` and `.m4a`) and serves them via HTTP.

It includes built-in support for **Tailscale Funnel**, allowing you to securely expose your podcast to the public internet with a single command-line flag, no port forwarding required.

## Features

*   **Auto-Generated Feed**: Scans a directory for audio files and builds a podcast RSS feed.
*   **Format Support**: Supports `.mp3` and `.m4a` files.
*   **Metadata Parsing**:
    *   Extracts duration automatically.
    *   Uses file modification time for publication date.
    *   Supports sidecar `.md` files for custom titles and descriptions.
    *   Supports per-episode images (same filename as audio, e.g., `episode1.jpg`).
*   **Smart Sorting**: Episodes are sorted alphanumerically by title.
*   **Tailscale Funnel**: Built-in support to expose your server publicly via Tailscale.
*   **State Persistence**: Persists Tailscale identity and certificates in a local state directory.

## Installation

### Build from Source

You need Go installed (1.20+ recommended).

```bash
# Clone the repository
git clone <your-repo-url>
cd go-podcast-server

# Build a static binary for the current platform
CGO_ENABLED=0 go build -o go-podcast-server .

# Build a static binary for Raspberry Pi (ARM64)
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o go-podcast-server-pi .

# Build a static binary for Linux AMD64
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o go-podcast-server-linux-amd64 .
```

## Configuration

The application uses a `config.yaml` file. A default one will be generated if it doesn't exist.

**Example `config.yaml`:**

```yaml
port: "8080"
base_url: "http://localhost:8080"  # Overridden automatically when using -tailscale
audio_folder: "./audio"            # Path to your audio files
podcast:
  title: "My Awesome Podcast"
  description: "A podcast about cool things."
  author: "Jane Doe"
  email: "jane@example.com"
  language: "en-us"
  category: "Technology"
  sub_category: "Software"
  explicit: "no"
```

## Usage

### Local Mode

Serve the podcast on your local network (default port 8080).

```bash
./go-podcast-server
```

Access the feed at: `http://localhost:8080/feed.xml`

### Tailscale Funnel Mode

Expose the podcast to the public internet using Tailscale Funnel.

```bash
./go-podcast-server -tailscale
```

*   **First Run**: You will be prompted to authenticate with a URL.
*   **Access**: The server will log the public URL, e.g., `https://podcast-server-v2.tailnet-name.ts.net/feed.xml`.
*   **Requirements**: You must enable **Funnel** for the node in your Tailscale Admin Console (Access Controls and Machine Settings).

### Other Flags

*   `-verbose`: Enable detailed logging for Tailscale debugging.

## Content Management

1.  **Audio Files**: Place `.mp3` or `.m4a` files in your `audio_folder`.
2.  **Metadata**:
    *   By default, the filename is used as the title.
    *   Create a markdown file with the same name (e.g., `episode1.md`) to define a custom title and description.
        *   **Title**: The first line starting with `# `.
        *   **Description**: The rest of the file content.
3.  **Images**:
    *   **Channel Image**: Name an image `default.jpg` (or png) in the audio folder.
    *   **Episode Image**: Name an image matching the audio file (e.g., `episode1.jpg`).

## License

MIT
