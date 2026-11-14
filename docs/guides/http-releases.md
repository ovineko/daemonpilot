# HTTP Releases

`pkg/httpsource` and `pkg/release` implement both halves of daemonpilot's signed
update protocol, using only the standard library. Signature and digest headers
arrive before the body, so downloads stream directly into the updater's staging
file without buffering the artifact in memory.

## Client

The client snippets use `github.com/ovineko/daemonpilot/pkg/httpsource` and
`github.com/ovineko/daemonpilot/pkg/daemonpilot`; the server snippets use
`github.com/ovineko/daemonpilot/pkg/release`. Supply the application's base URL,
credentials, trusted public key, and artifact map rather than fetching keys from
the release response.

```go
source, err := httpsource.New(httpsource.Config{
    ManifestURL: base + "/manifest/{os}/{arch}",
    DownloadURL: base + "/download/{version}/{os}/{arch}",
    Authorize: func(req *http.Request) error {
        req.Header.Set("Authorization", "Bearer " + token)
        return nil
    },
})
if err != nil {
    return err
}
update := &daemonpilot.UpdateConfig{Source: source, PublicKey: publicKey}
```

| `httpsource.Config` field             | Default and behavior                                                                                                                                                            |
| ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Client *http.Client`                 | Nil creates a client with 30-second dial and response-header timeouts, 10-second TLS handshake timeout, and no whole-body deadline. A supplied client's settings are preserved. |
| `ManifestURL string`                  | Empty disables polling for a push-only source. Expands `{os}` and `{arch}` from `runtime.GOOS/GOARCH`.                                                                          |
| `DownloadURL string`                  | Required. Expands path-escaped `{version}`, `{os}`, and `{arch}`.                                                                                                               |
| `Authorize func(*http.Request) error` | Nil leaves requests unchanged. Called for every outgoing request, including fallback and redirects; an error aborts it.                                                         |
| `SignatureHeader string`              | `X-Daemonpilot-Signature`; required on the wire, hex-encoded 64-byte ed25519 signature.                                                                                         |
| `SHA256Header string`                 | `X-Daemonpilot-Sha256`; optional hex-encoded 32-byte digest.                                                                                                                    |
| `FallbackVersion string`              | Empty disables fallback. Otherwise a concrete version's 404 triggers exactly one request with this version, unless already requesting it.                                       |

Templates must be absolute HTTP(S) URLs without embedded credentials or fragments.
Placeholders belong in the path; unknown placeholders are rejected. Header names
must be valid, distinct HTTP field names. The authorization hook should check the
request's destination before adding credentials if the server can redirect to
another host. Use HTTPS for release traffic outside trusted local transport.
The default client has no body timeout: supply a context deadline or a custom
client when a download must be time-limited.

```go
func New(cfg Config) (*Source, error)
func (s *Source) CheckUpdate(ctx context.Context, currentVersion string) (*daemonpilot.UpdateInfo, error)
func (s *Source) DownloadUpdate(ctx context.Context, info *daemonpilot.UpdateInfo) (io.ReadCloser, []byte, error)
type StatusError struct { StatusCode int; Body string }
func (e *StatusError) Error() string
```

The manifest is JSON with `version`, `watchdog_version`, `size`, and `sha256`.
A 204, 404, or version equal to the current version means no update. Invalid JSON,
an empty version, or a negative size is an error; manifest decoding is limited to
1 MiB. No ordering is imposed beyond equality. A custom release hash
(`UpdateConfig.NewHash`) moves the digest to a `digest` field: under `NewHash` —
an explicit `sha256.New` included — any nonempty `sha256` field rejects the
offer, while without `NewHash` both fields must be valid SHA-256 hex that agree
when both are present.

`DownloadUpdate` returns an unread body and decoded signature. The caller must
close the body; the source closes it on every error. It fills an empty
`UpdateInfo.SHA256` from the hash header and a zero `Size` from a positive
Content-Length. Existing manifest metadata always wins, including on fallback:
if the server now serves a different artifact, verification fails and the next
manifest poll discovers the new release. Fallback never silently replaces the
manifest's integrity expectations. Other non-2xx responses return `*StatusError`,
whose body is capped at 512 bytes. A fallback failure reports the fallback error.

## Server

```go
handler, err := release.NewHandler(release.HandlerConfig{
    Key: privateKey,
    Resolve: func(goos, goarch string) (*release.Artifact, error) {
        return artifacts[goos + "/" + goarch], nil
    },
})
if err != nil {
    return err
}
mux.Handle("/releases/", handler)
```

Set the client's base to `https://your-server/releases`. No rewriting is needed:
the handler accepts any prefix and matches `/manifest/{os}/{arch}` and
`/download/{version}/{os}/{arch}`. GET and HEAD are supported. The resolver sees
only OS/architecture names from fixed Go platform allow-lists; request values are
never used as filesystem paths. The application maps each pair to a trusted file.

```go
type Artifact struct { Path, Version, WatchdogVersion string }
type HandlerConfig struct {
    Key ed25519.PrivateKey
    SignatureContext string
    Resolve func(goos, goarch string) (*Artifact, error)
    Authorize func(*http.Request) error
    SignatureHeader, SHA256Header string
}
func NewHandler(cfg HandlerConfig) (http.Handler, error)
```

`Key` and `Resolve` are required. Header defaults match the client. Nil
`Authorize` means open access; returning an error produces 401 without disclosing
the error. `(nil, nil)` from `Resolve` means 404; a resolver or file failure is 500. Callbacks must support concurrent requests.

**A download for any version serves the artifact `Resolve` currently returns.**
The server serves what it has; the version path is not a historical artifact
selector. Deploy immutable files through atomic replacement. Signatures are
cached by path, size, and modification time, with one current entry per path.
`SignatureContext` is copied into the handler at construction and stays fixed
for that handler and its cache.
Do not change content while retaining both size and mtime. Hashing is serialized
on cache misses so simultaneous fleet requests do not repeatedly hash the same
release; response streaming happens outside that lock. A rename during a request
cannot change the file descriptor being signed and served.

## Signing and Keys

```go
func SignDigest(priv ed25519.PrivateKey, digest [32]byte) []byte
func SignDigestContext(priv ed25519.PrivateKey, context string, digest [32]byte) []byte
func SignFile(priv ed25519.PrivateKey, path string) (sig []byte, sha256Hex string, size int64, err error)
func SignFileContext(priv ed25519.PrivateKey, context, path string) (sig []byte, sha256Hex string, size int64, err error)
```

`SignFile` streams a regular file and signs its SHA-256 digest, exactly as
daemonpilot's built-in verifier expects. `SignDigest` mirrors `ed25519.Sign` and
panics for an invalid private-key length; `SignFile` and `NewHandler` return errors
for invalid key lengths. The private key must be a valid ed25519 key.

The `Context` variants sign `[]byte(context) || SHA256(binary)`; their key-length
behavior matches the corresponding no-context functions. `SignDigest` and
`SignFile` delegate with an empty context, preserving their output byte for byte.
When a key signs multiple kinds of messages, use distinct purpose and version
tags for every use to prevent cross-purpose replay. End the tag in a NUL byte
to delimit it from the digest, for example `"myapp-binary-v1\x00"`.

Set `release.HandlerConfig.SignatureContext` and
`daemonpilot.UpdateConfig.SignatureContext` to that same string. The handler uses
it for every signature; `httpsource` needs no additional setting. The context is
local configuration, not a value trusted from an HTTP response. An empty or
different verifier context rejects the signature without fallback.
`UpdateConfig.VerifyFunc` replaces built-in verification entirely and ignores
`SignatureContext`.

From the daemonpilot source checkout, build the separate CLI with
`go build -o daemonpilot-sign ./cmd/daemonpilot-sign`, then run it from that directory:

```bash
./daemonpilot-sign keygen -key signing.key
./daemonpilot-sign pubkey -key signing.key
./daemonpilot-sign sign -key signing.key ./myapp
./daemonpilot-sign sign -key signing.key -context 'myapp-binary-v1\x00' ./myapp
```

The `sign` command's optional `-context <string>` flag accepts Go-style escapes:
it wraps the value in double quotes and uses `strconv.Unquote`. Preserve
backslashes with shell single quotes as above; `\x00` becomes a NUL byte.
Malformed escapes are errors. An absent or empty flag keeps the existing output.
Go API context strings are used directly, without this extra parsing.

The default `-key` path is `signing.key`. `keygen` creates the file with mode 0600
and refuses to overwrite an existing file. Its format is a **32-byte ed25519 seed
encoded as 64 hexadecimal characters and a newline**; it is not the 64-byte Go
private-key representation. To load it for `HandlerConfig.Key`, hex-decode the
trimmed contents, require `ed25519.SeedSize` bytes, then call
`ed25519.NewKeyFromSeed(seed)`. Protect the private key using filesystem access
controls appropriate to the host; embed only the public key in client binaries.

`keygen` and `pubkey` print the public key as hex. `sign` prints exactly two lines:
the signature hex followed by the SHA-256 hex. Errors go to stderr and exit 1.
The handler computes these values itself; no separate `.sig` request or file is
needed. The end-to-end release test runs a handler download through the real
`updater.DownloadAndVerify` path.
