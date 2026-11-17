package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type WavelogClient struct {
	BaseURL     string
	APIKey      string
	client      *http.Client
	lookupCache map[string]lookupCacheEntry
	v2Scopes    []string
}

type StationProfile struct {
	ID  string
	Raw map[string]string
}

// CreateProfileResult is returned by CreateStationProfile: the newly resolved
// profile id plus the freshly fetched profiles/returnedFields so callers can
// update their cache without a second GetStationProfiles round-trip.
type CreateProfileResult struct {
	ProfileID      string
	Profiles       []StationProfile
	ReturnedFields map[string]bool
}

var KnownFields = map[string]string{
	"STATION_CALLSIGN": "station_callsign",
	"MY_GRIDSQUARE":    "station_gridsquare",
	"MY_DXCC":          "station_dxcc",
	"MY_COUNTRY":       "station_country",
	"MY_CQ_ZONE":       "station_cq",
	"MY_ITU_ZONE":      "station_itu",
	"MY_CITY":          "station_city",
	"MY_IOTA":          "station_iota",
	"MY_SOTA_REF":      "station_sota",
	"MY_POTA_REF":      "station_pota",
	"MY_WWFF_REF":      "station_wwff",
	"MY_SIG":           "station_sig",
	"MY_SIG_INFO":      "station_sig_info",
	"MY_STATE":         "station_state",
	"MY_CNTY":          "station_cnty",
}

type QSOImportResponse struct {
	Status   string           `json:"status"`
	Messages []string         `json:"messages"`
	Summary  *v2ImportSummary `json:"-"`
}

type ImportResult struct {
	QSO    *QSO
	Status string // "imported", "dupe", "error"
	Err    error
	// Count is set for aggregate v2 results, which do not identify individual QSOs.
	Count int
}

func NewWavelogClient(baseURL, apiKey string) *WavelogClient {
	return &WavelogClient{
		BaseURL:     strings.TrimRight(baseURL, "/"),
		APIKey:      apiKey,
		client:      &http.Client{Timeout: 60 * time.Second},
		lookupCache: make(map[string]lookupCacheEntry),
	}
}

func (c *WavelogClient) CheckVersion() (string, error) {
	if c.usesV2() {
		return c.checkV2Token()
	}
	var resp struct {
		Status  string `json:"status"`
		Version string `json:"version"`
	}
	if err := c.post("/api/version", map[string]string{"key": c.APIKey}, &resp); err != nil {
		return "", fmt.Errorf("version check failed: %w", err)
	}
	if resp.Status != "ok" {
		return "", fmt.Errorf("unexpected version response: %s", resp.Status)
	}
	return resp.Version, nil
}

func (c *WavelogClient) GetStationProfiles() ([]StationProfile, map[string]bool, error) {
	if c.usesV2() {
		return c.getV2StationProfiles()
	}
	resp, err := c.client.Get(fmt.Sprintf("%s/api/station_info/%s", c.BaseURL, c.APIKey))
	if err != nil {
		return nil, nil, fmt.Errorf("fetching station profiles: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, nil, fmt.Errorf("station_info returned %d: %s", resp.StatusCode, string(body))
	}

	var raw []map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, nil, fmt.Errorf("decoding station profiles: %w", err)
	}

	returnedFields := make(map[string]bool)
	if len(raw) > 0 {
		for k := range raw[0] {
			returnedFields[strings.ToLower(k)] = true
		}
	}

	profiles := make([]StationProfile, 0, len(raw))
	for _, r := range raw {
		m := make(map[string]string, len(r))
		for k, v := range r {
			switch val := v.(type) {
			case string:
				m[strings.ToLower(k)] = val
			case nil:
				m[strings.ToLower(k)] = ""
			}
		}
		profiles = append(profiles, StationProfile{ID: m["station_id"], Raw: m})
	}
	return profiles, returnedFields, nil
}

func (c *WavelogClient) CreateStationProfile(loc StationLocation, matchFields []string, nameFormat string, lookup bool, ituDefault string) (CreateProfileResult, error) {
	fields := loc.QSOs[0].Fields
	name := loc.DisplayName(matchFields, nameFormat)

	payload := map[string]string{
		"station_profile_name": name,
		"station_active":       "1",
		"link_active_logbook":  "1",
	}
	for adifName, jsonKey := range KnownFields {
		payload[jsonKey] = fields[adifName]
	}

	if lookup && mandatoryStationFieldsMissing(payload) {
		if err := c.enrichMandatoryFields(loc, payload); err != nil {
			return CreateProfileResult{}, err
		}
	}

	// Wavelog accepts an empty station_itu at creation but then fails every QSO
	// import for that profile, so ensure a zone is present. Prefer the ITU zone
	// from private_lookup; if it has none, fall back to -itu-zone or a prompt.
	if payload["station_itu"] == "" && lookup {
		if z := c.lookupItuZone(loc); z != "" {
			payload["station_itu"] = z
			fmt.Printf("  Looked up ITU zone %s\n", z)
		}
	}
	if payload["station_itu"] == "" {
		zone, err := resolveItuZone(name, ituDefault)
		if err != nil {
			return CreateProfileResult{}, err
		}
		payload["station_itu"] = zone
		fmt.Printf("  Applied ITU zone %s\n", zone)
	}
	if c.usesV2() {
		return c.createV2StationProfile(loc, matchFields, payload)
	}

	resp, err := c.postRaw("/api/create_station", []map[string]string{payload})
	if err != nil {
		return CreateProfileResult{}, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var apiResp struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return CreateProfileResult{}, fmt.Errorf("parsing create_station response (%d): %s", resp.StatusCode, string(body))
	}

	switch {
	case resp.StatusCode == http.StatusCreated:
		fmt.Printf("  Created station profile: %s\n", name)
		return c.resolveProfileID(loc, matchFields)
	case resp.StatusCode == http.StatusOK && apiResp.Status == "dupe":
		fmt.Printf("  Station profile already exists (dupe): %s\n", name)
		return c.resolveProfileID(loc, matchFields)
	default:
		return CreateProfileResult{}, fmt.Errorf("create_station returned %d: %s", resp.StatusCode, apiResp.Message)
	}
}

func (c *WavelogClient) resolveProfileID(loc StationLocation, matchFields []string) (CreateProfileResult, error) {
	profiles, returnedFields, err := c.GetStationProfiles()
	if err != nil {
		return CreateProfileResult{}, fmt.Errorf("re-fetching profiles after create: %w", err)
	}
	id, err := MatchProfile(loc, profiles, matchFields, returnedFields)
	if err != nil {
		return CreateProfileResult{}, fmt.Errorf("finding newly created profile: %w", err)
	}
	return CreateProfileResult{ProfileID: id, Profiles: profiles, ReturnedFields: returnedFields}, nil
}

func (c *WavelogClient) postRaw(path string, payload interface{}) (*http.Response, error) {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}
	resp, err := c.client.Post(c.BaseURL+path+"/"+c.APIKey, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", path, err)
	}
	return resp, nil
}

func (c *WavelogClient) ImportQSOs(profileID string, adifString string) (*QSOImportResponse, error) {
	if c.usesV2() {
		return c.importV2QSOs(profileID, adifString)
	}
	jsonData, _ := json.Marshal(map[string]string{
		"key":                c.APIKey,
		"station_profile_id": profileID,
		"type":               "adif",
		"string":             adifString,
	})
	resp, err := c.client.Post(c.BaseURL+"/api/qso", "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return nil, fmt.Errorf("POST /api/qso: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var qsoResp QSOImportResponse
	if err := json.Unmarshal(body, &qsoResp); err != nil {
		return nil, fmt.Errorf("decoding /api/qso response: %s", string(body))
	}
	if resp.StatusCode >= 400 && qsoResp.Status != "abort" {
		return nil, fmt.Errorf("/api/qso returned %d: %s", resp.StatusCode, string(body))
	}
	return &qsoResp, nil
}

const defaultChunkBytes = 1024 * 1024 // 1MB

func transformQSO(raw string, fields map[string]string, gridPrecision int) string {
	if gridPrecision < 6 {
		raw = truncateGridInRaw(raw, fields, gridPrecision)
	}
	raw = transformLoTWFields(raw, fields)
	raw = addDefaultRST(raw, fields)
	return raw
}

func (c *WavelogClient) ImportQSOsChunked(profileID string, qsos []QSO, gridPrecision int, label string) []ImportResult {
	if c.usesV2() {
		return c.importV2QSOsChunked(profileID, qsos, gridPrecision, label)
	}
	total := len(qsos)
	var results []ImportResult
	processed := 0

	for processed < total {
		chunk, chunkBytes := nextChunk(qsos[processed:], defaultChunkBytes)
		chunkEnd := processed + len(chunk)
		adifStr := buildADIFString(chunk, gridPrecision)

		resp, err := c.ImportQSOs(profileID, adifStr)
		if err != nil {
			fmt.Printf("  ERROR chunk %d-%d/%d for %s: %v\n", processed+1, chunkEnd, total, label, err)
			for i := range chunk {
				results = append(results, ImportResult{QSO: &chunk[i], Status: "error", Err: err})
			}
			processed = chunkEnd
			continue
		}

		switch {
		case resp.Status == "ok" || resp.Status == "created":
			for i := range chunk {
				results = append(results, ImportResult{QSO: &chunk[i], Status: "imported"})
			}
			fmt.Printf("  Imported %d/%d QSOs for %s (%d KB)\n", chunkEnd, total, label, chunkBytes/1024)

		case resp.Status == "abort":
			dupeSet := parseDupeMessages(resp.Messages)
			fmt.Printf("  Chunk %d-%d: duplicate found, retrying remaining QSOs...\n", processed+1, chunkEnd)
			for i := range chunk {
				if isDupe(chunk[i], dupeSet) {
					results = append(results, ImportResult{QSO: &chunk[i], Status: "dupe"})
					continue
				}
				singleADIF := transformQSO(chunk[i].Raw, chunk[i].Fields, gridPrecision) + "<eor>"
				singleResp, singleErr := c.ImportQSOs(profileID, singleADIF)
				if singleErr != nil {
					results = append(results, ImportResult{QSO: &chunk[i], Status: "error", Err: singleErr})
					continue
				}
				status := "imported"
				if singleResp.Status == "abort" {
					status = "dupe"
				}
				results = append(results, ImportResult{QSO: &chunk[i], Status: status})
			}

		default:
			chunkErr := fmt.Errorf("unexpected status %q: %s", resp.Status, strings.Join(resp.Messages, "; "))
			fmt.Printf("  ERROR chunk %d-%d/%d for %s: %v\n", processed+1, chunkEnd, total, label, chunkErr)
			for i := range chunk {
				results = append(results, ImportResult{QSO: &chunk[i], Status: "error", Err: chunkErr})
			}
		}
		processed = chunkEnd
	}
	return results
}

func parseDupeMessages(messages []string) map[string]bool {
	dupes := make(map[string]bool)
	for _, raw := range messages {
		for _, msg := range strings.Split(raw, "<br>") {
			msg = strings.TrimSpace(msg)
			if !strings.Contains(msg, "Duplicate") {
				continue
			}
			call := extractMsgField(msg, "Callsign: ")
			band := extractMsgField(msg, "Band: ")
			dateTime := extractMsgField(msg, "Date/Time: ")
			if call == "" || band == "" || dateTime == "" {
				continue
			}
			parts := strings.SplitN(dateTime, " ", 2)
			if len(parts) < 2 {
				continue
			}
			date := strings.ReplaceAll(parts[0], "-", "")
			time := strings.ReplaceAll(parts[1], ":", "")
			dupes[strings.ToUpper(call+"|"+band+"|"+date+"|"+time)] = true
		}
	}
	return dupes
}

var msgFieldEnds = []string{"Callsign: ", "Band: ", "Duplicate"}

func extractMsgField(msg, prefix string) string {
	idx := strings.Index(msg, prefix)
	if idx < 0 {
		return ""
	}
	rest := msg[idx+len(prefix):]
	end := len(rest)
	for _, t := range msgFieldEnds {
		if i := strings.Index(rest, t); i >= 0 && i < end {
			end = i
		}
	}
	return strings.TrimSpace(rest[:end])
}

func isDupe(qso QSO, dupeSet map[string]bool) bool {
	key := strings.ToUpper(qso.GetField("CALL") + "|" + qso.GetField("BAND") + "|" + qso.GetField("QSO_DATE") + "|" + qso.GetField("TIME_ON"))
	return dupeSet[key]
}

func nextChunk(qsos []QSO, maxBytes int) ([]QSO, int) {
	totalBytes := 0
	for i, qso := range qsos {
		qsoBytes := len(qso.Raw) + 5
		if i > 0 && totalBytes+qsoBytes > maxBytes {
			return qsos[:i], totalBytes
		}
		totalBytes += qsoBytes
	}
	return qsos, totalBytes
}

func buildADIFString(qsos []QSO, gridPrecision int) string {
	var sb strings.Builder
	for _, qso := range qsos {
		sb.WriteString(transformQSO(qso.Raw, qso.Fields, gridPrecision))
		sb.WriteString("<eor>")
	}
	return sb.String()
}

func defaultRST(mode string) string {
	switch strings.ToUpper(mode) {
	case "SSB", "USB", "LSB", "FM", "AM":
		return "59"
	case "CW":
		return "599"
	default:
		return "0"
	}
}

func addDefaultRST(raw string, fields map[string]string) string {
	_, hasSent := fields["RST_SENT"]
	_, hasRcvd := fields["RST_RCVD"]
	if hasSent && hasRcvd {
		return raw
	}
	rst := defaultRST(fields["MODE"])
	var sb strings.Builder
	if !hasSent {
		sb.WriteString(fmt.Sprintf("<RST_SENT:%d>%s", len(rst), rst))
	}
	if !hasRcvd {
		sb.WriteString(fmt.Sprintf("<RST_RCVD:%d>%s", len(rst), rst))
	}
	return raw + sb.String()
}

func transformLoTWFields(raw string, fields map[string]string) string {
	_, isLoTW := fields["APP_LOTW_RXQSO"]
	if !isLoTW {
		_, isLoTW = fields["APP_LOTW_RXQSL"]
	}
	if !isLoTW {
		return raw
	}

	raw = removeADIFField(raw, "QSL_RCVD")
	raw = removeADIFField(raw, "QSLRDATE")

	var sb strings.Builder
	if val, ok := fields["APP_LOTW_RXQSO"]; ok {
		raw = removeADIFField(raw, "APP_LoTW_RXQSO")
		if d := extractADIFDate(val); d != "" {
			sb.WriteString(fmt.Sprintf("<LOTW_QSLSDATE:8>%s", d))
		}
		sb.WriteString("<LOTW_QSL_SENT:1>Y")
	}
	if val, ok := fields["APP_LOTW_RXQSL"]; ok {
		raw = removeADIFField(raw, "APP_LoTW_RXQSL")
		if d := extractADIFDate(val); d != "" {
			sb.WriteString(fmt.Sprintf("<LOTW_QSLRDATE:8>%s", d))
		}
		sb.WriteString("<LOTW_QSL_RCVD:1>Y")
	}
	if sb.Len() > 0 {
		return raw + sb.String()
	}
	return raw
}

func extractADIFDate(timestamp string) string {
	if len(timestamp) < 10 {
		return ""
	}
	return strings.ReplaceAll(timestamp[:10], "-", "")
}

func removeADIFField(raw, fieldname string) string {
	upper := strings.ToUpper(raw)
	pattern := "<" + strings.ToUpper(fieldname) + ":"
	for {
		idx := strings.Index(upper, pattern)
		if idx < 0 {
			break
		}
		tagEnd := strings.IndexByte(raw[idx:], '>')
		if tagEnd < 0 {
			break
		}
		tagEnd += idx
		length := 0
		for _, ch := range raw[idx+len(pattern) : tagEnd] {
			if ch >= '0' && ch <= '9' {
				length = length*10 + int(ch-'0')
			} else {
				break
			}
		}
		valEnd := tagEnd + 1 + length
		if valEnd > len(raw) {
			break
		}
		raw = raw[:idx] + raw[valEnd:]
		upper = strings.ToUpper(raw)
	}
	return raw
}

func truncateGridInRaw(raw string, fields map[string]string, precision int) string {
	gridVal := fields["MY_GRIDSQUARE"]
	if gridVal == "" || len(gridVal) <= precision {
		return raw
	}
	upper := strings.ToUpper(raw)
	idx := strings.Index(upper, "<MY_GRIDSQUARE:")
	if idx < 0 {
		return raw
	}
	tagEnd := strings.IndexByte(raw[idx:], '>')
	if tagEnd < 0 {
		return raw
	}
	tagEnd += idx
	valStart := tagEnd + 1
	valEnd := valStart + len(gridVal)
	if valEnd > len(raw) {
		return raw
	}
	return raw[:idx] + fmt.Sprintf("<MY_GRIDSQUARE:%d>%s", precision, strings.ToUpper(gridVal[:precision])) + raw[valEnd:]
}

func (c *WavelogClient) post(path string, payload interface{}, result interface{}) error {
	jsonData, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}
	resp, err := c.client.Post(c.BaseURL+path, "application/json", bytes.NewReader(jsonData))
	if err != nil {
		return fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("%s returned %d: %s", path, resp.StatusCode, string(body))
	}
	if result != nil {
		if err := json.Unmarshal(body, result); err != nil {
			return fmt.Errorf("decoding %s response: %s", path, string(body))
		}
	}
	return nil
}

func MatchProfile(loc StationLocation, profiles []StationProfile, matchFields []string, returnedFields map[string]bool) (string, error) {
	for _, p := range profiles {
		if profileMatches(loc, p, matchFields, returnedFields) {
			return p.ID, nil
		}
	}
	return "", fmt.Errorf("no matching profile for %s", loc.DisplayName(matchFields, ""))
}

func profileMatches(loc StationLocation, p StationProfile, matchFields []string, returnedFields map[string]bool) bool {
	for _, field := range matchFields {
		upper := strings.ToUpper(field)
		jsonKey, known := KnownFields[upper]
		if !known || !returnedFields[jsonKey] {
			continue
		}
		locVal := strings.ToUpper(loc.Match[upper])
		profileVal := strings.ToUpper(p.Raw[jsonKey])
		if isGridField(upper) && len(profileVal) > len(locVal) && locVal != "" {
			profileVal = profileVal[:len(locVal)]
		}
		if locVal != profileVal {
			return false
		}
	}
	return true
}

func WarnUnsupportedFields(matchFields []string, returnedFields map[string]bool) {
	for _, field := range matchFields {
		upper := strings.ToUpper(field)
		jsonKey, known := KnownFields[upper]
		if !known {
			fmt.Fprintf(os.Stderr, "  WARNING: unknown match field %q — no mapping to Wavelog API\n", field)
		} else if !returnedFields[jsonKey] {
			fmt.Fprintf(os.Stderr, "  WARNING: field %q not returned by Wavelog API — skipped for matching\n", field)
		}
	}
}
