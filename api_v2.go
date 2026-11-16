package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// API selection is credential-based; authentication failures never fall back to v1.
func (c *WavelogClient) usesV2() bool {
	return strings.HasPrefix(c.APIKey, "wl2_")
}

type v2Error struct {
	Status  int
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Details json.RawMessage `json:"details"`
}

func (e *v2Error) Error() string {
	return fmt.Sprintf("API v2 returned %d (%s): %s", e.Status, e.Code, e.Message)
}

// requestV2 keeps credentials out of URLs and refuses redirects, including those
// to other origins. Error bodies are not printed wholesale: they may contain secrets.
func (c *WavelogClient) requestV2(method, path string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encoding API v2 request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, c.BaseURL+"/api/v2/"+path, body)
	if err != nil {
		return fmt.Errorf("building API v2 request: %s", c.redactV2(err.Error()))
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := *c.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("API v2 request failed: %s", c.redactV2(err.Error()))
	}
	defer resp.Body.Close()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error *v2Error        `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("API v2 returned HTTP %d with an invalid JSON envelope", resp.StatusCode)
	}
	if envelope.Error != nil {
		envelope.Error.Status = resp.StatusCode
		envelope.Error.Message = c.redactV2(envelope.Error.Message)
		envelope.Error.Code = c.redactV2(envelope.Error.Code)
		// Details are retained for classifying validation failures, never logged verbatim.
		return envelope.Error
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("API v2 returned HTTP %d", resp.StatusCode)
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("API v2 response is missing data")
	}
	if result != nil {
		if err := json.Unmarshal(envelope.Data, result); err != nil {
			return fmt.Errorf("decoding API v2 data: %s", c.redactV2(err.Error()))
		}
	}
	return nil
}

func (c *WavelogClient) redactV2(s string) string {
	s = strings.ReplaceAll(s, c.APIKey, "[redacted]")
	return strings.ReplaceAll(s, url.QueryEscape(c.APIKey), "[redacted]")
}

func (c *WavelogClient) checkV2Token() (string, error) {
	var token struct {
		ID     int      `json:"id"`
		Scopes []string `json:"scopes"`
	}
	if err := c.requestV2(http.MethodGet, "token", nil, &token); err != nil {
		return "", fmt.Errorf("checking API v2 token: %w", err)
	}
	if token.ID <= 0 || token.Scopes == nil {
		return "", fmt.Errorf("API v2 returned invalid token metadata")
	}
	c.v2Scopes = token.Scopes
	return "API v2", nil
}

func (c *WavelogClient) checkV2Scopes(lookup bool) error {
	required := []string{"station:read", "station:write", "qso:write", "logbook:read", "logbook:write"}
	if lookup {
		required = append(required, "lookup:read")
	}
	var missing []string
	for _, scope := range required {
		if !slices.Contains(c.v2Scopes, scope) {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("API v2 token is missing required scopes: %s", strings.Join(missing, ", "))
	}
	return nil
}

// These field translations preserve the importer's existing matching representation.
var v2StationFields = map[string]string{
	"id": "station_id", "uuid": "station_uuid", "name": "station_profile_name",
	"callsign": "station_callsign", "gridsquare": "station_gridsquare",
	"city": "station_city", "dxcc": "station_dxcc", "country": "station_country",
	"cq": "station_cq", "itu": "station_itu", "state": "station_state",
	"cnty": "station_cnty", "iota": "station_iota", "sota": "station_sota",
	"wwff": "station_wwff", "pota": "station_pota", "sig": "station_sig",
	"sig_info": "station_sig_info",
}

func v2Station(raw map[string]json.RawMessage) (StationProfile, map[string]bool, error) {
	profile := StationProfile{Raw: make(map[string]string)}
	fields := make(map[string]bool)
	for source, target := range v2StationFields {
		value, ok := raw[source]
		if !ok {
			continue
		}
		fields[target] = true
		var scalar any
		decoder := json.NewDecoder(bytes.NewReader(value))
		decoder.UseNumber()
		if err := decoder.Decode(&scalar); err != nil {
			return profile, nil, fmt.Errorf("invalid API v2 station field %s", source)
		}
		switch v := scalar.(type) {
		case nil:
			profile.Raw[target] = ""
		case string:
			profile.Raw[target] = v
		case json.Number:
			profile.Raw[target] = string(v)
		default:
			return profile, nil, fmt.Errorf("invalid API v2 station field %s", source)
		}
	}
	profile.ID = profile.Raw["station_id"]
	if id, err := strconv.Atoi(profile.ID); err != nil || id <= 0 {
		return profile, nil, fmt.Errorf("API v2 station is missing a valid id")
	}
	return profile, fields, nil
}

func (c *WavelogClient) getV2StationProfiles() ([]StationProfile, map[string]bool, error) {
	var raw []map[string]json.RawMessage
	if err := c.requestV2(http.MethodGet, "station", nil, &raw); err != nil {
		return nil, nil, err
	}
	profiles := make([]StationProfile, 0, len(raw))
	fields := make(map[string]bool)
	for _, row := range raw {
		profile, returned, err := v2Station(row)
		if err != nil {
			return nil, nil, err
		}
		if len(profiles) == 0 {
			fields = returned
		}
		profiles = append(profiles, profile)
	}
	return profiles, fields, nil
}

type v2Logbook struct {
	ID         int   `json:"id"`
	Active     bool  `json:"active"`
	StationIDs []int `json:"station_ids"`
}

func (c *WavelogClient) createV2StationProfile(loc StationLocation, matchFields []string, legacy map[string]string) (CreateProfileResult, error) {
	// Resolve the active logbook before creating a location, so a permission
	// failure cannot leave an unlinked profile behind without an explanation.
	var logbooks []v2Logbook
	if err := c.requestV2(http.MethodGet, "logbook", nil, &logbooks); err != nil {
		return CreateProfileResult{}, err
	}
	activeID := 0
	for _, book := range logbooks {
		if book.Active {
			activeID = book.ID
			break
		}
	}
	payload := make(map[string]any)
	for field, old := range v2StationFields {
		if field != "id" && field != "uuid" && field != "country" {
			if value := legacy[old]; value != "" {
				payload[field] = value
			}
		}
	}
	// Mirror legacy creation defaults: no inherited third-party credentials/uploads.
	payload["clublog_upload"] = "disabled"
	var row map[string]json.RawMessage
	if err := c.requestV2(http.MethodPost, "station", payload, &row); err != nil {
		var apiErr *v2Error
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusConflict {
			return c.resolveProfileID(loc, matchFields)
		}
		return CreateProfileResult{}, err
	}
	profile, fields, err := v2Station(row)
	if err != nil {
		return CreateProfileResult{}, err
	}
	if activeID > 0 {
		// PATCH replaces the list. Re-read immediately before appending so we
		// preserve existing links (the API has no atomic append operation).
		path := "logbook/" + strconv.Itoa(activeID)
		var book v2Logbook
		if err := c.requestV2(http.MethodGet, path, nil, &book); err != nil {
			return CreateProfileResult{}, fmt.Errorf("station #%s created, but linking failed: %w", profile.ID, err)
		}
		id, _ := strconv.Atoi(profile.ID)
		if !slices.Contains(book.StationIDs, id) {
			ids := append(book.StationIDs, id)
			if err := c.requestV2(http.MethodPatch, path, map[string]any{"station_ids": ids}, &book); err != nil {
				return CreateProfileResult{}, fmt.Errorf("station #%s created, but linking failed: %w", profile.ID, err)
			}
		}
	}
	profiles, returned, err := c.getV2StationProfiles()
	if err != nil {
		return CreateProfileResult{}, fmt.Errorf("station #%s created, but re-fetching profiles failed: %w", profile.ID, err)
	}
	if len(profiles) == 0 {
		returned = fields
	}
	if !slices.ContainsFunc(profiles, func(p StationProfile) bool { return p.ID == profile.ID }) {
		profiles = append(profiles, profile)
	}
	fmt.Printf("  Created station profile #%s: %s\n", profile.ID, legacy["station_profile_name"])
	return CreateProfileResult{ProfileID: profile.ID, Profiles: profiles, ReturnedFields: returned}, nil
}

func (c *WavelogClient) lookupV2Callsign(callsign string) (CallsignLookup, error) {
	var raw map[string]json.RawMessage
	// Full detail includes ITU data when supported by the Wavelog version.
	err := c.requestV2(http.MethodGet, "lookup?"+url.Values{"callsign": {callsign}, "detail": {"full"}}.Encode(), nil, &raw)
	if err != nil {
		return CallsignLookup{}, err
	}
	values := make(map[string]string)
	for _, field := range []string{"dxcc", "dxcc_id", "dxcc_cqz", "dxcc_ituz"} {
		value := raw[field]
		if len(value) == 0 || string(value) == "null" || string(value) == "false" {
			continue
		}
		var text string
		if json.Unmarshal(value, &text) == nil {
			values[field] = text
			continue
		}
		var number json.Number
		if json.Unmarshal(value, &number) != nil {
			return CallsignLookup{}, fmt.Errorf("invalid API v2 lookup field %s", field)
		}
		values[field] = string(number)
	}
	lookup := CallsignLookup{DXCC: values["dxcc"], DXCCID: values["dxcc_id"], CQZ: values["dxcc_cqz"], ItuZ: json.Number(values["dxcc_ituz"])}
	if lookup.DXCCID == "-1" || lookup.DXCCID == "0" {
		lookup.DXCCID = ""
	}
	return lookup, nil
}

type v2ImportSummary struct {
	Parsed   *int     `json:"parsed"`
	Imported *int     `json:"imported"`
	Skipped  *int     `json:"skipped"`
	Messages []string `json:"messages"`
}

func (c *WavelogClient) importV2QSOs(profileID, adif string) (*QSOImportResponse, error) {
	id, err := strconv.Atoi(profileID)
	if err != nil || id <= 0 {
		return nil, fmt.Errorf("invalid station profile id %q", profileID)
	}
	var summary v2ImportSummary
	err = c.requestV2(http.MethodPost, "qso", map[string]any{
		"station_profile_id": id, "import_type": "adif", "adif": adif,
	}, &summary)
	if err != nil {
		return nil, err
	}
	return &QSOImportResponse{Status: "created", Summary: &summary}, nil
}

// V2 reports counts, not per-QSO identities, and may already have saved part of a
// batch. Account for aggregates directly and never feed it into the v1 retry loop.
func (c *WavelogClient) importV2QSOsChunked(profileID string, qsos []QSO, precision int, label string) []ImportResult {
	var results []ImportResult
	for len(qsos) > 0 {
		chunk, _ := nextChunk(qsos, defaultChunkBytes)
		response, err := c.importV2QSOs(profileID, buildADIFString(chunk, precision))
		imported, skipped := 0, 0
		if err == nil {
			s := response.Summary
			if s.Parsed == nil || s.Imported == nil || s.Skipped == nil || *s.Parsed < 0 || *s.Parsed > len(chunk) ||
				*s.Imported < 0 || *s.Skipped < 0 || *s.Imported > *s.Parsed || *s.Skipped > *s.Parsed-*s.Imported ||
				(len(s.Messages) > 0 && *s.Imported+*s.Skipped == len(chunk)) {
				err = fmt.Errorf("invalid API v2 import counts; batch may have been partially saved; not retried")
			} else {
				imported, skipped = *s.Imported, *s.Skipped
				if len(s.Messages) > 0 {
					err = fmt.Errorf("%s", c.redactV2(strings.Join(s.Messages, "; ")))
				}
			}
		} else {
			var apiErr *v2Error
			if errors.As(err, &apiErr) && apiErr.Status == 400 && apiErr.Code == "validation_error" {
				var details struct {
					Duplicate []string `json:"duplicate"`
				}
				if json.Unmarshal(apiErr.Details, &details) == nil && len(details.Duplicate) < len(chunk) {
					skipped = len(details.Duplicate)
				}
			}
		}
		if imported > 0 {
			results = append(results, ImportResult{Status: "imported", Count: imported})
		}
		if skipped > 0 {
			results = append(results, ImportResult{Status: "dupe", Count: skipped})
		}
		if failed := len(chunk) - imported - skipped; failed > 0 {
			if err == nil {
				err = fmt.Errorf("API v2 did not report these QSOs as imported or duplicate")
			}
			results = append(results, ImportResult{Status: "error", Count: failed, Err: err})
		}
		fmt.Printf("  %s: imported %d, duplicates %d, errors %d (API v2)\n", label, imported, skipped, len(chunk)-imported-skipped)
		qsos = qsos[len(chunk):]
	}
	return results
}
