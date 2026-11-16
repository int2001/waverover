package integration

import (
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestURLSchemesAndInstallationPaths(t *testing.T) {
	for _, mode := range []string{"http", "https", "bare-hostname"} {
		for _, key := range []string{legacyKey, v2Key} {
			for _, source := range []string{"flag", "environment"} {
				t.Run(mode+"/"+key+"/"+source, func(t *testing.T) {
					if mode != "http" && (runtime.GOOS == "windows" || runtime.GOOS == "darwin") {
						t.Skip("SSL_CERT_FILE test trust injection is only supported with Unix file-based certificate roots")
					}
					var requests atomic.Int32
					// Include a subdirectory and index.php, ensuring neither is dropped.
					const prefix = "/logs/index.php"
					handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						requests.Add(1)
						if !strings.HasPrefix(r.URL.Path, prefix+"/api/") {
							t.Errorf("installation path lost: %s", r.URL)
						}
						if key == v2Key {
							if r.Header.Get("Authorization") != "Bearer "+key {
								t.Error("missing v2 authorization")
							}
							switch strings.TrimPrefix(r.URL.Path, prefix) {
							case "/api/v2/token":
								token(w)
							case "/api/v2/station":
								writeV2(w, 200, []any{station()})
							case "/api/v2/qso":
								summary(w, 1)
							default:
								t.Errorf("unexpected v2 path %s", r.URL)
							}
						} else {
							switch strings.TrimPrefix(r.URL.Path, prefix) {
							case "/api/version":
								fmt.Fprint(w, `{"status":"ok","version":"2.4.3"}`)
							case "/api/station_info/" + legacyKey:
								fmt.Fprint(w, `[{"station_id":"42","station_callsign":"DL1ABC/P","station_gridsquare":"JO30OO"}]`)
							case "/api/qso":
								fmt.Fprint(w, `{"status":"ok","messages":[]}`)
							default:
								t.Errorf("unexpected legacy path %s", r.URL)
							}
						}
					})
					var server *httptest.Server
					var env []string
					if mode == "http" {
						server = httptest.NewServer(handler)
					} else {
						server = httptest.NewTLSServer(handler)
						certPath := filepath.Join(t.TempDir(), "test-ca.pem")
						cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
						if err := os.WriteFile(certPath, cert, 0600); err != nil {
							t.Fatal(err)
						}
						env = append(env, "SSL_CERT_FILE="+certPath)
					}
					defer server.Close()
					input := server.URL + prefix + "/"
					if mode == "bare-hostname" {
						input = strings.TrimPrefix(input, "https://")
					}
					if source == "environment" {
						env = append(env, "WAVELOG_URL="+input)
						input = ""
					} else {
						// A flag continues to override a malformed environment value.
						env = append(env, "WAVELOG_URL=ftp://invalid.example")
					}
					output, err := runWithEnv(t, input, key, records(1), env)
					if err != nil || requests.Load() != 3 || !strings.Contains(output, "Imported: 1\n") {
						t.Fatalf("requests=%d err=%v\n%s", requests.Load(), err, output)
					}
				})
			}
		}
	}
}

func TestURLRejectsInvalidBasesBeforeParsing(t *testing.T) {
	for _, input := range []string{"https://", "http:///logs", "ftp://log.example.com", "https://:8080", "https://a b", "localhost:bad-port", "https://log.example.com?key=secret", "https://log.example.com#fragment"} {
		for _, key := range []string{legacyKey, v2Key} {
			for _, source := range []string{"flag", "environment"} {
				t.Run(input+"/"+key+"/"+source, func(t *testing.T) {
					var env []string
					flagURL := input
					if source == "environment" {
						flagURL = ""
						env = []string{"WAVELOG_URL=" + input}
					}
					output, err := runWithEnv(t, flagURL, key, records(1), env, "-dry-run")
					if err == nil || !strings.Contains(output, "Invalid Wavelog URL:") || strings.Contains(output, "Parsing ") || strings.Contains(output, "secret") {
						t.Fatalf("err=%v\n%s", err, output)
					}
				})
			}
		}
	}
}

func TestURLHTTPSFailureNeverFallsBackToHTTP(t *testing.T) {
	for _, key := range []string{legacyKey, v2Key} {
		t.Run(key, func(t *testing.T) {
			var httpRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpRequests.Add(1)
			}))
			defer server.Close()
			bare := strings.TrimPrefix(server.URL, "http://")
			output, err := run(t, bare, key, records(1))
			if err == nil || httpRequests.Load() != 0 || !strings.Contains(output, "HTTPS was used") || !strings.Contains(output, "no automatic fallback") {
				t.Fatalf("HTTP requests=%d err=%v\n%s", httpRequests.Load(), err, output)
			}
		})
	}
}
