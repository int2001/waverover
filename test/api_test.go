package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

const v2Key = "wl2_test-secret"
const legacyKey = "wl_legacy-test"

var scopes = []string{"station:read", "station:write", "qso:write", "logbook:read", "logbook:write", "lookup:read"}
var binary string

// Build the real CLI once. Tests use only its public command-line/HTTP contract.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "waverover-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "waveRover")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-race", "-o", binary, "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build CLI: %v\n%s", err, output)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func run(t *testing.T, serverURL, key string, records []map[string]string, flags ...string) (string, error) {
	t.Helper()
	return runWithEnv(t, serverURL, key, records, nil, flags...)
}

func runWithEnv(t *testing.T, serverURL, key string, records []map[string]string, env []string, flags ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "activation.adi")
	var adif strings.Builder
	for _, record := range records {
		for field, value := range record {
			fmt.Fprintf(&adif, "<%s:%d>%s", field, len(value), value)
		}
		adif.WriteString("<EOR>\n")
	}
	if err := os.WriteFile(path, []byte(adif.String()), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-key", key}
	if serverURL != "" {
		args = append(args, "-url", serverURL)
	}
	args = append(args, flags...)
	args = append(args, path)
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "WAVELOG_URL=", "WAVELOG_API_KEY=")
	cmd.Env = append(cmd.Env, env...)
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func records(n int) []map[string]string {
	var records []map[string]string
	for i := 0; i < n; i++ {
		records = append(records, map[string]string{
			"CALL": fmt.Sprintf("DL%dABC", i+1), "STATION_CALLSIGN": "DL1ABC/P", "MY_GRIDSQUARE": "JO30OO",
			"MODE": "CW", "BAND": "20m", "QSO_DATE": "20261116", "TIME_ON": "120000",
		})
	}
	return records
}

func writeV2(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func readBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Errorf("decode request: %v", err)
	}
	if _, ok := body["key"]; ok {
		t.Error("v2 key sent in body")
	}
	return body
}

func v2Server(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+v2Key {
			t.Errorf("missing bearer header: %q", r.Header.Get("Authorization"))
		}
		if strings.Contains(r.URL.String(), v2Key) || !strings.HasPrefix(r.URL.Path, "/index.php/api/v2/") {
			t.Errorf("unexpected URL %s", r.URL)
		}
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing JSON Accept")
		}
		if r.Method == "POST" || r.Method == "PATCH" {
			if r.Header.Get("Content-Type") != "application/json" {
				t.Error("missing JSON Content-Type")
			}
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	return server
}

func token(w http.ResponseWriter) { writeV2(w, 200, map[string]any{"id": 1, "scopes": scopes}) }
func station() map[string]any {
	return map[string]any{"id": 42, "name": "Summit", "callsign": "DL1ABC/P", "gridsquare": "JO30OO", "sota": "DM/RP-001", "pota": nil, "dxcc": 230, "cq": 14, "itu": 28}
}
func summary(w http.ResponseWriter, n int) {
	writeV2(w, 201, map[string]any{"parsed": n, "imported": n, "skipped": 0, "messages": []string{}})
}

func TestV2ExistingStationMatching(t *testing.T) {
	calls := 0
	server := v2Server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch r.Method + " " + r.URL.Path {
		case "GET /index.php/api/v2/token":
			token(w)
		case "GET /index.php/api/v2/station":
			wrong := station()
			wrong["id"] = 7
			wrong["pota"] = "DE-0001"
			writeV2(w, 200, []any{wrong, station()})
		case "POST /index.php/api/v2/qso":
			body := readBody(t, r)
			if body["station_profile_id"] != float64(42) || body["import_type"] != "adif" {
				t.Errorf("wrong matched profile or format: %v", body)
			}
			if strings.Count(body["adif"].(string), "<eor>") != 1 {
				t.Error("wrong ADIF")
			}
			summary(w, 1)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.Error(w, "unexpected", 500)
		}
	})
	qsos := records(1)
	qsos[0]["MY_SOTA_REF"] = "DM/RP-001"
	qsos[0]["MY_DXCC"] = "230"
	output, err := run(t, server.URL+"/index.php/", v2Key, qsos, "-match-fields", "STATION_CALLSIGN,MY_GRIDSQUARE,MY_SOTA_REF,MY_POTA_REF,MY_DXCC")
	if err != nil || calls != 3 || !strings.Contains(output, "Imported: 1") || !strings.Contains(output, "Matched existing profile #42") {
		t.Fatalf("calls=%d err=%v\n%s", calls, err, output)
	}
}

func TestV2CreateLookupAndLinkage(t *testing.T) {
	for _, scenario := range []string{"created", "no-active-logbook", "conflict", "link-failed", "logbook-denied"} {
		t.Run(scenario, func(t *testing.T) {
			listed := 0
			created := false
			imported := false
			server := v2Server(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /index.php/api/v2/token":
					token(w)
				case "GET /index.php/api/v2/station":
					listed++
					if listed == 1 {
						writeV2(w, 200, []any{})
					} else {
						writeV2(w, 200, []any{station()})
					}
				case "GET /index.php/api/v2/lookup":
					if r.URL.Query().Get("callsign") != "DL1ABC/P" || r.URL.Query().Get("detail") != "full" {
						t.Errorf("query=%s", r.URL)
					}
					// API fields may be strings or numbers; country is read-only on station creation.
					writeV2(w, 200, map[string]any{"dxcc": "GERMANY", "dxcc_id": 230, "dxcc_cqz": "14", "dxcc_ituz": 28})
				case "GET /index.php/api/v2/logbook":
					if scenario == "logbook-denied" {
						w.WriteHeader(403)
						fmt.Fprint(w, `{"error":{"code":"insufficient_scope","message":"denied"}}`)
						return
					}
					if scenario == "no-active-logbook" {
						writeV2(w, 200, []any{})
						return
					}
					writeV2(w, 200, []any{map[string]any{"id": 7, "active": true, "station_ids": []int{1}}})
				case "POST /index.php/api/v2/station":
					created = true
					body := readBody(t, r)
					for field, want := range map[string]any{"name": "DL1ABC/P @ JO30OO DM/RP-001", "callsign": "DL1ABC/P", "gridsquare": "JO30OO", "dxcc": "230", "cq": "14", "itu": "28", "state": "RP", "cnty": "County", "sota": "DM/RP-001", "clublog_upload": "disabled"} {
						if body[field] != want {
							t.Errorf("%s=%v want %v", field, body[field], want)
						}
					}
					if _, ok := body["station_active"]; ok {
						t.Error("legacy field sent")
					}
					if scenario == "conflict" {
						w.WriteHeader(409)
						fmt.Fprint(w, `{"error":{"code":"conflict","message":"exists"}}`)
						return
					}
					writeV2(w, 201, station())
				case "GET /index.php/api/v2/logbook/7":
					if scenario == "link-failed" {
						w.WriteHeader(500)
						fmt.Fprint(w, `{"error":{"code":"internal_error","message":"failed"}}`)
						return
					}
					writeV2(w, 200, map[string]any{"id": 7, "active": true, "station_ids": []int{1, 9}})
				case "PATCH /index.php/api/v2/logbook/7":
					body := readBody(t, r)
					if !reflect.DeepEqual(body["station_ids"], []any{float64(1), float64(9), float64(42)}) {
						t.Errorf("links=%v", body)
					}
					writeV2(w, 200, map[string]any{"id": 7, "station_ids": []int{1, 9, 42}})
				case "POST /index.php/api/v2/qso":
					imported = true
					summary(w, 1)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 500)
				}
			})
			qsos := records(1)
			qsos[0]["MY_SOTA_REF"] = "DM/RP-001"
			qsos[0]["MY_STATE"] = "RP"
			qsos[0]["MY_CNTY"] = "County"
			output, err := run(t, server.URL+"/index.php", v2Key, qsos, "-match-fields", "STATION_CALLSIGN,MY_GRIDSQUARE,MY_SOTA_REF")
			failed := scenario == "link-failed" || scenario == "logbook-denied"
			if failed {
				if err == nil || imported {
					t.Fatalf("err=%v imported=%v\n%s", err, imported, output)
				}
				if scenario == "logbook-denied" && created {
					t.Fatal("created before checking logbook access")
				}
				if scenario == "link-failed" && !strings.Contains(output, "station #42 created, but linking failed") {
					t.Fatal(output)
				}
			} else if err != nil || !imported || !strings.Contains(output, "Imported: 1") {
				t.Fatalf("err=%v imported=%v\n%s", err, imported, output)
			}
		})
	}
}

func TestV2CredentialsAndScopes(t *testing.T) {
	for _, tc := range []struct {
		name, body              string
		status                  int
		lookupDisabled, success bool
	}{
		{"revoked", `{"error":{"code":"invalid_token","message":"rejected wl2_test-secret"}}`, 401, false, false},
		{"expired", `{"error":{"code":"token_expired","message":"expired"}}`, 401, false, false},
		{"forbidden", `{"error":{"code":"insufficient_scope","message":"denied"}}`, 403, false, false},
		{"rate-limit", `{"error":{"code":"rate_limited","message":"slow down"}}`, 429, false, false},
		{"missing-scopes", `{"data":{"id":1,"scopes":["station:read"]}}`, 200, false, false},
		{"without-lookup", `{"data":{"id":1,"scopes":["station:read","station:write","qso:write","logbook:read","logbook:write"]}}`, 200, true, true},
		{"requires-lookup", `{"data":{"id":1,"scopes":["station:read","station:write","qso:write","logbook:read","logbook:write"]}}`, 200, false, false},
		{"invalid-json", "<html>Login</html>", 200, false, false},
		{"missing-data", `{}`, 200, false, false},
		{"null-data", `{"data":null}`, 200, false, false},
		{"invalid-metadata", `{"data":{"id":0,"scopes":[]}}`, 200, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := v2Server(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch r.URL.Path {
				case "/index.php/api/v2/token":
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				case "/index.php/api/v2/station":
					writeV2(w, 200, []any{station()})
				case "/index.php/api/v2/qso":
					summary(w, 1)
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			})
			var flags []string
			if tc.lookupDisabled {
				flags = append(flags, "-lookup=false")
			}
			output, err := run(t, server.URL+"/index.php", v2Key, records(1), flags...)
			if tc.success {
				if err != nil || calls != 3 {
					t.Fatalf("calls=%d err=%v\n%s", calls, err, output)
				}
			} else if err == nil || calls != 1 {
				t.Fatalf("calls=%d err=%v\n%s", calls, err, output)
			}
			if strings.Contains(output, v2Key) {
				t.Fatal("credential leaked in output")
			}
		})
	}
}

func TestV2RedirectDoesNotForwardBearer(t *testing.T) {
	var forwarded atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1) }))
	defer target.Close()
	server := v2Server(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) })
	output, err := run(t, server.URL+"/index.php", v2Key, records(1))
	if err == nil || forwarded.Load() != 0 {
		t.Fatalf("forwarded=%d err=%v\n%s", forwarded.Load(), err, output)
	}
}

func TestV2BatchCountsAndNoReplay(t *testing.T) {
	for _, tc := range []struct {
		name, body                      string
		status, imported, dupes, failed int
	}{
		{"success", `{"data":{"parsed":3,"imported":3,"skipped":0,"messages":[]}}`, 201, 3, 0, 0},
		{"duplicates", `{"data":{"parsed":3,"imported":0,"skipped":3,"messages":[]}}`, 201, 0, 3, 0},
		{"partial", `{"data":{"parsed":3,"imported":1,"skipped":1,"messages":["invalid mode"]}}`, 201, 1, 1, 1},
		{"rejected", `{"error":{"code":"validation_error","message":"bad","details":{"duplicate":["dupe"],"validation":["bad","bad"]}}}`, 400, 0, 1, 2},
		{"missing-counts", `{"data":{}}`, 201, 0, 0, 3},
		{"impossible-counts", `{"data":{"parsed":3,"imported":4,"skipped":0}}`, 201, 0, 0, 3},
		{"contradictory-messages", `{"data":{"parsed":3,"imported":3,"skipped":0,"messages":["failed"]}}`, 201, 0, 0, 3},
		{"impossible-error-duplicates", `{"error":{"code":"validation_error","message":"bad","details":{"duplicate":["one","two","three"],"validation":["bad"]}}}`, 400, 0, 0, 3},
		{"unparsed-record", `{"data":{"parsed":2,"imported":2,"skipped":0}}`, 201, 2, 0, 1},
		{"server-failure", `{"error":{"code":"internal_error","message":"failed"}}`, 500, 0, 0, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			server := v2Server(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.Method + " " + r.URL.Path {
				case "GET /index.php/api/v2/token":
					token(w)
				case "GET /index.php/api/v2/station":
					writeV2(w, 200, []any{station()})
				case "POST /index.php/api/v2/qso":
					writes++
					body := readBody(t, r)
					if body["station_profile_id"] != float64(42) || body["import_type"] != "adif" || strings.Count(body["adif"].(string), "<eor>") != 3 {
						t.Errorf("payload=%v", body)
					}
					w.WriteHeader(tc.status)
					fmt.Fprint(w, tc.body)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
			})
			output, err := run(t, server.URL+"/index.php", v2Key, records(3))
			if writes != 1 {
				t.Fatalf("batch replayed %d times", writes)
			}
			if (tc.failed > 0) != (err != nil) {
				t.Fatalf("wrong exit: %v\n%s", err, output)
			}
			for label, count := range map[string]int{"Imported": tc.imported, "Duplicates skipped": tc.dupes, "Errors": tc.failed} {
				if count > 0 || label == "Imported" {
					if !strings.Contains(output, fmt.Sprintf("%s: %d\n", label, count)) {
						t.Fatalf("missing count %s=%d\n%s", label, count, output)
					}
				}
			}
			if tc.dupes > 0 && !strings.Contains(output, "individual QSOs not identified") {
				t.Fatal("invented per-QSO results")
			}
		})
	}
}

func TestLegacyCLIAndDuplicateRetry(t *testing.T) {
	for _, duplicates := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicates), func(t *testing.T) {
			writes := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" || strings.Contains(r.URL.Path, "/v2/") {
					t.Error("legacy request changed to v2")
				}
				switch r.Method + " " + r.URL.Path {
				case "POST /api/version":
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["key"] != legacyKey {
						t.Error("missing legacy key")
					}
					fmt.Fprint(w, `{"status":"ok","version":"2.4.3"}`)
				case "GET /api/station_info/" + legacyKey:
					fmt.Fprint(w, `[{"station_id":"42","station_callsign":"DL1ABC/P","station_gridsquare":"JO30OO"}]`)
				case "POST /api/qso":
					writes++
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["key"] != legacyKey || body["station_profile_id"] != "42" || body["type"] != "adif" {
						t.Errorf("payload=%v", body)
					}
					if duplicates && writes == 1 {
						w.WriteHeader(400)
						fmt.Fprint(w, `{"status":"abort","messages":["Date/Time: 2026-11-16 12:00:00 Callsign: DL1ABC Band: 20m Duplicate for DL1ABC/P<br>"]}`)
						return
					}
					if duplicates && strings.Contains(body["string"], "<CALL:6>DL1ABC") {
						t.Error("duplicate was retried")
					}
					fmt.Fprint(w, `{"status":"ok","messages":[]}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.Error(w, "unexpected", 500)
				}
			}))
			defer server.Close()
			output, err := run(t, server.URL, legacyKey, records(2))
			wantWrites, wantImported := 1, 2
			if duplicates {
				wantWrites, wantImported = 2, 1
			}
			if err != nil || writes != wantWrites || !strings.Contains(output, fmt.Sprintf("Imported: %d\n", wantImported)) || !strings.Contains(output, "Connected to Wavelog v2.4.3") {
				t.Fatalf("writes=%d err=%v\n%s", writes, err, output)
			}
			if duplicates && !strings.Contains(output, "Duplicates skipped: 1") {
				t.Fatal(output)
			}
		})
	}
}

func TestLegacyCreateAndLookup(t *testing.T) {
	listed := 0
	created := false
	lookups := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			fmt.Fprint(w, `{"status":"ok","version":"2.4.3"}`)
		case "/api/station_info/" + legacyKey:
			listed++
			if listed == 1 {
				fmt.Fprint(w, `[]`)
			} else {
				fmt.Fprint(w, `[{"station_id":"42","station_callsign":"DL1ABC/P","station_gridsquare":"JO30OO"}]`)
			}
		case "/api/private_lookup":
			lookups++
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["key"] != legacyKey || body["callsign"] != "DL1ABC/P" {
				t.Errorf("lookup=%v", body)
			}
			fmt.Fprint(w, `{"dxcc":"GERMANY","dxcc_id":"230","dxcc_cqz":"14","dxcc_ituz":28}`)
		case "/api/create_station/" + legacyKey:
			created = true
			var body []map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
				return
			}
			for field, want := range map[string]string{"station_callsign": "DL1ABC/P", "station_gridsquare": "JO30OO", "station_dxcc": "230", "station_cq": "14", "station_itu": "28", "link_active_logbook": "1"} {
				if body[0][field] != want {
					t.Errorf("%s=%q want %q", field, body[0][field], want)
				}
			}
			w.WriteHeader(201)
			fmt.Fprint(w, `{"status":"success","message":"1 locations imported."}`)
		case "/api/qso":
			fmt.Fprint(w, `{"status":"ok","messages":[]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	}))
	defer server.Close()
	output, err := run(t, server.URL, legacyKey, records(1))
	if err != nil || !created || lookups != 1 || !strings.Contains(output, "Imported: 1") {
		t.Fatalf("created=%v lookups=%d err=%v\n%s", created, lookups, err, output)
	}
}

func TestDryRunMakesNoAPIRequests(t *testing.T) {
	for _, key := range []string{legacyKey, v2Key} {
		t.Run(key, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "unexpected", 500) }))
			defer server.Close()
			output, err := run(t, server.URL, key, records(1), "-dry-run")
			if err != nil || calls != 0 || !strings.Contains(output, "Dry run — no changes made") {
				t.Fatalf("calls=%d err=%v\n%s", calls, err, output)
			}
		})
	}
}

func TestV2ChunkBoundaries(t *testing.T) {
	writes := 0
	server := v2Server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/index.php/api/v2/token":
			token(w)
		case "/index.php/api/v2/station":
			writeV2(w, 200, []any{station()})
		case "/index.php/api/v2/qso":
			writes++
			body := readBody(t, r)
			adif := body["adif"].(string)
			if strings.Count(adif, "<eor>") != 1 {
				t.Error("batch exceeded expected chunk boundary")
			}
			if !strings.Contains(adif, "<RST_SENT:3>599") {
				t.Error("existing QSO transforms were bypassed")
			}
			summary(w, 1)
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	})
	qsos := records(2)
	for _, qso := range qsos {
		// Keep physical lines within the existing ADIF parser's line buffer.
		qso["COMMENT"] = strings.Repeat(strings.Repeat("x", 1023)+"\n", 600)
	}
	output, err := run(t, server.URL+"/index.php", v2Key, qsos)
	if err != nil || writes != 2 || !strings.Contains(output, "Imported: 2\n") {
		t.Fatalf("writes=%d err=%v\n%s", writes, err, output)
	}
}

func TestV2UnresolvableCallsignIsCachedWithoutWrites(t *testing.T) {
	lookups := 0
	server := v2Server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("wrote an unresolved station")
		}
		switch r.URL.Path {
		case "/index.php/api/v2/token":
			token(w)
		case "/index.php/api/v2/station":
			writeV2(w, 200, []any{})
		case "/index.php/api/v2/lookup":
			lookups++
			writeV2(w, 200, map[string]any{"dxcc": false, "dxcc_id": -1})
		default:
			t.Errorf("unexpected request %s", r.URL)
		}
	})
	qsos := records(2)
	qsos[1]["MY_GRIDSQUARE"] = "JO31OO"
	output, err := run(t, server.URL+"/index.php", v2Key, qsos)
	if err == nil || lookups != 1 || !strings.Contains(output, "Errors: 2\n") {
		t.Fatalf("lookups=%d err=%v\n%s", lookups, err, output)
	}
}
