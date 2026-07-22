# Synology NAS setup

Prepare the NAS once: a dedicated read-only service account, and a TLS trust
strategy for DSM's default self-signed certificate.

## DSM service account

The storage, Active Backup, and Hyper Backup APIs require an
**administrators-group** account. A standard user receives permission errors (DSM
code 105) and the collector reports the affected area as inaccessible. (Hyper
Backup is an optional package: if it is not installed, the collector reports it as
`NOT_INSTALLED` — an expected, healthy state, not an error.)

Recommended setup on the NAS:

1. **Control Panel → User & Group → Create** a dedicated account, e.g. `svc-rmm`.
2. Add it to the **administrators** group.
3. **Disable 2-Step Verification** for this account. The Web API login is
   non-interactive; if 2FA is enforced, login fails with a clear message telling
   you to use a service account without 2FA.
4. Optionally restrict it: **Control Panel → Security → Account** allow-list the
   RMM/agent source IP so the account can only sign in from your management host.
5. Use a strong, unique password and store it in your RMM's secure field.

The collector only ever performs **read** operations.

## TLS

Synology ships self-signed certificates by default, so certificate verification
would normally fail. The collector verifies certificates **by default**; choose
one of:

- **`--tls-pin <fingerprint>` (recommended).** Pins the server's certificate by
  its SHA-256 fingerprint. This is a real verification mode — the connection is
  refused if the certificate does not match — not an unverified connection. Get
  the fingerprint with:

  ```bash
  openssl s_client -connect 192.168.1.20:5001 </dev/null 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256
  ```

  Paste the value into `--tls-pin` (colons and an optional `sha256:` prefix are
  accepted).

- **`--ca-file bundle.pem`.** If you run an internal CA and installed a proper
  certificate on the NAS, verify against your CA bundle.

- **`--insecure-skip-verify`.** Disables verification entirely. Last resort; the
  connection is encrypted but not authenticated.
