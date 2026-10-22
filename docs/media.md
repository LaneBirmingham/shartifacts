# Media and data files

Open **Media library** in the sidebar to upload files and copy HTML embed code. Data files (CSV, JSON, plain text, Markdown, XLSX, PDF) get a `window.fs.readFile(...)` snippet instead; uploads keep their file name for it. Agents add `?name=sales.csv` to the upload URL to do the same. HTML and React previews support native media elements, including controls, playback, seeking, posters, and WebVTT captions. Uploaded files are immutable and stored under `data/media/`; duplicate bytes with the same MIME type reuse the same URL. Files survive restarts, artifact revisions, duplication, and deletion. Back up the entire data directory. There is no automatic media deletion, so old versions keep working.

For shell-capable agents, call **`list_media` first**. Alongside the media list it returns `base_url`, the exact `upload_url` (including host and port), the HTTP method, maximum size and a ready-to-run `curl_example`. The same URL is also advertised in MCP initialization, media tool descriptions, `list_media` and `begin_media_upload` results. No upload session is needed for HTTP uploads.

Replace the example filename and MIME type, then run the command in the agent's shell. Curl reads the file bytes directly; no base64 passes through model context. Use the returned `/media/...` URL in the artifact. The URL is derived separately for each request from the validated MCP Host and direct HTTP/TLS connection, so LAN, IPv6 and Tailscale clients receive their own connection address. Forwarded headers are ignored. The agent's shell must be able to reach that address; an MCP connection routed through a separate host does not itself give a sandbox shell network access.

A remote MCP server cannot open a `file_path` on the agent's machine. If shell networking is unavailable, use the browser Media library or a client that can **programmatically supply** base64 chunks. Do not manually transcribe encoded output: shell output truncation and transcription errors corrupt it. There is no universal model-safe transcription size; the 1 MiB limit below is a protocol limit.

For clients that can supply bytes programmatically (hidden by `SHARTIFACTS_NO_CHUNK_UPLOAD=true`, which saves their tool schemas in agent context when only shell agents connect):

1. Call `begin_media_upload` with `mime_type`, such as `video/mp4`.
2. Call `upload_media_chunk` with the returned `upload_id`, `offset: 0`, and standard base64 in `data_base64`. Each chunk is at most **1 MiB decoded**. Use the returned offset for the next chunk.
3. Set `final: true` on the last chunk (an empty final chunk is allowed). The result includes `url`, `mime_type`, `size`, and `id`. Uploads expire after 30 minutes of inactivity; up to 16 uploads may be pending. Interrupted uploads must restart after a server restart.
4. Create or update an artifact referencing the returned URL. `list_media` discovers already uploaded files. Do not put base64 video into the 2 MiB artifact source.

Example of raw upload (use the actual URL returned by `list_media`):

```sh
curl -f -H 'Content-Type: video/mp4' --data-binary @clip.mp4 \
  http://192.168.1.20:8080/api/media
```

Use the returned URL literally in source:

```html
<video
  controls
  playsinline
  preload="metadata"
  src="/media/RETURNED_ID.mp4"
></video>
<audio controls src="/media/RETURNED_ID.mp3"></audio>
<img src="/media/RETURNED_ID.png" alt="A useful description" />
```

Maximum **256 MiB per file**, streamed to disk over HTTP, with byte-range requests for seeking. Supported MIME types: `video/mp4`, `video/webm`, `video/ogg`, `video/quicktime`; `audio/mpeg`, `audio/mp4`, `audio/ogg`, `audio/wav`, `audio/webm`, `audio/flac`; `image/png`, `image/jpeg`, `image/gif`, `image/webp`, `image/avif`; `text/vtt`; and data files `text/csv`, `application/json`, `text/plain`, `text/markdown`, `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet`, `application/pdf`. Every media response carries `Content-Security-Policy: default-src 'none'; sandbox`, so an uploaded file never runs as a page. Supply the correct MIME type; uploads are stored without transcoding. Actual playback depends on browser codec support; prefer MP4 with H.264/AAC or WebM. Autoplay generally requires `muted playsinline`. For caption tracks, put `crossorigin="anonymous"` on the video/audio element. Uploaded raster images also work as Markdown images. External URLs must first be downloaded by the agent's own tools and uploaded here; the server does not fetch remote URLs or open agent filesystem paths.

The MCP initialization instructions and tool schemas describe this workflow, limits, and examples to connected agents. Media endpoints allow read-only access from sandbox previews; API writes remain protected by the existing origin and LAN checks. Media is shared with everyone who can access this service.
