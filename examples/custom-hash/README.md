# custom-hash

A daemonpilot service whose release digests use SHA-384 (`UpdateConfig.NewHash`)
instead of the default SHA-256. The library feeds the staged bytes to the
application's hasher streaming — the binary is never held in memory — and the
built-in Ed25519 check signs the `SignatureContext` followed by the 48-byte
digest. Manifests carry that digest in the `digest` field; internal rollback
digests stay SHA-256 and are unaffected.

Setting `NewHash` — even explicitly to `sha256.New` — makes `digest` the only
accepted manifest field: any nonempty `sha256` field rejects the offer
regardless of its value, correct or not. The library never guesses the
algorithm from the manifest.

## Inputs

| Variable                     | Meaning                                                                                        |
| ---------------------------- | ---------------------------------------------------------------------------------------------- |
| `AGENT_ED25519_PUBLIC_KEY`   | hex Ed25519 public key (64 bytes) whose private half signs `SignatureContext + sha384(binary)` |
| `AGENT_RELEASE_MANIFEST_URL` | JSON manifest URL with the `digest` field set to the hex SHA-384                               |
| `AGENT_RELEASE_DOWNLOAD_URL` | artifact URL                                                                                   |

## Preparing a test release

```sh
# 1. Sign: any Ed25519 tool over "myapp-sha384-v1\x00" + sha384(binary) —
#    or use cmd/daemonpilot-sign with a SHA-384-capable wrapper around it.
# 2. Manifest: {"version":"2.0.0","size":N,"digest":"<hex sha384>"}.
#    Do not put the SHA-384 value into "sha256": that field means SHA-256.
```
