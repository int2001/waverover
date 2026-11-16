# API compatibility tests

Run from the repository root:

```sh
go test -race ./...
go vet ./...
```

`TestMain` builds the real executable in a temporary directory, using the Go toolchain running the tests. Each test launches it with synthetic ADIF files and a temporary `httptest` HTTP server. The server checks the API path, authorization, payload, request ordering, and number of imports, then returns controlled responses. It is not a Wavelog installation and has no database or real credentials. The executable is race-instrumented too, so a supported native platform and C compiler are required.

Fixtures follow Wavelog's [v2 overview](https://docs.wavelog.org/developer/api-v2/), [station](https://docs.wavelog.org/developer/api-v2/station/), [logbook](https://docs.wavelog.org/developer/api-v2/logbook/), [lookup](https://docs.wavelog.org/developer/api-v2/lookup/), [QSO](https://docs.wavelog.org/developer/api-v2/qso/), and [legacy API](https://docs.wavelog.org/developer/api/) contracts. The v2 response shapes were also checked against the Wavelog 3.2.0 source tag and the supplied November 2026 checkout. In particular, `respond_bulk_import()` returns aggregate counts for partially saved batches, not a per-contact result list.

Automated checks cover:

- Real CLI imports using legacy keys and `wl2_` tokens, including an `/index.php/` base URL.
- Numeric/null v2 station fields, references in matching, new locations, existing locations, creation conflicts, and retaining logbook links added after the first read.
- Callsign enrichment and ITU lookup, using portable callsigns in encoded query parameters; the legacy lookup/creation wire format remains tested too.
- Expired/revoked credentials, insufficient scopes, optional lookup scope, rate limiting, malformed envelopes, and redirects. V2 failures never fall back to v1 or print the token.
- All-success, all-duplicate, mixed-success/error, unparsed, impossible/missing count, and server-error responses. V2 sends each batch only once; legacy duplicate-abort retry behavior is retained.
- Local dry runs for either credential type make no HTTP requests.
- Bare hostnames default to HTTPS for flags and environment configuration, explicit HTTP/HTTPS URLs retain installation subpaths, and malformed bases fail before parsing. HTTPS tests trust only a temporary test certificate through `SSL_CERT_FILE`; no TLS verification is disabled. An HTTPS failure never retries over HTTP.

## Manual integration before submitting a PR

Use a disposable Wavelog account/database with an active logbook and a synthetic ADIF. Do not use your real activation archive for these checks.

1. Create a legacy read/write key and a v2 token with the scopes listed in the main README. Run the same single-location ADIF with each credential in separate clean accounts. Compare callsign, grid, SOTA/POTA combination, DXCC/zones, county/state, per-QSO power, and logbook linkage. Export the contacts and compare operator references.
2. Repeat the file and confirm duplicate counts. Import a file with a mix of valid contacts, existing contacts, and a deliberately invalid contact. Confirm v2 reports the aggregate counts without replaying saved contacts and exits nonzero for errors. Inspect the actual database through Wavelog's UI.
3. Try missing DXCC/CQ/ITU data, with and without lookup, and with an explicit ITU fallback. Check a callsign lookup yielding no entity and a temporarily failing lookup.
4. Test token revocation, expiry, insufficient scopes, and a club token below officer permission; confirm creation/import is denied appropriately. Confirm a failed logbook permission check happens before location creation.
5. Confirm existing logbook links survive new location creation, and inspect the active station before/after. Test with no active logbook. A failure after creation is intentionally not rolled back; verify the location ID in the error and repair its link before retrying.
6. Check the account setting that filters station listings to the active logbook. Avoid concurrent manual logbook-link edits: v2 has a replace-list PATCH, not an atomic append.

Record the Wavelog/Go versions, branch revision, exact options (excluding credentials), and results with the PR. Fake-server tests validate client behavior; they do not establish that a live Wavelog deployment has passed these manual checks.
