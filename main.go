package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func main() {
	url := flag.String("url", "", "Wavelog base URL (defaults to HTTPS if scheme omitted), or set WAVELOG_URL")
	key := flag.String("key", "", "Wavelog legacy read+write key or API v2 token (wl2_), or set WAVELOG_API_KEY")
	dryRun := flag.Bool("dry-run", false, "Show what would happen without making changes")
	matchFields := flag.String("match-fields", "STATION_CALLSIGN,MY_GRIDSQUARE", "Comma-separated ADIF fields for station location matching")
	nameFormat := flag.String("name-format", "", "Profile name template with {FIELD} placeholders from match fields, e.g. \"{STATION_CALLSIGN} @ {MY_SOTA_REF}\" (empty: auto from match fields)")
	gridPrecision := flag.Int("grid-precision", 6, "Grid locator precision (4 or 6)")
	lookup := flag.Bool("lookup", true, "Look up missing DXCC/CQ/Country from the station callsign via Wavelog when creating a profile")
	ituZone := flag.String("itu-zone", "", "Default ITU zone for profiles missing MY_ITU_ZONE (Wavelog needs it to import QSOs). If empty, prompt per location")
	flag.Parse()

	waveURL := coalesce(*url, os.Getenv("WAVELOG_URL"))
	waveKey := coalesce(*key, os.Getenv("WAVELOG_API_KEY"))
	files := expandFiles(flag.Args())

	if waveURL == "" || waveKey == "" || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: waveRover -url <url> -key <key> [-dry-run] <file.adi> [file2.adi ...]  (wildcards supported, e.g. *.adi)")
		fmt.Fprintln(os.Stderr, "\nFlags:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nEnvironment variables:")
		fmt.Fprintln(os.Stderr, "  WAVELOG_URL      Wavelog base URL")
		fmt.Fprintln(os.Stderr, "  WAVELOG_API_KEY  Wavelog API key")
		os.Exit(1)
	}

	defaultedHTTPS := !strings.Contains(waveURL, "://")
	waveURL, err := normalizeWavelogURL(waveURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid Wavelog URL: %v\n", err)
		os.Exit(1)
	}

	if *ituZone != "" && normalizeItuZone(*ituZone) == "" {
		fmt.Fprintf(os.Stderr, "Invalid -itu-zone %q: must be an integer 1-90\n", *ituZone)
		os.Exit(1)
	}

	fields := parseMatchFields(*matchFields)
	warnUnknownPlaceholders(*nameFormat, fields)
	fmt.Println("waveRover by DJ7NT")
	fmt.Printf("Match fields: %v\nGrid precision: %d\nDry run: %v\n\n", fields, *gridPrecision, *dryRun)

	var allQSOs []QSO
	for _, file := range files {
		fmt.Printf("Parsing %s...\n", file)
		qsos, err := parseFile(file)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error parsing %s: %v\n", file, err)
			os.Exit(1)
		}
		fmt.Printf("  Found %d QSOs\n", len(qsos))
		allQSOs = append(allQSOs, qsos...)
	}
	fmt.Printf("\nTotal QSOs: %d\n\n", len(allQSOs))

	if len(allQSOs) == 0 {
		fmt.Println("No QSOs found. Exiting.")
		return
	}

	locations := GroupByLocation(allQSOs, fields, *gridPrecision)
	fmt.Printf("Unique station locations: %d\n\n", len(locations))
	for i, loc := range locations {
		fmt.Printf("  [%d] %s (%d QSOs)\n", i+1, loc.DisplayName(fields, *nameFormat), len(loc.QSOs))
	}
	fmt.Println()

	if *dryRun {
		fmt.Println("Dry run — no changes made.")
		for _, loc := range locations {
			fmt.Printf("  Would import: %s (%d QSOs)\n", loc.DisplayName(fields, *nameFormat), len(loc.QSOs))
		}
		return
	}

	client := NewWavelogClient(waveURL, waveKey)
	version, err := client.CheckVersion()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to connect to Wavelog: %v\n", err)
		if defaultedHTTPS {
			fmt.Fprintln(os.Stderr, "No URL scheme was specified, so HTTPS was used. HTTP requires an explicit http:// URL; no automatic fallback was attempted.")
		}
		os.Exit(1)
	}
	if client.usesV2() {
		if err := client.checkV2Scopes(*lookup); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print("Connected to Wavelog (API v2)\n\n")
	} else {
		fmt.Printf("Connected to Wavelog v%s\n\n", version)
	}

	profiles, returnedFields, err := client.GetStationProfiles()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to fetch station profiles: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("Existing station profiles: %d\n", len(profiles))
	for _, p := range profiles {
		fmt.Printf("  #%s: %s (%s @ %s)\n", p.ID, p.Raw["station_profile_name"], p.Raw["station_callsign"], p.Raw["station_gridsquare"])
	}
	WarnUnsupportedFields(fields, returnedFields)
	fmt.Println()

	var allResults []ImportResult
	for i, loc := range locations {
		label := loc.DisplayName(fields, *nameFormat)
		fmt.Printf("[%d/%d] Processing %s (%d QSOs)\n", i+1, len(locations), label, len(loc.QSOs))

		profileID, err := MatchProfile(loc, profiles, fields, returnedFields)
		if err != nil {
			fmt.Println("  No matching profile found, creating new station location...")
			created, createErr := client.CreateStationProfile(loc, fields, *nameFormat, *lookup, *ituZone)
			if createErr != nil {
				fmt.Fprintf(os.Stderr, "  ERROR creating profile: %v\n", createErr)
				for j := range loc.QSOs {
					allResults = append(allResults, ImportResult{QSO: &loc.QSOs[j], Status: "error", Err: createErr})
				}
				continue
			}
			profileID = created.ProfileID
			profiles, returnedFields = created.Profiles, created.ReturnedFields
		} else {
			fmt.Printf("  Matched existing profile #%s\n", profileID)
		}

		allResults = append(allResults, client.ImportQSOsChunked(profileID, loc.QSOs, *gridPrecision, label)...)
	}

	printSummary(allResults)
	if client.usesV2() && slices.ContainsFunc(allResults, func(r ImportResult) bool { return r.Status == "error" }) {
		os.Exit(1)
	}
}

func printSummary(results []ImportResult) {
	var imported, dupes, errors int
	for _, r := range results {
		count := r.Count
		if count == 0 {
			count = 1
		}
		switch r.Status {
		case "imported":
			imported += count
		case "dupe":
			dupes += count
		case "error":
			errors += count
		}
	}

	fmt.Println("=== Summary ===")
	fmt.Printf("Imported: %d\n", imported)
	if dupes > 0 {
		fmt.Printf("Duplicates skipped: %d\n", dupes)
	}
	if errors > 0 {
		fmt.Printf("Errors: %d\n", errors)
	}

	if dupes > 0 {
		fmt.Println("\nDuplicates:")
		for _, r := range results {
			if r.Status == "dupe" {
				fmt.Printf("  %s\n", importResultLabel(r))
			}
		}
	}
	if errors > 0 {
		fmt.Println("\nErrors:")
		for _, r := range results {
			if r.Status == "error" {
				fmt.Printf("  %s — %v\n", importResultLabel(r), r.Err)
			}
		}
	}
}

func importResultLabel(r ImportResult) string {
	if r.QSO == nil {
		return fmt.Sprintf("%d QSOs (API v2 batch result; individual QSOs not identified)", r.Count)
	}
	return qsoLabel(r.QSO)
}

func qsoLabel(q *QSO) string {
	parts := []string{q.GetField("CALL"), q.GetField("BAND"), q.GetField("MODE"), q.GetField("QSO_DATE")}
	if t := q.GetField("TIME_ON"); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

func expandFiles(files []string) []string {
	var expanded []string
	for _, f := range files {
		matches, err := filepath.Glob(f)
		if err != nil || len(matches) == 0 {
			expanded = append(expanded, f)
		} else {
			expanded = append(expanded, matches...)
		}
	}
	return expanded
}

func parseFile(path string) ([]QSO, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening file: %w", err)
	}
	defer f.Close()
	return ParseADIF(f)
}

func warnUnknownPlaceholders(format string, fields []string) {
	for _, m := range placeholderRe.FindAllString(format, -1) {
		if name := placeholderField(m); !slices.Contains(fields, name) {
			fmt.Fprintf(os.Stderr, "Warning: -name-format placeholder {%s} is not in -match-fields, will be empty\n", name)
		}
	}
}

func parseMatchFields(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(strings.ToUpper(p)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func coalesce(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// normalizeWavelogURL defaults bare hostnames to HTTPS without probing or
// downgrading the connection. Preserve installation paths such as /index.php.
func normalizeWavelogURL(raw string) (string, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("enter a hostname or an http:// or https:// URL (for example https://log.example.com)")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q; use http:// or https://", u.Scheme)
	}
	if u.Hostname() == "" {
		return "", fmt.Errorf("a hostname is required (for example https://log.example.com)")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return "", fmt.Errorf("use the instance base URL without a query string or fragment")
	}
	return u.String(), nil
}
