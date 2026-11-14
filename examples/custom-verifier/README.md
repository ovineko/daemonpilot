# custom-verifier

A daemonpilot service whose update offers are verified by an application-supplied
ECDSA P-256 verifier (`UpdateConfig.VerifyMessage`) instead of the built-in
Ed25519 check. The key travels in the closure; no Ed25519 public key is
configured.

## Inputs

| Variable                                 | Meaning                                                                          |
| ---------------------------------------- | -------------------------------------------------------------------------------- |
| `AGENT_ECDSA_KEY_X`, `AGENT_ECDSA_KEY_Y` | hex-encoded P-256 coordinates of the verifier public key                         |
| `AGENT_RELEASE_MANIFEST_URL`             | JSON manifest URL (`version`, `size`, `digest` — hex SHA-256 of the binary)      |
| `AGENT_RELEASE_DOWNLOAD_URL`             | artifact URL; `X-Daemonpilot-Signature: <hex>` carries the ASN.1 ECDSA signature |

The signed message is `sha256("myapp-ecdsa-v1\x00" + release-digest)` — the
same construction `VerifyMessage` verifies.

## Preparing a test release

```sh
# 1. Verifier key pair (P-256); publish X/Y to the service, keep the private key.
openssl ecparam -name prime256v1 -genkey -noout -out ecdsa-key.pem
openssl ec -in ecdsa-key.pem -pubout | openssl pkey -pubin -text -noout  # read X, Y

# 2. Hash the release binary and sign the message with any ECDSA tool
#    (message = "myapp-ecdsa-v1\x00" + sha256(binary)).
sha256sum myapp-v2                      # digest for the manifest
# sign sha256("myapp-ecdsa-v1\x00" + digest) with ecdsa-key.pem → ASN.1 sig

# 3. Serve manifest ({"version":"2.0.0","size":N,"digest":"<hex>"}) and the
#    binary; point the two AGENT_RELEASE_* URLs at them.
```

Wrong keys or signatures fail verification; nothing falls back to Ed25519 or
to an unsigned apply.
