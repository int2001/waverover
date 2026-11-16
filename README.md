# WaveRover

<img src="https://github.com/int2001/waverover/blob/master/WR.png" width="400" alt="Logo">

This tool helps first-time users to migrate their logs to Wavelog. Import ADIF files into Wavelog with automatic station location management. The tool detects unique station locations from your ADIF records, creates missing station profiles in Wavelog, and imports QSOs to the correct profile.

> **Warning:** Make sure the rate limits on your Wavelog instance are set high enough before importing.

## Usage

```
waveRover -url <url> -key <key> [options] <file.adi> [file2.adi ...]
```

File arguments support glob wildcards: `*.adi`, `data/**/*.adi`, etc.

### Options

| Flag | Default | Description |
|------|---------|-------------|
| `-url` | — | Wavelog base URL; HTTPS if the scheme is omitted (or set `WAVELOG_URL`) |
| `-key` | — | Legacy read+write API key or API v2 token (`wl2_…`), or set `WAVELOG_API_KEY` |
| `-dry-run` | false | Parse and show what would happen, but don't import |
| `-match-fields` | `STATION_CALLSIGN,MY_GRIDSQUARE` | Comma-separated ADIF fields that define a unique station location |
| `-name-format` | — | Profile name template with `{FIELD}` placeholders from match fields, e.g. `"{STATION_CALLSIGN} @ {MY_SOTA_REF}"`. Empty: name is built automatically from all match field values |
| `-grid-precision` | `6` | Grid locator precision for grouping. `6` = full grid square, `4` = grid field only |
| `-lookup` | `true` | When a new profile is missing DXCC/CQ/Country, resolve them from the station callsign via Wavelog's lookup. Set `-lookup=false` to disable |
| `-itu-zone` | — | Default ITU zone for profiles missing `MY_ITU_ZONE` (Wavelog needs it to import QSOs). If empty, you are prompted per location |

### Getting your API key

In Wavelog: User Menu → API. Both credential types are supported:

- **Legacy API key:** create a read+write key as before. Existing commands and legacy API behavior remain supported.
- **API v2 token:** on **Wavelog 3.2.0 or later**, create a dedicated token with these scopes:
  - `station:read`, `station:write`: find and create station locations.
  - `qso:write`: import contacts.
  - `logbook:read`, `logbook:write`: link new locations to the active logbook.
  - `lookup:read`: fill missing station data; not required with `-lookup=false`.

The client checks this complete set before starting an import, including when you expect every station/contact to be a duplicate. A token with only `lookup:read`, `qso:read`, `qso:write`, `station:read`, and `station:write` therefore also needs **`logbook:read` and `logbook:write`**. `qso:read` is not required: Wavelog checks contact duplicates during import. Use the expanded token permissions or create a replacement token with the required scopes.

API v2 was introduced in Wavelog 3.1.0, but WaveRover also needs the logbook resource added in 3.2.0. Clubstation tokens additionally need officer-level permissions for station/logbook management. No delete or administrator scopes are required. Choose an expiry and copy the token when Wavelog shows it; it is only displayed once. Renew expired tokens or revoke them from the same API page.

Pass either credential through `-key` or `WAVELOG_API_KEY`. A credential beginning with `wl2_` selects API v2 automatically. Other keys retain the legacy client. V2 requests use the Bearer authorization header, not a key in a URL or request body. Invalid/expired v2 tokens never fall back to legacy authentication.

For example, to avoid putting the token into your shell history or command-line arguments:

```bash
export WAVELOG_URL=https://log.example.com
read -r -s -p 'Wavelog token: ' WAVELOG_API_KEY; echo
export WAVELOG_API_KEY
waveRover activation.adi
unset WAVELOG_API_KEY
```

Use the final instance URL, including `/index.php` or a deployment subdirectory if your installation requires it. A bare hostname such as `log.example.com` defaults to **`https://log.example.com`**. This applies to both `-url` and `WAVELOG_URL`, for legacy keys and v2 tokens. Explicit `http://` and `https://` URLs remain supported; a supplied `-url` takes precedence over the environment variable. Invalid URLs, unsupported schemes, and base URLs containing query strings or fragments are rejected before parsing the log, including in dry runs.

WaveRover **never automatically falls back to HTTP** if HTTPS fails, since that could expose the API credential. For a trusted instance that requires unencrypted HTTP, explicitly use `-url http://host:port` or set `WAVELOG_URL` to that complete HTTP URL. The v2 client refuses redirects; use the destination HTTPS URL directly rather than an HTTP-to-HTTPS redirect.

V2 authentication and required scopes are checked before importing. **`-dry-run` is local parsing/grouping only:** it does not contact Wavelog, check credentials, resolve existing locations, or validate server-side imports.

If a `wl2_` token produces an error mentioning **`/api/version`**, you are running an older executable, not this branch's v2 client. Rebuild after changing branches or updating the source:

```sh
go build -o waveRover .
./waveRover -help
```

The updated `-key` help mentions API v2 tokens. A non-dry-run v2 connection checks `/api/v2/token`; missing scopes are listed by name before any contacts or locations are written. `go test` builds its own temporary executable and does not replace an existing `./waveRover` binary.

## Examples

### Basic import

```
waveRover -url https://log.example.com -key abc123 my_log.adi
```

### Dry run first (recommended)

```
waveRover -url https://log.example.com -key abc123 -dry-run my_log.adi
```

Output shows all detected station locations and QSO counts without touching Wavelog.

### Multiple ADIF files

```
waveRover -url https://log.example.com -key abc123 lotw_export.adi clublog.adi
```

### Wildcard import

```
waveRover -url https://log.example.com -key abc123 *.adi
waveRover -url https://log.example.com -key abc123 ~/adif/**/*.adi
```

### Import a LoTW export

```
waveRover -url https://log.example.com -key abc123 ~/Downloads/lotw_exp.adi
```

LoTW exports are handled automatically — see [LoTW special handling](#lotw-special-handling) below.

### Broader grouping with 4-character grids

```
waveRover -url https://log.example.com -key abc123 -grid-precision 4 my_log.adi
```

Groups QSOs by 4-char grid field (e.g. `JO30`) instead of 6-char square (e.g. `JO30OO`). Grids in the imported QSOs are truncated to match.

### Match on additional fields (SOTA, POTA, etc.)

```
waveRover -url https://log.example.com -key abc123 \
  -match-fields "STATION_CALLSIGN,MY_GRIDSQUARE,MY_SOTA_REF" my_log.adi
```

QSOs with the same callsign and grid but different SOTA references are treated as separate station locations. Any ADIF `MY_*` field is supported: `MY_SOTA_REF`, `MY_POTA_REF`, `MY_WWFF_REF`, `MY_SIG`, `MY_SIG_INFO`, `MY_CITY`, `MY_IOTA`, `MY_STATE`, etc.

### Profile naming

By default, new station profiles are named from all match field values: the first value, then `@`, then the rest. With the match fields above, a profile becomes e.g. `DL1ABC @ JO30OO DM/RP-001`.

Use `-name-format` to define your own scheme with `{FIELD}` placeholders (match fields only):

```
waveRover -url https://log.example.com -key abc123 \
  -match-fields "STATION_CALLSIGN,MY_SOTA_REF" \
  -name-format "SOTA {MY_SOTA_REF} ({STATION_CALLSIGN})" my_log.adi
```

Creates profiles like `SOTA DM/RP-001 (DL1ABC)`. Placeholders referencing fields not listed in `-match-fields` produce a warning and stay empty.

### Using environment variables

```
export WAVELOG_URL=https://log.example.com
export WAVELOG_API_KEY=abc123
waveRover my_log.adi
```

## Missing station data (DXCC/CQ/Country lookup)

Wavelog needs DXCC, CQ zone, and country to create a station profile. ADIFs that lack the
`MY_DXCC`, `MY_CQ_ZONE`, and `MY_COUNTRY` fields would otherwise fail at profile creation.

When a new profile is missing any of these, the tool resolves them from the station's own
callsign (`STATION_CALLSIGN`, falling back to `MY_STATION_CALLSIGN` / `OPERATOR`) via Wavelog's
`private_lookup` API and fills the gaps:

```
  Looked up DL1ABC → DXCC 230 (FEDERAL REPUBLIC OF GERMANY), CQ 14
```

Details:

- **Fill-missing-only** — values already present in the ADIF are never overwritten.
- **One lookup per callsign** — results are cached, so locations sharing a callsign reuse a
  single request. Matched/existing profiles trigger no lookup at all.
- **ITU zone lookup depends on the Wavelog version** — when the callsign lookup returns an ITU
  zone, WaveRover uses it if `MY_ITU_ZONE` is missing. Wavelog lets you create a
  profile with an empty ITU zone, but then fails every QSO import for that profile, so waveRover
  never leaves it empty. If neither the ADIF nor lookup supplies a zone, it falls back to
  `-itu-zone`, or prompts you per location if that flag isn't set. With `-lookup=false`, the
  callsign lookup is skipped for both API versions.

  ```
    Location "DL1ABC @ JO30OO" has no MY_ITU_ZONE, but Wavelog needs one to import QSOs.
    Enter ITU zone (1-90): 28
      Applied ITU zone 28
  ```
- Disable the DXCC/CQ/Country lookup with `-lookup=false` (e.g. offline runs). ITU fallback still
  applies, since it does not depend on the lookup.

If the lookup can't resolve a callsign, that station location is skipped with a warning and the
run continues with the others. The QSOs appear under `Errors` in the summary:

```
  ERROR creating profile: cannot resolve mandatory station fields (DXCC/CQ/Country) for "DL1ABC" via lookup: no DXCC entity returned
```

## Re-running and duplicates

QSOs that already exist in the selected Wavelog location are detected as duplicates and skipped. With a legacy key, the summary lists each duplicate by callsign, band, mode, date, and time:

```
=== Summary ===
Imported: 17950
Duplicates skipped: 10

Duplicates:
  W1AW 20M FT8 20260515 142300
  DL2XYZ 40M CW 20260516 080100
  ...
```

If a batch contains a mix of new and duplicate QSOs, the new ones are imported and only the duplicates are skipped.

With an **API v2 token**, Wavelog returns aggregate `parsed`, `imported`, and `skipped` counts, plus validation messages. It does not identify every individual duplicate or rejected contact. WaveRover reports these as batch counts instead of attributing them to particular QSOs. For example:

```text
=== Summary ===
Imported: 12
Duplicates skipped: 2
Errors: 1

Duplicates:
  2 QSOs (API v2 batch result; individual QSOs not identified)
```

V2 batches are never automatically replayed: some contacts may already have been saved before an error. V2 runs with import errors exit nonzero; duplicate-only runs succeed. After a network error, the outcome may be unknown; inspect Wavelog before retrying. Keep the same matching options when retrying, since duplicate checking is per selected location.

New v2 locations are linked to the active logbook if one exists. The client reads its latest station list before appending the new location, preserving existing links; avoid editing that list concurrently because the API replaces it rather than providing an atomic append. If creation succeeds but linkage fails, the error includes the created location ID; repair the link in Station Setup before retrying, particularly when your account only lists locations in the active logbook. Without an active logbook, the location is created unlinked, matching the legacy behavior. Existing locations are not relinked or reconfigured.

V2 creation leaves service credentials empty and Club Log upload disabled. The station API controls which location is active (the first location may become active); WaveRover does not explicitly switch an existing active location. Review newly created profiles in Wavelog before configuring third-party uploads.

## LoTW special handling

When importing an ADIF from [ARRL Logbook of the World](https://lotw.arrl.org/), the tool applies several transforms automatically:

**Field conversion:**
- `APP_LoTW_RXQSL` (LoTW QSL received timestamp) is converted to ADIF-compliant `LOTW_QSLRDATE` + `LOTW_QSL_RCVD=Y`
- `APP_LoTW_RXQSO` (LoTW QSO insert timestamp) is converted to `LOTW_QSLSDATE` + `LOTW_QSL_SENT=Y`
- Date format is converted from LoTW format (`2026-05-20 10:46:46`) to ADIF format (`20260520`)

**QSL field cleanup:**
- `QSL_RCVD` and `QSLRDATE` are stripped from LoTW exports — their content simply sets QSL_RCVD (meant for paper) regardless if there's a paper-QSL or not.

**RST defaults:**
- Missing `RST_SENT` and `RST_RCVD` are filled based on mode: `59` for phone (SSB, FM, AM, USB, LSB), `599` for CW, `0` for digital modes (FT8, RTTY, etc.)

These transforms only apply when LoTW-specific fields are detected, so regular ADIF files are left unchanged.

### A word on LoTW Exports

Importing ADIF files generated by LoTW (Logbook of The World) exports is **not recommended**. LoTW's export format has several limitations that cause data loss:

- **QSL information is overwritten** — LoTW marks every confirmed QSO with `QSL_RCVD="Y"`, which in most logging software indicates a *paper QSL card* was received. This incorrectly sets QSL status and loses the distinction between LoTW confirmation and physical card confirmation.
- **RST is lost** — LoTW exports do not include sent/received signal reports (RST), so imported QSOs will be missing this data.
- **Duplicate risk** — Re-importing LoTW confirmations on top of existing QSOs can create duplicates or overwrite manually maintained QSL records.

If you need LoTW confirmation data, it's better to match against your own log (e.g. using Wavelog's LoTW sync) rather than importing raw LoTW exports.

Note the following version requirements:

- **Wavelog 2.4.3 or later** is required for matching beyond callsign and grid square. Newer versions support additional match criteria (xOTA, WWFF, SIG/SIG_INFO).
- **Wavelog below 2.4.3** only supports matching by callsign and grid square.

## How it works

1. Parses all ADIF files and extracts QSO records
2. Groups QSOs by unique station locations (default: callsign + grid square)
3. Fetches existing station profiles from Wavelog
4. For each location:
   - Matches against existing profiles (callsign, grid, and any additional match fields)
   - Creates a new station profile if no match is found (named from the match field values, or your `-name-format` template). Missing DXCC/CQ/Country are resolved from the station callsign — see [Missing station data](#missing-station-data-dxcccqcountry-lookup)
   - Imports all QSOs in 1MB chunks
5. With a legacy key, handles duplicate aborts by importing remaining QSOs individually; with a v2 token, accounts for the returned batch counts without replaying the batch
6. Prints a summary with counts and available details for duplicates or errors

## Building

Requires the Go version declared in `go.mod` (currently Go 1.26.3 or later).

```
go build -o waveRover .
```

Cross-compile for other platforms:

```
GOOS=linux GOARCH=amd64 go build -o waveRover .
GOOS=windows GOARCH=amd64 go build -o waveRover.exe .
GOOS=darwin GOARCH=arm64 go build -o waveRover .
```

## Testing

The integration suite lives in [`test/`](test/) and builds the real CLI. It uses temporary Go HTTP servers that check requests and return representative Wavelog API responses. No running Wavelog, Docker, real credentials, or database is needed, and no personal logs are modified.

```bash
go test -race ./...
go vet ./...
```

The tests build a race-instrumented CLI, requiring a native platform supported by Go's race detector and a C compiler. CI runs them on Linux amd64. They cover both API versions, station creation/matching, logbook linkage, lookup enrichment, authentication/scope failures, duplicate handling, and partially successful imports. See [`test/README.md`](test/README.md) for the contract sources and manual integration checks against a disposable Wavelog account.
